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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first architect intent: %v", err)
	}

	_, err = store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-2",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1201",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
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
		ContextID:    "2026-05-19-1202",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create architect intent after delete: %v", err)
	}
}

func TestSessionStoreAllowsOneTicketIntentPerTicket(t *testing.T) {
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
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      "/tmp/repo",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first ticket intent: %v", err)
	}

	_, err = store.CreateIntent(context.Background(), domain.CreateSessionIntentParams{
		ID:           "intent-2",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      "/tmp/repo",
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second ticket intent on same ticket, got %v", err)
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
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

func TestSessionStorePersistsFreeformIntentFields(t *testing.T) {
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
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-19-1200-investigate-login-failure",
		Prompt:       "Investigate login failure and report root cause",
		Workdir:      "/tmp/service-a",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create freeform intent: %v", err)
	}

	intent, err := store.GetIntent(context.Background(), "intent-1")
	if err != nil {
		t.Fatalf("get freeform intent: %v", err)
	}
	if intent.SessionType != domain.SessionTypeFreeform || intent.ContextID != "2026-05-19-1200-investigate-login-failure" {
		t.Fatalf("unexpected intent identity %#v", intent)
	}
	if intent.Prompt != "Investigate login failure and report root cause" || intent.Workdir != "/tmp/service-a" {
		t.Fatalf("unexpected persisted freeform fields %#v", intent)
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
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

func TestSessionStoreAgentStatusOnCreate(t *testing.T) {
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	startedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	run, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.AgentStatus != "" {
		t.Fatalf("expected empty agent status on create, got %q", run.AgentStatus)
	}
}

func TestSessionStoreUpdateRunAgentStatus(t *testing.T) {
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := store.UpdateRunAgentStatus(context.Background(), "run-1", domain.AgentStatusActive); err != nil {
		t.Fatalf("update agent status: %v", err)
	}

	run, err := store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.AgentStatus != domain.AgentStatusActive {
		t.Fatalf("expected agent status %q, got %q", domain.AgentStatusActive, run.AgentStatus)
	}

	if err := store.UpdateRunAgentStatus(context.Background(), "run-1", domain.AgentStatusWaiting); err != nil {
		t.Fatalf("update agent status again: %v", err)
	}
	run, err = store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get run after second update: %v", err)
	}
	if run.AgentStatus != domain.AgentStatusWaiting {
		t.Fatalf("expected agent status %q, got %q", domain.AgentStatusWaiting, run.AgentStatus)
	}
}

func TestSessionStoreMarkRunCompletedSetsAgentStatusStopped(t *testing.T) {
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := store.MarkRunCompleted(context.Background(), "run-1"); err != nil {
		t.Fatalf("mark run completed: %v", err)
	}

	run, err := store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.AgentStatus != domain.AgentStatusStopped {
		t.Fatalf("expected agent status %q on completed run, got %q", domain.AgentStatusStopped, run.AgentStatus)
	}
	if run.Status != domain.SessionRunStatusCompleted {
		t.Fatalf("expected run status %q, got %q", domain.SessionRunStatusCompleted, run.Status)
	}
}

func TestSessionStoreMarkRunFailedSetsAgentStatusStopped(t *testing.T) {
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
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionIntentID: "intent-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := store.MarkRunFailed(context.Background(), "run-1", domain.SessionRunFailureProcessExited); err != nil {
		t.Fatalf("mark run failed: %v", err)
	}

	run, err := store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.AgentStatus != domain.AgentStatusStopped {
		t.Fatalf("expected agent status %q on failed run, got %q", domain.AgentStatusStopped, run.AgentStatus)
	}
	if run.Status != domain.SessionRunStatusFailed {
		t.Fatalf("expected run status %q, got %q", domain.SessionRunStatusFailed, run.Status)
	}
}
