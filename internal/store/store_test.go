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

	for _, table := range []string{"schema_migrations", "sessions", "session_runs", "session_events"} {
		var name string
		if err := db.QueryRowContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
		if name != table {
			t.Fatalf("expected table %q, got %q", table, name)
		}
	}
}

func TestSessionStoreAllowsOneArchitectSessionPerArchitect(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first architect session: %v", err)
	}

	_, err = store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-2",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1201",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second architect session, got %v", err)
	}

	if err := store.DeleteSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("delete architect session: %v", err)
	}

	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-3",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1202",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create architect session after delete: %v", err)
	}
}

func TestSessionStoreDeleteRunErasesRun(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:          "run-1",
		SessionID:   "session-1",
		ProfileName: "codex",
		Workdir:     "/tmp/architect",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := store.DeleteRun(context.Background(), "run-1"); err != nil {
		t.Fatalf("delete run: %v", err)
	}

	_, err = store.GetRun(context.Background(), "run-1")
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected deleted run to be missing, got %v", err)
	}
	current, err := store.GetCurrentRun(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("get current run: %v", err)
	}
	if current != nil {
		t.Fatalf("expected no current run, got %#v", current)
	}
}

func TestSessionStoreAllowsOneTicketSessionPerTicket(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      "/tmp/repo",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first ticket session: %v", err)
	}

	_, err = store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-2",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      "/tmp/repo",
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second ticket session on same ticket, got %v", err)
	}
}

func TestSessionStoreAllowsTicketSessionAfterFailedRun(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-failed",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      "/tmp/repo",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create first ticket session: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-failed",
		SessionID:       "session-failed",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create failed run: %v", err)
	}
	if err := store.MarkRunFailed(context.Background(), "run-failed", domain.SessionRunFailureRestoreFailed); err != nil {
		t.Fatalf("mark run failed: %v", err)
	}

	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-retry",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "retry",
		Workdir:      "/tmp/repo",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create replacement ticket session: %v", err)
	}

	_, err = store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-stale-retry",
		SessionID:       "session-failed",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       time.Now().UTC(),
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected stale session retry to conflict with replacement session, got %v", err)
	}
}

func TestSessionStoreAllowsOneRunningRunPerSession(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	startedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionID:       "session-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex", Args: []string{"chat"}, Env: map[string]string{"CODEX_HOME": "/tmp/codex"}},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt,
	}); err != nil {
		t.Fatalf("create first run: %v", err)
	}

	_, err = store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-2",
		SessionID:       "session-1",
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
		SessionID:       "session-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("create run after failed run: %v", err)
	}
}

func TestSessionStorePersistsFreeformSessionFields(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-19-1200-investigate-login-failure",
		Prompt:       "Investigate login failure and report root cause",
		Workdir:      "/tmp/service-a",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create freeform session: %v", err)
	}

	session, err := store.GetSession(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("get freeform session: %v", err)
	}
	if session.SessionType != domain.SessionTypeFreeform || session.ContextID != "2026-05-19-1200-investigate-login-failure" {
		t.Fatalf("unexpected session identity %#v", session)
	}
	if session.Prompt != "Investigate login failure and report root cause" || session.Workdir != "/tmp/service-a" {
		t.Fatalf("unexpected persisted freeform fields %#v", session)
	}
}

func TestSessionStorePersistsMCPServerSnapshot(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:          "run-1",
		SessionID:   "session-1",
		ProfileName: "claude-sonnet-plan",
		ProfileSnapshot: domain.AgentProfileSnapshot{
			Agent: "claude",
			MCP: map[string]domain.MCPServerSnapshot{
				"sentrux": {Command: "sentrux", Args: []string{"--mcp"}, Env: map[string]string{"E": "1"}},
			},
		},
		Workdir:   "/tmp/repo",
		StartedAt: time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	run, err := store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.ProfileSnapshot == nil {
		t.Fatal("expected profile snapshot to persist")
	}
	server, ok := run.ProfileSnapshot.MCP["sentrux"]
	if !ok {
		t.Fatalf("expected sentrux mcp in snapshot, got %#v", run.ProfileSnapshot.MCP)
	}
	if server.Command != "sentrux" || len(server.Args) != 1 || server.Args[0] != "--mcp" || server.Env["E"] != "1" {
		t.Fatalf("unexpected persisted mcp server %#v", server)
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
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionID:       "session-1",
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
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	startedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-old",
		SessionID:       "session-1",
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
		SessionID:       "session-1",
		ProfileName:     "codex-work",
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"},
		Workdir:         "/tmp/repo",
		StartedAt:       startedAt,
	}); err != nil {
		t.Fatalf("create new run: %v", err)
	}

	run, err := store.GetCurrentRun(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("get current run: %v", err)
	}
	if run == nil || run.ID != "run-new" || run.Status != domain.SessionRunStatusRunning {
		t.Fatalf("expected current run to be the running retry, got %#v", run)
	}

	session, err := store.GetSession(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.CurrentRun == nil || session.CurrentRun.ID != "run-new" {
		t.Fatalf("expected session current run to prefer running retry, got %#v", session.CurrentRun)
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
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	startedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	run, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionID:       "session-1",
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
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionID:       "session-1",
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
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionID:       "session-1",
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
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-19-1200",
		Prompt:       "kickoff",
		Workdir:      "/tmp/architect",
		CreatedBy:    domain.SessionCreatedByDesktop,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID:              "run-1",
		SessionID:       "session-1",
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
