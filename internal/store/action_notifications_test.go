package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestNotificationObligationMigrationAndEligibility(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	runs := NewActionRunStore(db)
	now := time.Now().UTC()
	legacy := domain.ActionRun{ID: "legacy", Action: "legacy", Trigger: domain.ActionRunTriggerArchitect, ArchitectKey: "alpha", Status: domain.ActionRunCompleted, CreatedAt: now, StartedAt: &now, EndedAt: &now}
	if err := runs.CreateActionRun(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	// Restore the actual pre-migration schema, retaining historical data.
	for _, query := range []string{`ALTER TABLE action_runs DROP COLUMN notification_pending`, `DELETE FROM schema_migrations WHERE version = 13`} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	runs = NewActionRunStore(db)
	for _, kind := range []domain.ActionRunTrigger{domain.ActionRunTriggerManual, domain.ActionRunTriggerArchitect, domain.ActionRunTriggerWorker} {
		run := legacy
		run.ID, run.Action, run.Trigger, run.Status, run.EndedAt = string(kind), string(kind), kind, domain.ActionRunRunning, nil
		if kind == domain.ActionRunTriggerWorker {
			run.RequesterSessionID = "worker"
		}
		if err := runs.CreateActionRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := runs.FinishActionRun(ctx, run.ID, domain.ActionRunFailed, "", "test failure", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []domain.ActionRunStatus{domain.ActionRunDenied, domain.ActionRunFailed} {
		run := legacy
		run.ID, run.Action, run.Status, run.StartedAt, run.EndedAt = string(status), string(status), domain.ActionRunPendingApproval, nil, nil
		if err := runs.CreateActionRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := runs.EndPendingActionRun(ctx, run.ID, status, "", "never started", now); err != nil {
			t.Fatal(err)
		}
	}
	owed, err := runs.PendingActionNotifications(ctx)
	if err != nil || len(owed) != 2 {
		t.Fatalf("obligations: %+v %v", owed, err)
	}
	for _, run := range owed {
		if run.ID != "architect" && run.ID != "worker" {
			t.Fatalf("historical/manual/never-run notice: %+v", run)
		}
	}
	// A crash after creating the outbox row but before clearing its obligation
	// must not allow pruning to erase the dedupe identity.
	messages := NewAgentMessageStore(db)
	notice := domain.AgentMessage{ID: "notice", Kind: domain.AgentMessageActionEnded, Recipient: "architect:alpha", ArchitectKey: "alpha", DedupeKey: "action_ended:architect", Body: "done", State: domain.AgentMessageAppeared, CreatedAt: now, UpdatedAt: now, EndedAt: &now}
	if err := messages.CreateAgentMessage(ctx, notice); err != nil {
		t.Fatal(err)
	}
	if n, err := messages.PruneAgentMessages(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("pruned outstanding obligation: %d %v", n, err)
	}
	if err := runs.ClearActionNotification(ctx, "architect"); err != nil {
		t.Fatal(err)
	}
	if n, err := messages.PruneAgentMessages(ctx, now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune transferred notice: %d %v", n, err)
	}

}
