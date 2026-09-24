package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func openDeferredIntentStore(t *testing.T) *DeferredIntentStore {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewDeferredIntentStore(db)
}

func deferredIntentFixture(id string, status domain.DeferredIntentStatus, created time.Time) domain.DeferredIntent {
	return domain.DeferredIntent{
		ID:        id,
		Type:      "runAction",
		Summary:   "run " + id,
		Payload:   map[string]any{"name": "triage"},
		Origin:    domain.IntentOrigin{ArchitectKey: "hiveryn", SessionID: "session-1", SessionType: domain.SessionTypeArchitect},
		Status:    status,
		CreatedAt: created,
	}
}

func TestDeferredIntentStoreRoundTripsAndTransitions(t *testing.T) {
	ctx := context.Background()
	s := openDeferredIntentStore(t)
	created := time.Date(2026, 9, 24, 8, 0, 0, 123, time.UTC)

	if err := s.CreateDeferredIntent(ctx, deferredIntentFixture("a", domain.DeferredIntentPendingApproval, created)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeferredIntent(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.DeferredIntentPendingApproval || !got.CreatedAt.Equal(created) || got.Payload["name"] != "triage" ||
		got.Origin.SessionID != "session-1" || got.ApprovedAt != nil || got.EndedAt != nil || got.Inputs != nil || got.Result != nil {
		t.Fatalf("round trip = %+v", got)
	}

	approved := created.Add(time.Minute)
	running := got
	running.Status = domain.DeferredIntentRunning
	running.Inputs = domain.IntentInputValues{"variant": "codex", "notify": true}
	running.ApprovedAt = &approved
	if err := s.TransitionDeferredIntent(ctx, domain.DeferredIntentPendingApproval, running); err != nil {
		t.Fatal(err)
	}

	// Compare-and-set: a transition from a status the record is not in
	// conflicts and changes nothing.
	denied := got
	denied.Status = domain.DeferredIntentDenied
	err = s.TransitionDeferredIntent(ctx, domain.DeferredIntentPendingApproval, denied)
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale transition err = %v, want ConflictError", err)
	}

	ended := approved.Add(time.Minute)
	completed := running
	completed.Status = domain.DeferredIntentCompleted
	completed.Result = map[string]any{"ticket_id": "t-1"}
	completed.EndedAt = &ended
	if err := s.TransitionDeferredIntent(ctx, domain.DeferredIntentRunning, completed); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetDeferredIntent(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.DeferredIntentCompleted || got.Inputs["variant"] != "codex" || got.Inputs["notify"] != true ||
		got.Result.(map[string]any)["ticket_id"] != "t-1" || !got.ApprovedAt.Equal(approved) || !got.EndedAt.Equal(ended) {
		t.Fatalf("completed = %+v", got)
	}

	if _, err := s.GetDeferredIntent(ctx, "missing"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("missing err = %v, want NotFound", err)
	}
	if err := s.TransitionDeferredIntent(ctx, domain.DeferredIntentPendingApproval, deferredIntentFixture("missing", domain.DeferredIntentDenied, created)); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("missing transition err = %v, want NotFound", err)
	}
}

func TestDeferredIntentStoreFailsOpenRecordsAndPrunesResolved(t *testing.T) {
	ctx := context.Background()
	s := openDeferredIntentStore(t)
	now := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	oldEnd := old.Add(time.Hour)

	oldDone := deferredIntentFixture("old-done", domain.DeferredIntentCompleted, old)
	oldDone.EndedAt = &oldEnd
	recentDone := deferredIntentFixture("recent-done", domain.DeferredIntentDenied, now)
	recentDone.EndedAt = &now
	for _, in := range []domain.DeferredIntent{
		deferredIntentFixture("pending", domain.DeferredIntentPendingApproval, old),
		deferredIntentFixture("running", domain.DeferredIntentRunning, old),
		oldDone,
		recentDone,
	} {
		if err := s.CreateDeferredIntent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	// Open records are never pruned, however old.
	cutoff := now.Add(-30 * 24 * time.Hour)
	if n, err := s.PruneDeferredIntents(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want only the old resolved record", n, err)
	}
	if _, err := s.GetDeferredIntent(ctx, "old-done"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("old-done err = %v, want pruned", err)
	}

	n, err := s.FailOpenDeferredIntents(ctx, "pending reason", "running reason", now)
	if err != nil || n != 2 {
		t.Fatalf("fail open = %d, %v; want 2", n, err)
	}
	for id, reason := range map[string]string{"pending": "pending reason", "running": "running reason"} {
		got, err := s.GetDeferredIntent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.DeferredIntentFailed || got.Error != reason || got.EndedAt == nil || !got.EndedAt.Equal(now) {
			t.Fatalf("%s = %+v, want failed with %q", id, got, reason)
		}
	}
	if got, _ := s.GetDeferredIntent(ctx, "recent-done"); got.Status != domain.DeferredIntentDenied {
		t.Fatalf("a resolved record must not be touched, got %+v", got)
	}
}
