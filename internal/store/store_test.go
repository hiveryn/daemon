package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestOpenRunsSessionMigrations(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, table := range []string{"schema_migrations", "session_intents", "session_runs", "session_events"} {
		var name string
		if err := db.QueryRowContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
		if name != table {
			t.Fatalf("expected table %q, got %q", table, name)
		}
	}
}

func TestSessionStoreAllowsOneArchitectIntentPerArchitect(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first architect intent: %v", err)
	}

	_, err = store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-2",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second architect intent, got %v", err)
	}

	if err := store.DeleteIntent(context.Background(), "intent-1"); err != nil {
		t.Fatalf("delete architect intent: %v", err)
	}

	if _, err := store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-3",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create architect intent after delete: %v", err)
	}
}

func TestSessionStoreAllowsOneWorkIntentPerTicket(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeWork,
		TicketID:     "ticket-1",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first work intent: %v", err)
	}

	_, err = store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-2",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeWork,
		TicketID:     "ticket-1",
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second work intent on same ticket, got %v", err)
	}
}

func TestSessionStoreAllowsOneRunningRunPerIntent(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	startedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex", Args: []string{"chat"}, Env: map[string]string{"CODEX_HOME": "/tmp/codex"}},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt,
	}); err != nil {
		t.Fatalf("create first run: %v", err)
	}

	_, err = store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-2",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt.Add(time.Minute),
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second running run, got %v", err)
	}

	if err := store.MarkRunFailed(context.Background(), "run-1", domain.SessionRunFailureLaunchFailed); err != nil {
		t.Fatalf("mark run failed: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-3",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("create run after failed run: %v", err)
	}
}

func TestSessionStoreUpdatesRunNativeID(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionIntentID: "intent-1",
		ProfileName:     "opencode",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "opencode"},
		Workdir:         "/tmp/repo",
		NativeID:        "native-1",
		StartedAt:       time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := store.UpdateRunNativeID(context.Background(), "run-1", "native-2"); err != nil {
		t.Fatalf("update run native id: %v", err)
	}

	run, err := store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.NativeID != "native-2" {
		t.Fatalf("expected updated native id, got %q", run.NativeID)
	}
}

func TestSessionStorePrefersRunningCurrentRun(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	startedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-old",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt,
	}); err != nil {
		t.Fatalf("create old run: %v", err)
	}
	if err := store.MarkRunFailed(context.Background(), "run-old", domain.SessionRunFailureLaunchFailed); err != nil {
		t.Fatalf("mark old run failed: %v", err)
	}
	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-new",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt,
	}); err != nil {
		t.Fatalf("create new run: %v", err)
	}

	run, err := store.GetCurrentRun(context.Background(), "intent-1")
	if err != nil {
		t.Fatalf("get current run: %v", err)
	}
	if run == nil || run.ID != "run-new" || run.Status != domain.SessionRunStatusRunning {
		t.Fatalf("expected current run to be the running retry, got %#v", run)
	}

	intent, err := store.GetIntent(context.Background(), "intent-1")
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if intent.CurrentRun == nil || intent.CurrentRun.ID != "run-new" {
		t.Fatalf("expected intent current run to prefer running retry, got %#v", intent.CurrentRun)
	}
}
