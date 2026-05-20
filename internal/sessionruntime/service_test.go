package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	aropencode "github.com/hiveryn/agentruntime/adapter/opencode"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

func TestCreateRunMarksRunFailedWhenTerminalStartFails(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		Prompt:       "kickoff",
		Instructions: "system",
	}
	terminal := &fakeTerminalManager{startErr: errors.New("terminal unavailable")}
	adapter := &fakeAdapter{}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfig(t),
		repo:   repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	_, err := service.CreateRun(context.Background(), "intent-1", domain.CreateSessionRunRequest{
		ProfileName: "codex",
		Cols:        132,
		Rows:        48,
	})
	if err == nil {
		t.Fatal("expected terminal start error")
	}

	if repo.createdRun.ID == "" {
		t.Fatal("expected run to be reserved before terminal start")
	}
	if terminal.firstStartSpec().SessionID != "intent-1" {
		t.Fatalf("expected terminal to start on intent-1, got %#v", terminal.firstStartSpec())
	}
	if terminal.firstStartSpec().Size.Cols != 132 || terminal.firstStartSpec().Size.Rows != 48 {
		t.Fatalf("expected requested dimensions, got %#v", terminal.firstStartSpec().Size)
	}
	if repo.failedRunID != repo.createdRun.ID || repo.failedRunReason != domain.SessionRunFailureLaunchFailed {
		t.Fatalf("expected failed run state, got run=%q reason=%q", repo.failedRunID, repo.failedRunReason)
	}
	if adapter.ensureRequest.Marker != setupMarker {
		t.Fatalf("expected setup marker %q, got %q", setupMarker, adapter.ensureRequest.Marker)
	}
}

func TestRestoreRunningSessionsMarksRestoreFailure(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedIntents = []domain.SessionIntent{{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfig(t),
		repo:   repo,
	}

	err := service.RestoreRunningSessions(context.Background())
	if err == nil {
		t.Fatal("expected restore failure")
	}
	if !strings.Contains(err.Error(), "intent-1") {
		t.Fatalf("expected restore error to mention intent, got %v", err)
	}
	if repo.failedRunID != "run-1" || repo.failedRunReason != domain.SessionRunFailureRestoreFailed {
		t.Fatalf("expected restore failure to mark run failed, got run=%q reason=%q", repo.failedRunID, repo.failedRunReason)
	}
}

func TestConcludeArchitectSessionAppendsEndedEventRawBody(t *testing.T) {
	t.Parallel()

	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-architect",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CreatedAt:    time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC),
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfig(t),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "intent-architect", domain.ConcludeSessionParams{Body: "architect conclusion"})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	if event.RunID != "run-1" {
		t.Fatalf("expected run id in ended event, got %#v", event)
	}
	if event.Raw["body"] != "architect conclusion" || event.Raw["lifecycle"] != "concluded" {
		t.Fatalf("unexpected ended event raw %#v", event.Raw)
	}
	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}
}

func TestConcludeArchitectSessionConflictsWhenWorkerRunsStillRunning(t *testing.T) {
	t.Parallel()

	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-architect",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun: &domain.SessionRun{
			ID:     "run-1",
			Status: domain.SessionRunStatusRunning,
		},
	}
	repo.listedIntents = []domain.SessionIntent{
		repo.createdIntent,
		{ID: "intent-work-2", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeWork, CurrentRun: &domain.SessionRun{ID: "run-2", Status: domain.SessionRunStatusRunning}},
		{ID: "intent-work-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeWork, CurrentRun: &domain.SessionRun{ID: "run-3", Status: domain.SessionRunStatusRunning}},
		{ID: "intent-other", ArchitectKey: "other", SessionType: domain.SessionTypeWork, CurrentRun: &domain.SessionRun{ID: "run-4", Status: domain.SessionRunStatusRunning}},
	}
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfig(t),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "intent-architect", domain.ConcludeSessionParams{Body: "architect conclusion"})
	if err == nil {
		t.Fatal("expected conflict error")
	}

	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if got, want := conflict.Message, "Cannot conclude architect session: 2 worker session(s) still active: intent-work-1, intent-work-2"; got != want {
		t.Fatalf("expected conflict message %q, got %q", want, got)
	}
	if len(operations) != 0 {
		t.Fatalf("expected no conclude operations, got %#v", operations)
	}
}

func TestConcludeWorkSessionAppendsEndedEventRawConclusionData(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	daemonRepoPath := t.TempDir()
	desktopRepoPath := t.TempDir()
	daemonCommit := createTestGitCommit(t, daemonRepoPath)
	desktopCommit := createTestGitCommit(t, desktopRepoPath)
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	started := created.Add(5 * time.Minute)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-work",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeWork,
		TicketID:     "ticket-1",
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			Workdir:     daemonRepoPath,
			StartedAt:   &started,
			ProfileName: "codex",
		},
	}
	cfg := testRuntimeConfigWithPaths(architectPath, daemonRepoPath)
	cfg.Architects["hiveryn"] = config.ArchitectConfig{Path: architectPath, Group: "personal", Repos: map[string]string{"daemon": daemonRepoPath, "desktop": desktopRepoPath}}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    cfg,
		repo:   repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{
			TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusProgress, Created: &created, Updated: &created},
			Body:          "body",
		}},
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "intent-work", domain.ConcludeSessionParams{
		Body:            "worker conclusion",
		Commits:         []domain.CommitRef{{SHA: daemonCommit, Repo: "daemon"}, {SHA: desktopCommit, Repo: "desktop"}},
		Rejected:        true,
		RejectionReason: "needs another pass",
	})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	commits, ok := event.Raw["commits"].([]domain.CommitRef)
	if !ok || len(commits) != 2 {
		t.Fatalf("expected commits in raw payload, got %#v", event.Raw["commits"])
	}
	if event.Raw["rejected"] != true || event.Raw["rejection_reason"] != "needs another pass" {
		t.Fatalf("unexpected event raw %#v", event.Raw)
	}
	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}
}

func TestCreateIntentReloadsArchitectsFile(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	hiverynPath := t.TempDir()
	lithoPath := t.TempDir()
	cfgPath := writeRuntimeConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Group: "personal",
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
	})
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewArchitectsReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}

	repo := newFakeSessionRepository()
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          cfg,
		configSource: source,
		repo:         repo,
		tickets:      &fakeTicketService{},
	}

	_, err = service.CreateIntent(context.Background(), domain.CreateSessionIntentRequest{
		ArchitectKey: "litho",
		SessionType:  domain.SessionTypeArchitect,
	})
	if err == nil {
		t.Fatal("expected missing architect before reload")
	}
	if _, ok := err.(*domain.NotFoundError); !ok {
		t.Fatalf("expected not found error, got %T %v", err, err)
	}

	writeRuntimeYAML(t, filepath.Join(configDir, "architects.yaml"), map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Group: "personal",
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
		"litho": {
			Path:  lithoPath,
			Group: "personal",
			Repos: map[string]string{"app": "/tmp/lithoapp"},
		},
	})

	intent, err := service.CreateIntent(context.Background(), domain.CreateSessionIntentRequest{
		ArchitectKey: "litho",
		SessionType:  domain.SessionTypeArchitect,
	})
	if err != nil {
		t.Fatalf("CreateIntent after reload failed: %v", err)
	}
	if intent.ArchitectKey != "litho" || intent.SessionType != domain.SessionTypeArchitect {
		t.Fatalf("unexpected intent after reload: %#v", intent)
	}
}

func TestCreateTerminalUsesCurrentRunWorkdirAndConfiguredShell(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeWork,
		CurrentRun: &domain.SessionRun{
			ID:      "run-1",
			Status:  domain.SessionRunStatusRunning,
			Workdir: repoPath,
		},
	}
	terminal := &fakeTerminalManager{}
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:            config.Config{Shell: "/bin/zsh"},
		repo:           repo,
		terminal:       terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}

	info, err := service.CreateTerminal(context.Background(), "intent-1", domain.CreateTerminalParams{})
	if err != nil {
		t.Fatalf("CreateTerminal failed: %v", err)
	}
	if info.Command != "/bin/zsh" {
		t.Fatalf("expected configured shell, got %#v", info)
	}
	if got := terminal.firstStartSpec(); got.Command != "/bin/zsh" || got.Workdir != repoPath {
		t.Fatalf("unexpected terminal start spec %#v", got)
	}
}

func TestKillTerminalRejectsMainTerminalID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{ID: "intent-1"}
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{"intent-1": {mainTerminalID: "term-main-1"}},
	}

	err := service.KillTerminal(context.Background(), "intent-1", "term-main-1")
	if err == nil {
		t.Fatal("expected main terminal kill to be rejected")
	}
	if vErr, ok := err.(*domain.ValidationError); !ok || vErr.Field != "terminal_id" {
		t.Fatalf("expected validation error, got %T %#v", err, err)
	}
}

func TestListIntentsHydratesMainTerminalID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedIntents = []domain.SessionIntent{{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}}
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminalStates: map[string]sessionTerminalState{"intent-1": {mainTerminalID: "term-main-1"}},
	}

	intents, err := service.ListIntents(context.Background())
	if err != nil {
		t.Fatalf("ListIntents failed: %v", err)
	}
	if len(intents) != 1 || intents[0].CurrentRun == nil || intents[0].CurrentRun.MainTerminalID != "term-main-1" {
		t.Fatalf("expected hydrated main terminal id, got %#v", intents)
	}
}

func TestHandleTerminalExitResumesMainTerminal(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	started := time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC)
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		Instructions: "system prompt",
		CurrentRun: &domain.SessionRun{
			ID:              "run-1",
			ProfileName:     "codex",
			Status:          domain.SessionRunStatusRunning,
			NativeID:        "native-1",
			Workdir:         "/tmp/workdir",
			StartedAt:       &started,
			ProfileSnapshot: &domain.AgentProfileSnapshot{Agent: "codex", Env: map[string]string{"CODEX_HOME": "/custom/codex"}},
		},
	}
	adapter := &fakeAdapter{}
	terminal := &fakeTerminalManager{}
	oldBridgeCancelled := false
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfig(t),
		repo:   repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){"intent-1": func() { oldBridgeCancelled = true }},
		terminalStates: map[string]sessionTerminalState{
			"intent-1": {
				mainTerminalID: "term-main-old",
				tabs: []sessionTabState{{
					tab: domain.SessionTab{Type: "terminal", TerminalID: "term-extra", Command: "yazi", Status: "running"},
				}},
			},
		},
	}

	service.handleTerminalExit(terminalExit{SessionID: "intent-1", TerminalID: "term-main-old", Err: errors.New("process exited")})

	if !oldBridgeCancelled {
		t.Fatal("expected previous receiver bridge to be cancelled")
	}
	startSpec := terminal.firstStartSpec()
	if !adapter.launchRequest.Resume || adapter.launchRequest.ResumeID != "native-1" {
		t.Fatalf("expected resume launch request, got %#v", adapter.launchRequest)
	}
	state := service.terminalStates["intent-1"]
	if state.mainTerminalID != startSpec.TerminalID {
		t.Fatalf("expected terminal state to point at resumed terminal %q, got %#v", startSpec.TerminalID, state)
	}
	if len(state.tabs) != 1 || state.tabs[0].tab.TerminalID != "term-extra" {
		t.Fatalf("expected existing tabs to be preserved, got %#v", state.tabs)
	}
	event := repo.lastAppendedEvent(t)
	if event.Type != "main_terminal_resumed" || event.RunID != "run-1" {
		t.Fatalf("unexpected resumed event %#v", event)
	}
}

func TestHandleReceiverEventUpdatesCurrentRun(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
	}

	service.handleReceiverEvent(agentruntime.Event{
		ID:       "intent-1",
		Status:   agentruntime.StatusWorking,
		NativeID: "native-1",
		Message:  "hello",
		At:       time.Now().UTC(),
	})

	event := repo.lastAppendedEvent(t)
	if event.SessionIntentID != "intent-1" || event.RunID != "run-1" {
		t.Fatalf("unexpected appended event %#v", event)
	}
	if repo.updatedRunNativeID != "run-1" || repo.updatedRunNative != "native-1" {
		t.Fatalf("expected run native id update, got run=%q native=%q", repo.updatedRunNativeID, repo.updatedRunNative)
	}
}

func TestCreateRunAddsMCPServer(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		Prompt:       "kickoff",
		Instructions: "system",
	}
	adapter := &fakeAdapter{}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfig(t),
		repo:   repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		baseURL:        "http://127.0.0.1:4200",
		executablePath: func() (string, error) { return "/tmp/hiverynd", nil },
	}

	if _, err := service.CreateRun(context.Background(), "intent-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	assertMCPServer(t, adapter.launchRequest.MCPServers, domain.SessionTypeArchitect, "intent-1")
}

func TestCreateRunOpenCodeArchitectDefinesNamedAgent(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		Prompt:       "kickoff",
		Instructions: "system prompt",
	}
	adapter := &fakeAdapter{}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfig(t),
		repo:   repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	if _, err := service.CreateRun(context.Background(), "intent-1", domain.CreateSessionRunRequest{ProfileName: "opencode"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	assertOpenCodeArchitectAgentConfig(t, adapter.launchRequest, "hiveryn", "system prompt")
}

func TestCreateRunOpenCodeLaunchSpecOmitNilAgentPermission(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{
		ID:           "intent-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		Prompt:       "kickoff",
		Instructions: "system prompt",
	}
	adapter := &fakePrepareLaunchAdapter{delegate: aropencode.New(aropencode.DefaultOptions())}
	terminal := &fakeTerminalManager{}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfig(t),
		repo:   repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		baseURL:       "http://127.0.0.1:4200",
	}

	if _, err := service.CreateRun(context.Background(), "intent-1", domain.CreateSessionRunRequest{ProfileName: "opencode"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	spec := terminal.firstStartSpec()
	if !hasArgFlag(spec.Args, "--agent") || !hasArgFlag(spec.Args, "--prompt") {
		t.Fatalf("expected opencode args to include --prompt and --agent, got %#v", spec.Args)
	}
	configContent := spec.Env["OPENCODE_CONFIG_CONTENT"]
	if strings.Contains(configContent, `"permission":null`) {
		t.Fatalf("nil agent permission must be omitted, got %s", configContent)
	}
}

func TestCreateRunOpenCodeFailsWhenProfileAlreadySetsAgentFlag(t *testing.T) {
	t.Parallel()

	cfg := testRuntimeConfig(t)
	cfg.Variants["opencode"] = config.VariantConfig{Agent: "opencode", Args: []string{"--agent", "custom"}}
	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{ID: "intent-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect, Prompt: "kickoff", Instructions: "system"}
	adapter := &fakeAdapter{}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    cfg,
		repo:   repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	_, err := service.CreateRun(context.Background(), "intent-1", domain.CreateSessionRunRequest{ProfileName: "opencode"})
	if err == nil {
		t.Fatal("expected architect OpenCode launch to fail when --agent is preset")
	}
	if !strings.Contains(err.Error(), "must not include --agent") {
		t.Fatalf("expected --agent validation error, got %v", err)
	}
	if repo.failedRunReason != domain.SessionRunFailureLaunchFailed {
		t.Fatalf("expected failed run reason launch_failed, got %q", repo.failedRunReason)
	}
}

func TestCreateRunOpenCodeWorkDoesNotDefineNamedAgent(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	repo := newFakeSessionRepository()
	repo.createdIntent = domain.SessionIntent{ID: "intent-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeWork, TicketID: "ticket-1", Prompt: "kickoff"}
	adapter := &fakeAdapter{}
	service := &Service{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfigWithPaths(architectPath, repoPath),
		repo:    repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusBacklog, Created: &created, Updated: &created}, Body: "body"}},
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	if _, err := service.CreateRun(context.Background(), "intent-1", domain.CreateSessionRunRequest{ProfileName: "opencode"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}
	if hasArgFlag(adapter.launchRequest.Args, "--agent") {
		t.Fatalf("worker OpenCode args unexpectedly include --agent: %#v", adapter.launchRequest.Args)
	}
	if len(adapter.launchRequest.OpenCodeAgentConfig) != 0 {
		t.Fatalf("worker OpenCode unexpectedly defined named agent config: %#v", adapter.launchRequest.OpenCodeAgentConfig)
	}
}

func testRuntimeConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Variants: map[string]config.VariantConfig{
			"codex":    {Agent: "codex", Env: map[string]string{"CODEX_HOME": "/custom/codex"}},
			"opencode": {Agent: "opencode"},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {Path: t.TempDir(), Group: "personal", Repos: map[string]string{}},
		},
	}
}

func testRuntimeConfigWithPaths(architectPath, repoPath string) config.Config {
	return config.Config{
		Variants: map[string]config.VariantConfig{
			"codex":    {Agent: "codex", Env: map[string]string{"CODEX_HOME": "/custom/codex"}},
			"opencode": {Agent: "opencode"},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {Path: architectPath, Group: "personal", Repos: map[string]string{"daemon": repoPath}},
		},
	}
}

func writeRuntimeConfigFiles(t *testing.T, configDir string, architects map[string]config.ArchitectConfig) string {
	t.Helper()

	path := filepath.Join(configDir, "config.yaml")
	writeRuntimeYAML(t, path, map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "debug",
	})
	writeRuntimeYAML(t, filepath.Join(configDir, "variants.yaml"), map[string]config.VariantConfig{
		"codex": {Agent: "codex"},
	})
	writeRuntimeYAML(t, filepath.Join(configDir, "architects.yaml"), architects)
	return path
}

func writeRuntimeYAML(t *testing.T, path string, v any) {
	t.Helper()

	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %q: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

type fakeAdapter struct {
	ensureRequest agentruntime.SetupRequest
	ensureErr     error
	launchRequest agentruntime.StartRequest
}

func (fakeAdapter) Agent() agentruntime.AgentKind { return agentruntime.AgentCodex }

func (f *fakeAdapter) PrepareLaunch(_ context.Context, req agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	f.launchRequest = req
	return agentruntime.LaunchSpec{Command: "fake-command", Workdir: "/tmp", Args: append([]string(nil), req.Args...)}, nil
}

func (f *fakeAdapter) EnsureSetup(_ context.Context, req agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	f.ensureRequest = req
	if f.ensureErr != nil {
		return agentruntime.SetupResult{}, f.ensureErr
	}
	return agentruntime.SetupResult{}, nil
}

func (f *fakeAdapter) RemoveSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}

func (fakeAdapter) NormalizeEvent(context.Context, []byte) (*agentruntime.Event, error) {
	return nil, nil
}

type fakePrepareLaunchAdapter struct {
	delegate agentruntime.Adapter
}

func (f *fakePrepareLaunchAdapter) Agent() agentruntime.AgentKind { return f.delegate.Agent() }

func (f *fakePrepareLaunchAdapter) PrepareLaunch(ctx context.Context, req agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	return f.delegate.PrepareLaunch(ctx, req)
}

func (f *fakePrepareLaunchAdapter) EnsureSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}

func (f *fakePrepareLaunchAdapter) RemoveSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}

func (f *fakePrepareLaunchAdapter) NormalizeEvent(context.Context, []byte) (*agentruntime.Event, error) {
	return nil, nil
}

type fakeTerminalManager struct {
	startErr   error
	startSpecs []terminalStartSpec
	terminals  []domain.TerminalInfo
	killed     []string
	operations *[]string
}

func (f *fakeTerminalManager) Start(_ context.Context, spec terminalStartSpec) error {
	f.startSpecs = append(f.startSpecs, spec)
	if f.startErr == nil {
		f.terminals = append(f.terminals, domain.TerminalInfo{TerminalID: spec.TerminalID, SessionID: spec.SessionID, Command: spec.Command, Status: "running"})
	}
	return f.startErr
}

func (f *fakeTerminalManager) firstStartSpec() terminalStartSpec {
	if len(f.startSpecs) > 0 {
		return f.startSpecs[0]
	}
	return terminalStartSpec{}
}

func (f *fakeTerminalManager) Attach(context.Context, string, string) (domain.TerminalAttachment, error) {
	return nil, nil
}

func (f *fakeTerminalManager) Kill(_ context.Context, sessionID, id string) error {
	f.killed = append(f.killed, sessionID+":"+id)
	for i, terminal := range f.terminals {
		if terminal.SessionID == sessionID && terminal.TerminalID == id {
			f.terminals = append(f.terminals[:i], f.terminals[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeTerminalManager) KillBySession(context.Context, string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "kill")
	}
	return nil
}

func (f *fakeTerminalManager) ListBySession(sessionID string) []domain.TerminalInfo {
	terminals := make([]domain.TerminalInfo, 0, len(f.terminals))
	for _, terminal := range f.terminals {
		if terminal.SessionID == sessionID {
			terminals = append(terminals, terminal)
		}
	}
	return terminals
}

func (f *fakeTerminalManager) Shutdown(context.Context) error { return nil }

type fakeSessionRepository struct {
	createdIntent      domain.SessionIntent
	createdRun         domain.SessionRun
	listedIntents      []domain.SessionIntent
	failedRunID        string
	failedRunReason    domain.SessionRunFailureReason
	completedRunID     string
	updatedRunNativeID string
	updatedRunNative   string
	appendedEvents     []domain.AppendSessionEventParams
	operations         *[]string
}

func newFakeSessionRepository() *fakeSessionRepository { return &fakeSessionRepository{} }

func (f *fakeSessionRepository) CreateIntent(_ context.Context, params domain.CreateSessionIntentParams) (domain.SessionIntent, error) {
	f.createdIntent = domain.SessionIntent{ID: params.ID, ArchitectKey: params.ArchitectKey, SessionType: params.SessionType, TicketID: params.TicketID, Prompt: params.Prompt, Instructions: params.Instructions, CreatedBy: params.CreatedBy}
	if f.createdIntent.ID == "" {
		f.createdIntent.ID = "intent-created"
	}
	return f.createdIntent, nil
}

func (f *fakeSessionRepository) GetIntent(context.Context, string) (domain.SessionIntent, error) {
	if f.createdIntent.ID != "" {
		return cloneIntent(f.createdIntent), nil
	}
	if len(f.listedIntents) > 0 {
		return cloneIntent(f.listedIntents[0]), nil
	}
	return domain.SessionIntent{}, &domain.NotFoundError{Resource: "session_intent", ID: "missing"}
}

func (f *fakeSessionRepository) ListIntents(context.Context) ([]domain.SessionIntent, error) {
	if len(f.listedIntents) == 0 {
		if f.createdIntent.ID == "" {
			return nil, nil
		}
		return []domain.SessionIntent{cloneIntent(f.createdIntent)}, nil
	}
	intents := make([]domain.SessionIntent, len(f.listedIntents))
	for i := range f.listedIntents {
		intents[i] = cloneIntent(f.listedIntents[i])
	}
	return intents, nil
}

func (f *fakeSessionRepository) DeleteIntent(context.Context, string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete")
	}
	return nil
}

func (f *fakeSessionRepository) CreateRun(_ context.Context, params domain.CreateSessionRunParams) (domain.SessionRun, error) {
	startedAt := params.StartedAt
	snapshot := params.ProfileSnapshot
	f.createdRun = domain.SessionRun{
		ID:              params.ID,
		SessionIntentID: params.SessionIntentID,
		Status:          domain.SessionRunStatusRunning,
		ProfileName:     params.ProfileName,
		ProfileSnapshot: &snapshot,
		Workdir:         params.Workdir,
		NativeID:        params.NativeID,
		StartedAt:       &startedAt,
	}
	if f.createdRun.ID == "" {
		f.createdRun.ID = "run-created"
	}
	f.createdIntent.CurrentRun = &f.createdRun
	return f.createdRun, nil
}

func (f *fakeSessionRepository) GetRun(context.Context, string) (domain.SessionRun, error) {
	return f.createdRun, nil
}

func (f *fakeSessionRepository) GetCurrentRun(context.Context, string) (*domain.SessionRun, error) {
	if f.createdIntent.CurrentRun == nil {
		return nil, nil
	}
	run := *f.createdIntent.CurrentRun
	return &run, nil
}

func (f *fakeSessionRepository) MarkRunCompleted(_ context.Context, id string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "complete")
	}
	f.completedRunID = id
	if f.createdIntent.CurrentRun != nil && f.createdIntent.CurrentRun.ID == id {
		f.createdIntent.CurrentRun.Status = domain.SessionRunStatusCompleted
	}
	return nil
}

func (f *fakeSessionRepository) MarkRunFailed(_ context.Context, id string, reason domain.SessionRunFailureReason) error {
	f.failedRunID = id
	f.failedRunReason = reason
	if f.createdIntent.CurrentRun != nil && f.createdIntent.CurrentRun.ID == id {
		f.createdIntent.CurrentRun.Status = domain.SessionRunStatusFailed
		f.createdIntent.CurrentRun.FailureReason = reason
	}
	return nil
}

func (f *fakeSessionRepository) UpdateRunNativeID(_ context.Context, id, nativeID string) error {
	f.updatedRunNativeID = id
	f.updatedRunNative = nativeID
	if f.createdIntent.CurrentRun != nil && f.createdIntent.CurrentRun.ID == id {
		f.createdIntent.CurrentRun.NativeID = nativeID
	}
	return nil
}

func (f *fakeSessionRepository) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (f *fakeSessionRepository) AppendSessionEvent(_ context.Context, params domain.AppendSessionEventParams) (domain.SessionEvent, error) {
	if f.operations != nil {
		*f.operations = append(*f.operations, "event")
	}
	f.appendedEvents = append(f.appendedEvents, params)
	return domain.SessionEvent{SessionIntentID: params.SessionIntentID, RunID: params.RunID, Type: params.Type, Status: params.Status, Message: params.Message, Raw: params.Raw, At: params.At}, nil
}

func (f *fakeSessionRepository) lastAppendedEvent(t *testing.T) domain.AppendSessionEventParams {
	t.Helper()
	if len(f.appendedEvents) == 0 {
		t.Fatal("expected appended session event")
	}
	return f.appendedEvents[len(f.appendedEvents)-1]
}

type fakeTicketService struct {
	ticket      domain.Ticket
	err         error
	moveErr     error
	concludeErr error
}

func (f *fakeTicketService) ListTickets(context.Context, string) (domain.TicketBoard, error) {
	return domain.TicketBoard{}, nil
}

func (f *fakeTicketService) GetTicket(context.Context, string, string) (domain.Ticket, error) {
	return f.ticket, f.err
}

func (f *fakeTicketService) CreateTicket(context.Context, string, domain.CreateTicketParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) EditTicket(context.Context, string, string, domain.EditTicketParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) UpdateTicketMetadata(context.Context, string, string, domain.UpdateTicketMetadataParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) DeleteTicket(context.Context, string, string) error { return nil }

func (f *fakeTicketService) MoveTicket(context.Context, string, string, domain.MoveTicketParams) (domain.Ticket, error) {
	return domain.Ticket{}, f.moveErr
}

func (f *fakeTicketService) ConcludeTicket(context.Context, string, string, domain.TicketConclusion) (domain.Ticket, error) {
	return domain.Ticket{}, f.concludeErr
}

func assertOpenCodeArchitectAgentConfig(t *testing.T, req agentruntime.StartRequest, architectKey, systemPrompt string) {
	t.Helper()
	if req.Instructions != "" {
		t.Fatalf("expected architect OpenCode instructions to move into named agent config, got %#v", req)
	}
	if len(req.Args) < 2 || req.Args[0] != "--agent" || req.Args[1] != architectKey {
		t.Fatalf("expected args to start with --agent %s, got %#v", architectKey, req.Args)
	}
	entry, ok := req.OpenCodeAgentConfig[architectKey]
	if !ok {
		t.Fatalf("expected named OpenCode agent config for %q, got %#v", architectKey, req.OpenCodeAgentConfig)
	}
	if entry.Description != "Hiveryn architect" || entry.Mode != "primary" || entry.Prompt != systemPrompt {
		t.Fatalf("unexpected architect agent config %#v", entry)
	}
	if entry.Permission != nil {
		t.Fatalf("expected no permission config, got %#v", entry)
	}
}

func assertMCPServer(t *testing.T, servers []agentruntime.MCPServerConfig, sessionType domain.SessionType, intentID string) {
	t.Helper()
	if len(servers) != 1 {
		t.Fatalf("mcp servers = %#v", servers)
	}
	server := servers[0]
	if server.Name != "hiveryn-daemon" || server.Command != "/tmp/hiverynd" {
		t.Fatalf("unexpected mcp server %#v", server)
	}
	if server.Env["HIVERYN_SESSION_TYPE"] != string(sessionType) || server.Env["HIVERYN_SESSION_ID"] != intentID {
		t.Fatalf("unexpected mcp env %#v", server.Env)
	}
}

func cloneIntent(intent domain.SessionIntent) domain.SessionIntent {
	cloned := intent
	if intent.CurrentRun != nil {
		run := *intent.CurrentRun
		cloned.CurrentRun = &run
	}
	return cloned
}

func createTestGitCommit(t *testing.T, repoPath string) string {
	t.Helper()
	runGit(t, repoPath, "init")
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoPath, "add", "README.md")
	runGit(t, repoPath, "commit", "-m", "initial")
	return runGit(t, repoPath, "rev-parse", "HEAD")
}

func runGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
