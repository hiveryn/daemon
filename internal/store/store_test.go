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

func TestSessionEventRingKeepsIntentAndQuestionEventsOutsideCap(t *testing.T) {
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
	if _, err := store.AppendSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: "session-1", Type: "question", Status: "required",
		Raw: map[string]any{"question_id": "question-1"},
	}); err != nil {
		t.Fatalf("append question event: %v", err)
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
	if len(events) != 102 {
		t.Fatalf("got %d events, want 100 capped ordinary events plus intent and question", len(events))
	}
	foundIntent, foundQuestion := false, false
	for _, event := range events {
		foundIntent = foundIntent || event.Type == "intent"
		foundQuestion = foundQuestion || event.Type == "question"
	}
	if !foundIntent || !foundQuestion {
		t.Fatalf("evicted by ordinary event ring: intent kept %v, question kept %v", foundIntent, foundQuestion)
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

// Removing Actions retires their rows and tables and nothing else: action
// sessions with their runs, events and remote resources, action_ended
// messages, executeAction intents and approval events go; ticket and
// architect sessions, their other events, other messages and other intents
// stay.
func TestRemoveActionsMigrationRetiresOnlyActionRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "daemon.db")
	all := migrationFiles
	migrationFiles = all[:14]
	db, err := Open(ctx, path)
	migrationFiles = all
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO sessions (id, architect_key, session_type, context_id, prompt, workdir) VALUES ('act', '', 'action', 'exec-1', 'p', '/repo'), ('tkt', 'hiveryn', 'ticket', 'T-1', 'p', '/repo'), ('arch', 'hiveryn', 'architect', 'hiveryn', 'p', '/ws')`,
		`INSERT INTO session_runs (id, session_id, status, profile_name, workdir) VALUES ('run-act', 'act', 'completed', 'codex', '/repo'), ('run-tkt', 'tkt', 'running', 'codex', '/repo')`,
		`INSERT INTO session_events (id, session_id, run_id, seq, type, raw, at) VALUES
			('e1', 'act', 'run-act', 1, 'status', '{}', '2026-10-20T08:00:00Z'),
			('e2', 'tkt', 'run-tkt', 1, 'intent', '{"intent_type":"executeAction"}', '2026-10-20T08:00:00Z'),
			('e3', 'tkt', 'run-tkt', 2, 'intent', '{"intent_type":"concludeSession"}', '2026-10-20T08:00:00Z'),
			('e4', 'tkt', 'run-tkt', 3, 'status', 'not json', '2026-10-20T08:00:00Z')`,
		`INSERT INTO remote_resources (session_id, ssh, socket) VALUES ('act', 'box', '/s1'), ('tkt', 'box', '/s2')`,
		`INSERT INTO action_runs (id, action, trigger, status, prompt, profile_name, repo_path, output_dir, created_at) VALUES ('exec-1', 'demo', 'manual', 'completed', 'p', 'codex', '/repo', '/out', '2026-10-20T08:00:00Z')`,
		`INSERT INTO action_artifacts (execution_id, kind, machine, path, state, created_at) VALUES ('exec-1', 'package', '', '/out', 'ready', '2026-10-20T08:00:00Z')`,
		`INSERT INTO agent_messages (id, kind, recipient, dedupe_key, body, state, created_at, updated_at) VALUES
			('m1', 'action_ended', 'architect:hiveryn', 'action_ended:exec-1', 'b', 'acknowledged', '2026-10-20T08:00:00Z', '2026-10-20T08:00:00Z'),
			('m2', 'ticket_concluded', 'architect:hiveryn', 'ticket_concluded:x', 'b', 'acknowledged', '2026-10-20T08:00:00Z', '2026-10-20T08:00:00Z')`,
		`INSERT INTO deferred_intents (id, session_id, intent_type, summary, origin, status, created_at) VALUES ('d1', 'arch', 'executeAction', 's', '{}', 'completed', '2026-10-20T08:00:00Z'), ('d2', 'arch', 'other', 's', '{}', 'completed', '2026-10-20T08:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
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
	for _, table := range []string{"action_runs", "action_artifacts"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("table %s still present (%d, %v)", table, n, err)
		}
	}
	for query, want := range map[string]string{
		`SELECT group_concat(id) FROM (SELECT id FROM sessions ORDER BY id)`:                        "arch,tkt",
		`SELECT group_concat(id) FROM (SELECT id FROM session_runs ORDER BY id)`:                    "run-tkt",
		`SELECT group_concat(id) FROM (SELECT id FROM session_events ORDER BY id)`:                  "e3,e4",
		`SELECT group_concat(session_id) FROM (SELECT session_id FROM remote_resources ORDER BY 1)`: "tkt",
		`SELECT group_concat(id) FROM (SELECT id FROM agent_messages ORDER BY id)`:                  "m2",
		`SELECT group_concat(id) FROM (SELECT id FROM deferred_intents ORDER BY id)`:                "d2",
	} {
		var got string
		if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", query, got, err, want)
		}
	}
}
