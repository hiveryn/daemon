package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
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

	for _, table := range []string{"schema_migrations", "sessions", "session_runs", "session_events", "deferred_intents"} {
		var name string
		if err := db.QueryRowContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
		if name != table {
			t.Fatalf("expected table %q, got %q", table, name)
		}
	}
}

func TestMigrationRemovesLegacyFreeformSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Recreate the pre-removal state: a freeform session with a run and an
	// event, next to a ticket session that must survive, and migration 3 not
	// yet applied.
	for _, stmt := range []string{
		`INSERT INTO sessions (id, architect_key, session_type, context_id, prompt, workdir) VALUES ('ff', 'hiveryn', 'freeform', '2026-05-21-1445-explore', 'explore', '/tmp/ff')`,
		`INSERT INTO session_runs (id, session_id, status, profile_name, workdir) VALUES ('ff-run', 'ff', 'running', 'claude', '/tmp/ff')`,
		`INSERT INTO session_events (id, session_id, run_id, seq, type, at) VALUES ('ff-event', 'ff', 'ff-run', 1, 'status', '2026-05-21T14:45:00Z')`,
		`INSERT INTO sessions (id, architect_key, session_type, context_id, prompt, workdir) VALUES ('tk', 'hiveryn', 'ticket', 'ticket-1', 'kickoff', '/tmp/tk')`,
		`INSERT INTO session_runs (id, session_id, status, profile_name, workdir) VALUES ('tk-run', 'tk', 'running', 'claude', '/tmp/tk')`,
		`DELETE FROM schema_migrations WHERE version = 3`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	_ = db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, check := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM sessions WHERE session_type = 'freeform'`, 0},
		{`SELECT COUNT(*) FROM session_runs WHERE session_id = 'ff'`, 0},
		{`SELECT COUNT(*) FROM session_events WHERE session_id = 'ff'`, 0},
		{`SELECT COUNT(*) FROM sessions WHERE id = 'tk'`, 1},
		{`SELECT COUNT(*) FROM session_runs WHERE session_id = 'tk'`, 1},
	} {
		var got int
		if err := db.QueryRowContext(ctx, check.query).Scan(&got); err != nil {
			t.Fatalf("query %q: %v", check.query, err)
		}
		if got != check.want {
			t.Fatalf("%s = %d, want %d", check.query, got, check.want)
		}
	}
}

func TestSessionEventRingKeepsIntentEventsOutsideCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(ctx, domain.CreateSessionParams{
		ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect,
		ContextID: "2026-08-11-1200", Prompt: "kickoff", Workdir: "/tmp/architect",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AppendSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: "session-1", Type: "intent", Status: "required",
		Raw: map[string]any{"intent_id": "intent-1"},
	}); err != nil {
		t.Fatalf("append intent event: %v", err)
	}
	for i := 0; i < 105; i++ {
		if _, err := store.AppendSessionEvent(ctx, domain.AppendSessionEventParams{
			SessionID: "session-1", Type: "output", Message: "event",
		}); err != nil {
			t.Fatalf("append ordinary event %d: %v", i, err)
		}
	}

	events, err := store.ListSessionEvents(ctx, "session-1")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 101 {
		t.Fatalf("got %d events, want 100 capped ordinary events plus intent", len(events))
	}
	foundIntent := false
	for _, event := range events {
		foundIntent = foundIntent || event.Type == "intent"
	}
	if !foundIntent {
		t.Fatal("intent event was evicted by ordinary event ring")
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

func TestSessionStorePersistsRepositoryScopeSnapshots(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := NewSessionStore(db)
	session, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID: "scope-session", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-scope",
		Prompt: "kickoff", Workdir: "/repos/daemon", AdditionalRepos: []string{"desktop", "shared"}, AdditionalWorkdirs: []string{"/repos/desktop", "/repos/shared"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !slices.Equal(session.AdditionalRepos, []string{"desktop", "shared"}) || !slices.Equal(session.AdditionalWorkdirs, []string{"/repos/desktop", "/repos/shared"}) {
		t.Fatalf("session scope = %#v", session)
	}
	run, err := store.CreateRun(context.Background(), domain.CreateSessionRunParams{
		ID: "scope-run", SessionID: session.ID, ProfileName: "codex", ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"}, Workdir: session.Workdir,
		AdditionalRepos: session.AdditionalRepos, AdditionalWorkdirs: session.AdditionalWorkdirs, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if !slices.Equal(run.AdditionalRepos, session.AdditionalRepos) || !slices.Equal(run.AdditionalWorkdirs, session.AdditionalWorkdirs) {
		t.Fatalf("run scope = %#v", run)
	}
}

func TestSessionStorePersistsWorkflowSelection(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := NewSessionStore(db)

	selected := []string{"/ws/workflows/B.md", "/ws/workflows/A.md"}
	session, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID: "wf-session", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-wf",
		Prompt: "kickoff", Workdir: "/repos/daemon", Workflows: selected,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !slices.Equal(session.Workflows, selected) {
		t.Fatalf("workflows = %v, want %v (order preserved)", session.Workflows, selected)
	}
	loaded, err := store.GetSession(context.Background(), "wf-session")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !slices.Equal(loaded.Workflows, selected) {
		t.Fatalf("reloaded workflows = %v, want %v", loaded.Workflows, selected)
	}

	// No selection is stored and read back as an empty list, never nil.
	none, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID: "wf-none", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect, ContextID: "2026-01-01-0000",
		Prompt: "kickoff", Workdir: "/ws",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if none.Workflows == nil || len(none.Workflows) != 0 {
		t.Fatalf("empty selection = %#v, want []", none.Workflows)
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
