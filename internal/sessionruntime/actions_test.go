package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

const testActionKickoff = "Handle this request:\n\n{{prompt}}\n\nWrite everything to {{output_dir}}.\n"

type actionFixture struct {
	service  *Service
	sessions *store.SessionStore
	runs     *store.ActionRunStore
	adapter  *fakeAdapter
	terminal *fakeTerminalManager
	root     string
	outRoot  string
	dbPath   string
}

func newActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	f := &actionFixture{root: t.TempDir(), outRoot: filepath.Join(t.TempDir(), "action-runs"), dbPath: filepath.Join(t.TempDir(), "daemon.db")}
	f.open(t)
	return f
}

// open (re)builds the service over the fixture's database, as a daemon
// restart would.
func (f *actionFixture) open(t *testing.T) {
	t.Helper()
	db, err := store.Open(context.Background(), f.dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f.sessions = store.NewSessionStore(db)
	f.runs = store.NewActionRunStore(db)
	f.adapter = &fakeAdapter{}
	f.terminal = &fakeTerminalManager{}
	f.service = &Service{
		intents:  newIntentStore(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:      config.Config{Variants: map[string]config.VariantConfig{"codex": {Agent: "codex"}}},
		repo:     f.sessions,
		receiver: ingest.NewReceiver(f.adapter),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: f.adapter,
		},
		terminal:       f.terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
		baseURL:        "http://127.0.0.1:4200",
		executablePath: func() (string, error) { return "/tmp/hiverynd", nil },
	}
	f.service.SetActions(f.runs, f.root, f.outRoot)
	f.service.SetDeferredIntentRepository(store.NewDeferredIntentStore(db))
}

func (f *actionFixture) writeAction(t *testing.T, name, manifest, kickoff string) string {
	t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "action.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if kickoff != "" {
		if err := os.WriteFile(filepath.Join(dir, "KICKOFF.md"), []byte(kickoff), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (f *actionFixture) writeValidAction(t *testing.T, name string) string {
	t.Helper()
	return f.writeAction(t, name, "name: "+name+"\ndescription: Compares things; say how many runs.\nartifacts: summary.md and results.json\n", testActionKickoff)
}

func (f *actionFixture) launch(t *testing.T, name, prompt string) domain.LaunchActionResult {
	t.Helper()
	result, err := f.service.LaunchAction(context.Background(), name, domain.LaunchActionRequest{Prompt: prompt, ProfileName: "codex"})
	if err != nil {
		t.Fatalf("launch %s: %v", name, err)
	}
	return result
}

func TestLaunchActionStartsRunningExecutionInRepoWithEmptyOutputDir(t *testing.T) {
	f := newActionFixture(t)
	repoPath := f.writeValidAction(t, "demo")
	events := f.service.SubscribeActionEvents()
	defer events.Close()

	result := f.launch(t, "demo", "Run AMS and LDN {{output_dir}} three times")
	run := result.Run

	if run.Status != domain.ActionRunRunning || run.Trigger != domain.ActionRunTriggerManual || run.StartedAt == nil || run.EndedAt != nil {
		t.Fatalf("launched run = %+v, want running manual execution", run)
	}
	wantOut := filepath.Join(f.outRoot, "demo", run.ID)
	if run.OutputDir != wantOut || run.RepoPath != repoPath || run.SessionID != result.Session.ID {
		t.Fatalf("run paths = %+v, want output %s repo %s", run, wantOut, repoPath)
	}
	entries, err := os.ReadDir(run.OutputDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("output dir entries = %v, err %v; want empty existing dir", entries, err)
	}
	if strings.HasPrefix(run.OutputDir, repoPath+string(filepath.Separator)) {
		t.Fatalf("output dir %s is inside the action repo", run.OutputDir)
	}

	session := result.Session
	if session.SessionType != domain.SessionTypeAction || session.ArchitectKey != "" || session.ContextID != run.ID {
		t.Fatalf("session = %+v, want architect-free action session keyed by execution", session)
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning || result.MainTerminalID == "" {
		t.Fatalf("session run not live: %+v main=%q", session.CurrentRun, result.MainTerminalID)
	}

	req := f.adapter.launchRequest
	if req.Workdir != repoPath {
		t.Fatalf("agent workdir = %q, want action repo %q", req.Workdir, repoPath)
	}
	if len(req.AdditionalWorkdirs) != 1 || req.AdditionalWorkdirs[0] != run.OutputDir {
		t.Fatalf("agent writable scope = %v, want the output dir", req.AdditionalWorkdirs)
	}
	// The kickoff carries the exact output path and the caller's prompt
	// verbatim: placeholder-looking prompt text is not expanded.
	for _, want := range []string{"Output directory: " + run.OutputDir, "Write everything to " + run.OutputDir, "Run AMS and LDN {{output_dir}} three times", "execution " + run.ID} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("kickoff missing %q:\n%s", want, req.Prompt)
		}
	}
	if !strings.Contains(req.Instructions, "readRecentConclusions") || !strings.Contains(req.Instructions, "concludeSession") {
		t.Fatalf("action instructions do not name the action tools:\n%s", req.Instructions)
	}
	if len(req.MCPServers) != 1 {
		t.Fatalf("mcp servers = %+v", req.MCPServers)
	}
	server := req.MCPServers[0]
	if server.Env["HIVERYN_SESSION_TYPE"] != "action" || server.Env["HIVERYN_SESSION_ID"] != session.ID {
		t.Fatalf("mcp env = %v", server.Env)
	}
	if _, ok := server.Env["HIVERYN_ARCHITECT_KEY"]; ok || strings.Contains(strings.Join(server.Args, " "), "--architect-key") {
		t.Fatalf("action mcp server carries an architect key: %+v", server)
	}

	select {
	case event := <-events.C():
		if event.ExecutionID != run.ID || event.Status != domain.ActionRunRunning || event.Action != "demo" {
			t.Fatalf("event = %+v", event)
		}
	default:
		t.Fatal("no action event published on launch")
	}

	tabs, err := f.service.ListSessionTabs(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) == 0 || tabs[0].Type != "action" {
		t.Fatalf("action session tabs = %+v, want the action tab first", tabs)
	}
	workdirs, err := f.service.ListTerminalWorkdirs(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workdirs) != 2 || workdirs[0].Path != repoPath || !workdirs[0].Default || workdirs[1].Path != run.OutputDir {
		t.Fatalf("terminal workdirs = %+v", workdirs)
	}
}

func TestLaunchActionRejectsBusyActionAndAllowsOthers(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "other")
	first := f.launch(t, "demo", "first")

	_, err := f.service.LaunchAction(context.Background(), "demo", domain.LaunchActionRequest{Prompt: "second", ProfileName: "codex"})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), first.Run.ID) {
		t.Fatalf("second launch err = %v, want busy conflict naming %s", err, first.Run.ID)
	}
	runs, _ := f.service.ListActionRuns(context.Background(), "demo", 0)
	if len(runs) != 1 {
		t.Fatalf("busy launch left a record: %+v", runs)
	}
	f.launch(t, "other", "independent")

	list, err := f.service.ListActions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range list.Actions {
		if def.Name == "demo" && def.RunningExecutionID != first.Run.ID {
			t.Fatalf("demo running id = %q, want %q", def.RunningExecutionID, first.Run.ID)
		}
	}
}

func TestLaunchActionRejectsInvalidDefinitionWithProblems(t *testing.T) {
	f := newActionFixture(t)
	f.writeAction(t, "broken", "name: other\ndescription: x\n", "no placeholders {{nope}}")

	_, err := f.service.LaunchAction(context.Background(), "broken", domain.LaunchActionRequest{Prompt: "go", ProfileName: "codex"})
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("err = %v, want validation error", err)
	}
	for _, want := range []string{"must equal the directory name", "artifacts is required", "{{prompt}}", "{{output_dir}}", "unknown placeholder {{nope}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	if runs, _ := f.service.ListActionRuns(context.Background(), "", 0); len(runs) != 0 {
		t.Fatalf("invalid launch created records: %+v", runs)
	}
}

func TestLaunchActionFailureFailsExecutionAndFreesAction(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.terminal.startErr = errors.New("pty unavailable")

	_, err := f.service.LaunchAction(context.Background(), "demo", domain.LaunchActionRequest{Prompt: "go", ProfileName: "codex"})
	if err == nil {
		t.Fatal("launch succeeded with a failing terminal")
	}
	runs, _ := f.service.ListActionRuns(context.Background(), "demo", 0)
	if len(runs) != 1 || runs[0].Status != domain.ActionRunFailed || !strings.Contains(runs[0].Error, "pty unavailable") || runs[0].EndedAt == nil {
		t.Fatalf("runs after failed launch = %+v", runs)
	}
	if sessions, _ := f.sessions.ListSessions(context.Background()); len(sessions) != 0 {
		t.Fatalf("failed launch left sessions: %+v", sessions)
	}

	f.terminal.startErr = nil
	f.launch(t, "demo", "retry")
}

func TestConcludeActionCompletesAndRetainsHistory(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")

	// completed is refused while nothing was delivered.
	_, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "done"})
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty-output conclusion err = %v", err)
	}
	if _, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: "maybe", Summary: "x"}); err == nil {
		t.Fatal("invalid outcome accepted")
	}
	if _, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: strings.Repeat("x", domain.MaxActionSummaryLength+1)}); err == nil {
		t.Fatal("over-long summary accepted")
	}

	if err := os.WriteFile(filepath.Join(result.Run.OutputDir, "summary.md"), []byte("# synthetic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "AMS 19.7% vs LDN 8.5%; LDN run 2 recovered."})
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	if run.Status != domain.ActionRunCompleted || run.Summary == "" || run.EndedAt == nil || run.OutputDir != result.Run.OutputDir {
		t.Fatalf("concluded run = %+v", run)
	}
	if _, err := f.sessions.GetSession(ctx, result.Session.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("action session still exists after conclusion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run.OutputDir, "summary.md")); err != nil {
		t.Fatalf("artifacts not retained: %v", err)
	}

	// A second execution reads the first one's summary back, and can conclude
	// failed — distinct from a completed run that reports findings.
	second := f.launch(t, "demo", "again")
	history, err := f.service.RecentActionConclusions(ctx, second.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ExecutionID != run.ID || history[0].Status != domain.ActionRunCompleted {
		t.Fatalf("history = %+v", history)
	}
	failed, err := f.conclude(t, second.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "collector missing"})
	if err != nil || failed.Status != domain.ActionRunFailed {
		t.Fatalf("failed conclusion = %+v, %v", failed, err)
	}
	all, _ := f.service.ListActionRuns(ctx, "demo", 0)
	if len(all) != 2 || all[0].ID != second.Run.ID {
		t.Fatalf("history listing = %+v, want newest first", all)
	}
}

func TestRecentActionConclusionsReturnsAtMostFive(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		r := f.launch(t, "demo", "go")
		if _, err := f.conclude(t, r.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "attempt"}); err != nil {
			t.Fatal(err)
		}
	}
	current := f.launch(t, "demo", "go")
	history, err := f.service.RecentActionConclusions(ctx, current.Session.ID)
	if err != nil || len(history) != 5 {
		t.Fatalf("history = %d entries, %v", len(history), err)
	}
}

func TestCancelActionRunFailsExecutionAndEndsSession(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")

	run, err := f.service.CancelActionRun(ctx, result.Run.ID)
	if err != nil || run.Status != domain.ActionRunFailed || run.Error != "cancelled by user" {
		t.Fatalf("cancel = %+v, %v", run, err)
	}
	if _, err := f.sessions.GetSession(ctx, result.Session.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("session survived cancel: %v", err)
	}
	if _, err := f.service.CancelActionRun(ctx, result.Run.ID); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("second cancel err = %v", err)
	}
	f.launch(t, "demo", "free again")
}

func TestRestartKeepsRestoredExecutionRunningAndFailsOrphans(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "orphan")
	ctx := context.Background()
	live := f.launch(t, "demo", "go")
	orphan := f.launch(t, "orphan", "go")
	// The orphan's session disappears while the daemon is down.
	if err := f.sessions.DeleteSession(ctx, orphan.Session.ID); err != nil {
		t.Fatal(err)
	}

	f.open(t)
	if err := f.service.RestoreRunningSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileActionRuns(ctx); err != nil {
		t.Fatal(err)
	}

	restored, _ := f.service.GetActionRun(ctx, live.Run.ID)
	if restored.Status != domain.ActionRunRunning {
		t.Fatalf("restored execution = %+v, want still running", restored)
	}
	if !f.adapter.launchRequest.Resume {
		t.Fatalf("restore did not resume the agent: %+v", f.adapter.launchRequest)
	}
	failed, _ := f.service.GetActionRun(ctx, orphan.Run.ID)
	if failed.Status != domain.ActionRunFailed || !strings.Contains(failed.Error, "interrupted") {
		t.Fatalf("orphan execution = %+v, want failed as interrupted", failed)
	}
	// The restored execution can still conclude with its stable id.
	if err := os.WriteFile(filepath.Join(live.Run.OutputDir, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := f.conclude(t, live.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "ok"})
	if err != nil || done.ID != live.Run.ID || done.Status != domain.ActionRunCompleted {
		t.Fatalf("conclude after restart = %+v, %v", done, err)
	}
}

func TestRestoreFailureFailsActionExecution(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	live := f.launch(t, "demo", "go")

	f.open(t)
	f.terminal.startErr = errors.New("resume broke")
	if err := f.service.RestoreRunningSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileActionRuns(ctx); err != nil {
		t.Fatal(err)
	}
	run, _ := f.service.GetActionRun(ctx, live.Run.ID)
	if run.Status != domain.ActionRunFailed || !strings.Contains(run.Error, "resume broke") {
		t.Fatalf("execution after failed restore = %+v", run)
	}
	if _, err := f.sessions.GetSession(ctx, live.Session.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("failed-restore session kept: %v", err)
	}
}

func TestActionSessionRejectsTicketAndArchitectConclusion(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	result := f.launch(t, "demo", "go")
	if _, err := f.service.ConcludeSession(context.Background(), result.Session.ID, domain.ConcludeSessionParams{Summary: "x"}); err == nil {
		t.Fatal("generic conclusion accepted for an action session")
	}
}

func TestAgentExitResumesOrFailsActionExecution(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")

	// An exited agent is resumed and the execution keeps running.
	f.service.handleTerminalExit(terminalExit{SessionID: result.Session.ID, TerminalID: result.MainTerminalID})
	if run, _ := f.service.GetActionRun(ctx, result.Run.ID); run.Status != domain.ActionRunRunning {
		t.Fatalf("execution after resumed exit = %+v", run)
	}
	if !f.adapter.launchRequest.Resume {
		t.Fatal("agent exit did not resume the agent")
	}

	// A resume that fails fails the execution.
	f.terminal.startErr = errors.New("cannot resume")
	f.service.handleTerminalExit(terminalExit{SessionID: result.Session.ID, TerminalID: "whatever"})
	run, _ := f.service.GetActionRun(ctx, result.Run.ID)
	if run.Status != domain.ActionRunFailed || !strings.Contains(run.Error, "cannot resume") {
		t.Fatalf("execution after failed resume = %+v", run)
	}
}

// Applying a conclusion kills the agent, and with it the MCP call; the
// teardown must still complete when the resolving ctx is cancelled mid-way —
// here the approving request's, and on auto-approval the detached agent's.
func TestConcludeActionSurvivesCallerCancellationDuringTeardown(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	result := f.launch(t, "demo", "go")
	if err := os.WriteFile(filepath.Join(result.Run.OutputDir, "summary.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := f.startConclude(result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "ok"})
	id, _ := f.pendingConclusion(t, result.Session.ID, done)
	ctx, cancel := context.WithCancel(context.Background())
	f.terminal.onKill = cancel
	if _, err := f.service.ApproveIntent(ctx, result.Session.ID, id, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	call := awaitConclude(t, done)
	if call.err != nil || call.res.Result.Status != domain.ActionRunCompleted {
		t.Fatalf("conclude = %+v, %v", call.res, call.err)
	}
	if _, err := f.sessions.GetSession(context.Background(), result.Session.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("session not torn down: %v", err)
	}
}
