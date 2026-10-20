package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func newAgentMessageStore(t *testing.T) *AgentMessageStore {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewAgentMessageStore(db)
}

func TestAgentMessageStoreRoundTripDedupeAndListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newAgentMessageStore(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 123456000, time.UTC)
	m := domain.AgentMessage{
		ID: "m1", Kind: domain.AgentMessageTicketConcluded, Recipient: "architect:hiveryn", ArchitectKey: "hiveryn", DedupeKey: "k1",
		Body: "Ticket T concluded", Payload: map[string]any{"ticket_id": "T"}, State: domain.AgentMessagePendingCommit, Condition: "waiting",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateAgentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	dup := m
	dup.ID = "m2"
	if err := s.CreateAgentMessage(ctx, dup); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("duplicate dedupe key err = %v", err)
	}
	got, err := s.GetAgentMessageByDedupeKey(ctx, "k1")
	if err != nil || got.ID != "m1" || got.Payload["ticket_id"] != "T" || !got.CreatedAt.Equal(now) || got.State != domain.AgentMessagePendingCommit {
		t.Fatalf("by dedupe = %+v %v", got, err)
	}
	if _, err := s.GetAgentMessage(ctx, "nope"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("missing err = %v", err)
	}

	later := now.Add(time.Minute)
	got.State = domain.AgentMessageHanded
	got.Condition = ""
	got.TargetSessionID, got.TargetAgent, got.TargetNativeID, got.TargetConfigRoot, got.TargetWorkdir = "s1", "claude", "n1", "/root", "/work"
	got.Mechanism, got.ProviderRef, got.Attempts, got.LastError = "waiter", "ref", 1, "earlier failure"
	got.CommittedAt, got.HandedAt, got.NextAttemptAt, got.UpdatedAt = &now, &later, &later, later
	if err := s.UpdateAgentMessage(ctx, got); err != nil {
		t.Fatal(err)
	}
	back, err := s.GetAgentMessage(ctx, "m1")
	if err != nil || back.State != domain.AgentMessageHanded || back.TargetSessionID != "s1" || back.TargetConfigRoot != "/root" || back.TargetWorkdir != "/work" ||
		back.ProviderRef != "ref" || back.Attempts != 1 || back.LastError != "earlier failure" || back.HandedAt == nil || !back.HandedAt.Equal(later) ||
		back.CommittedAt == nil || back.NextAttemptAt == nil || back.AppearedAt != nil {
		t.Fatalf("updated = %+v %v", back, err)
	}
	if err := s.UpdateAgentMessage(ctx, domain.AgentMessage{ID: "ghost", UpdatedAt: now}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("update missing err = %v", err)
	}

	other := domain.AgentMessage{ID: "m3", Kind: "note", Recipient: "session:x", ArchitectKey: "other", DedupeKey: "k3", Body: "b", State: domain.AgentMessagePending, CreatedAt: later, UpdatedAt: later}
	ended := domain.AgentMessage{ID: "m4", Kind: "note", Recipient: "architect:hiveryn", ArchitectKey: "hiveryn", DedupeKey: "k4", Body: "b", State: domain.AgentMessageAppeared, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now, EndedAt: ptrTime(now.Add(-47 * time.Hour))}
	for _, x := range []domain.AgentMessage{other, ended} {
		if err := s.CreateAgentMessage(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	open, err := s.ListOpenAgentMessages(ctx)
	if err != nil || len(open) != 2 || open[0].ID != "m1" || open[1].ID != "m3" {
		t.Fatalf("open = %+v %v", open, err)
	}
	mine, err := s.ListAgentMessages(ctx, "hiveryn", 0)
	if err != nil || len(mine) != 2 || mine[0].ID != "m1" || mine[1].ID != "m4" {
		t.Fatalf("hiveryn = %+v %v", mine, err)
	}
	limited, err := s.ListAgentMessages(ctx, "hiveryn", 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limited = %+v %v", limited, err)
	}
	n, err := s.PruneAgentMessages(ctx, now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d %v", n, err)
	}
	if _, err := s.GetAgentMessage(ctx, "m4"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("pruned still present: %v", err)
	}
	if _, err := s.GetAgentMessage(ctx, "m1"); err != nil {
		t.Fatalf("open message pruned: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
