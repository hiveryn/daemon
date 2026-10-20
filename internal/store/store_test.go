package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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

func TestActionRunStoreSingleRunningAndCompareAndSet(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runs := NewActionRunStore(db)

	now := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	mk := func(id, action string) domain.ActionRun {
		return domain.ActionRun{ID: id, Action: action, Trigger: domain.ActionRunTriggerManual, Status: domain.ActionRunRunning,
			Prompt: "p", ProfileName: "codex", RepoPath: "/repo", OutputDir: "/out/" + id, CreatedAt: now, StartedAt: &now}
	}
	if err := runs.CreateActionRun(ctx, mk("a", "demo")); err != nil {
		t.Fatal(err)
	}
	var conflict *domain.ConflictError
	if err := runs.CreateActionRun(ctx, mk("b", "demo")); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "execution a") {
		t.Fatalf("second running execution err = %v", err)
	}
	if err := runs.CreateActionRun(ctx, mk("c", "other")); err != nil {
		t.Fatalf("other action blocked: %v", err)
	}
	if err := runs.SetActionRunSession(ctx, "a", "sess-a"); err != nil {
		t.Fatal(err)
	}

	if err := runs.FinishActionRun(ctx, "a", domain.ActionRunCompleted, "delivered", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := runs.FinishActionRun(ctx, "a", domain.ActionRunFailed, "", "late", now.Add(2*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("finishing a finished execution err = %v", err)
	}
	got, err := runs.GetActionRun(ctx, "a")
	if err != nil || got.Status != domain.ActionRunCompleted || got.Summary != "delivered" || got.SessionID != "sess-a" || got.EndedAt == nil || got.Error != "" {
		t.Fatalf("finished execution = %+v, %v", got, err)
	}
	// The action is free again once its execution finished.
	later := mk("d", "demo")
	later.CreatedAt = now.Add(3 * time.Minute)
	if err := runs.CreateActionRun(ctx, later); err != nil {
		t.Fatalf("relaunch after completion: %v", err)
	}

	list, err := runs.ListActionRuns(ctx, "demo", 0)
	if err != nil || len(list) != 2 || list[0].ID != "d" || list[1].ID != "a" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	running, err := runs.RunningActionRuns(ctx)
	if err != nil || len(running) != 2 || running["demo"].ID != "d" || running["other"].ID != "c" {
		t.Fatalf("running = %+v, %v", running, err)
	}
	conclusions, err := runs.RecentActionConclusions(ctx, "demo", 5)
	if err != nil || len(conclusions) != 1 || conclusions[0].ExecutionID != "a" || conclusions[0].Summary != "delivered" {
		t.Fatalf("conclusions = %+v, %v", conclusions, err)
	}
	if _, err := runs.GetActionRun(ctx, "missing"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("missing err = %v", err)
	}
}

// Executions completed before frozen packages keep their output folder as
// their package, in place and never expiring; nothing else gains one.
func TestActionArtifactMigrationKeepsLegacyPackagesInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "daemon.db")
	all := migrationFiles
	migrationFiles = all[:8]
	db, err := Open(ctx, path)
	migrationFiles = all
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO action_runs (id, action, trigger, status, prompt, profile_name, repo_path, output_dir, created_at, ended_at) VALUES ('done', 'demo', 'manual', 'completed', 'p', 'codex', '/repo', '/home/me/.hiveryn/action-runs/demo/done', '2026-09-24T08:00:00.000000000Z', '2026-09-24T09:00:00.000000000Z')`,
		`INSERT INTO action_runs (id, action, trigger, status, prompt, profile_name, repo_path, output_dir, created_at) VALUES ('failed', 'demo', 'manual', 'failed', 'p', 'codex', '/repo', '/out/failed', '2026-09-24T08:00:00.000000000Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	_ = db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runs := NewActionRunStore(db)
	recs, err := runs.ActionArtifacts(ctx, "done")
	if err != nil || len(recs) != 1 {
		t.Fatalf("legacy package = %+v, %v", recs, err)
	}
	pkg := recs[0]
	if pkg.Kind != domain.ActionArtifactPackage || pkg.Path != "/home/me/.hiveryn/action-runs/demo/done" || pkg.Machine != "" ||
		pkg.State != domain.ActionArtifactsReady || pkg.ManagedRoot != "" || pkg.ExpiresAt != nil || pkg.ReadyAt == nil {
		t.Fatalf("legacy package = %+v", pkg)
	}
	if recs, _ := runs.ActionArtifacts(ctx, "failed"); len(recs) != 0 {
		t.Fatalf("failed execution gained a package: %+v", recs)
	}
	// Never removable: cleanup does not select it however far in the future.
	if clean, err := runs.ActionArtifactsToClean(ctx, time.Now().AddDate(10, 0, 0)); err != nil || len(clean) != 0 {
		t.Fatalf("cleanup selects %+v, %v", clean, err)
	}
	run, err := runs.GetActionRun(ctx, "done")
	if err != nil || run.Machine != "" || run.DeliveryDir != "" {
		t.Fatalf("legacy run = %+v, %v", run, err)
	}
}

func TestCompleteActionRunRecordsItsPackageAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runs := NewActionRunStore(db)
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	run := domain.ActionRun{ID: "r1", Action: "demo", Trigger: domain.ActionRunTriggerManual, Status: domain.ActionRunRunning,
		Prompt: "p", ProfileName: "box-codex", RepoPath: "/srv/demo", OutputDir: "/srv/out", Machine: "box", CreatedAt: now, StartedAt: &now}
	if err := runs.CreateActionRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(30 * 24 * time.Hour)
	pkg := domain.ActionArtifactRecord{ExecutionID: "r1", Kind: domain.ActionArtifactPackage, Machine: "box", SSH: "box", Path: "/home/x/.local/state/hiveryn-actions/artifacts/packages/r1",
		ManagedRoot: "/home/x/.local/state/hiveryn-actions/artifacts", State: domain.ActionArtifactsReady, Files: 3, Bytes: 42, CreatedAt: now, ReadyAt: &now, ExpiresAt: &expires}
	if err := runs.PublishActionConclusion(ctx, "r1", domain.ActionRunCompleted, "done", &domain.ActionArtifactRecord{ExecutionID: "r1", Kind: domain.ActionArtifactPackage, State: domain.ActionArtifactsCopying, Path: "/p"}, now); err == nil {
		t.Fatal("completed with a package that is not ready")
	}
	if err := runs.PublishActionConclusion(ctx, "r1", domain.ActionRunCompleted, "done", &pkg, now); err != nil {
		t.Fatal(err)
	}
	got, _ := runs.GetActionRun(ctx, "r1")
	if got.Status != domain.ActionRunCompleted || got.Machine != "box" || got.Summary != "done" {
		t.Fatalf("run = %+v", got)
	}
	recs, _ := runs.ActionArtifacts(ctx, "r1")
	if len(recs) != 1 || recs[0].Files != 3 || recs[0].ExpiresAt == nil || !recs[0].ExpiresAt.Equal(expires) {
		t.Fatalf("package = %+v", recs)
	}
	// Completing again conflicts and leaves everything as it was.
	if err := runs.PublishActionConclusion(ctx, "r1", domain.ActionRunCompleted, "again", &pkg, now); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("second completion err = %v", err)
	}

	// Cleanup selects copies left copying, failed removals and expired ones.
	copying := domain.ActionArtifactRecord{ExecutionID: "r1", Kind: domain.ActionArtifactDelivery, Machine: "", Path: "/h/.hiveryn/action-artifacts/deliveries/r1",
		ManagedRoot: "/h/.hiveryn/action-artifacts", State: domain.ActionArtifactsCopying, CreatedAt: now, ExpiresAt: &expires}
	if err := runs.PutActionArtifact(ctx, copying); err != nil {
		t.Fatal(err)
	}
	clean, _ := runs.ActionArtifactsToClean(ctx, now)
	if len(clean) != 1 || clean[0].Kind != domain.ActionArtifactDelivery {
		t.Fatalf("before expiry = %+v", clean)
	}
	clean, _ = runs.ActionArtifactsToClean(ctx, expires)
	if len(clean) != 2 {
		t.Fatalf("at expiry = %+v", clean)
	}
	pkg.State = domain.ActionArtifactsExpired
	if err := runs.PutActionArtifact(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if clean, _ = runs.ActionArtifactsToClean(ctx, expires); len(clean) != 1 {
		t.Fatalf("an expired, removed package is selected again: %+v", clean)
	}
}
