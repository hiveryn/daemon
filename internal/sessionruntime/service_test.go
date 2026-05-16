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
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestSpawnArchitectSessionMarksReservedSessionFailedWhenTerminalStartFails(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
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

	_, err := service.SpawnArchitectSession(context.Background(), domain.SpawnArchitectSessionRequest{
		ArchitectKey: "hiveryn",
		ProfileName:  "codex",
		Cols:         132,
		Rows:         48,
	})
	if err == nil {
		t.Fatal("expected terminal start error")
	}

	if repo.createdSession.ID == "" {
		t.Fatal("expected session to be reserved before terminal start")
	}
	if terminal.firstStartSpec().SessionID != repo.createdSession.ID {
		t.Fatalf("expected terminal to start reserved session %q, got %q", repo.createdSession.ID, terminal.firstStartSpec().SessionID)
	}
	if terminal.firstStartSpec().Size.Cols != 132 || terminal.firstStartSpec().Size.Rows != 48 {
		t.Fatalf("expected terminal start size to use requested dimensions, got %#v", terminal.firstStartSpec().Size)
	}
	if repo.updatedStatus != domain.SessionStatusFailed {
		t.Fatalf("expected reserved session to be marked failed, got %q", repo.updatedStatus)
	}
	if adapter.ensureRequest.Marker != setupMarker {
		t.Fatalf("expected setup marker %q, got %q", setupMarker, adapter.ensureRequest.Marker)
	}
	if adapter.ensureRequest.ConfigRoot != "/custom/codex" {
		t.Fatalf("expected codex config root to come from profile env, got %q", adapter.ensureRequest.ConfigRoot)
	}
	if adapter.ensureRequest.Hook.Endpoint == "" {
		t.Fatal("expected hook endpoint to be configured")
	}
}

func TestSpawnArchitectSessionFailsWhenSetupFails(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	adapter := &fakeAdapter{ensureErr: errors.New("setup unavailable")}
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
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	_, err := service.SpawnArchitectSession(context.Background(), domain.SpawnArchitectSessionRequest{
		ArchitectKey: "hiveryn",
		ProfileName:  "codex",
		Cols:         120,
		Rows:         40,
	})
	if err == nil {
		t.Fatal("expected setup error")
	}

	if repo.createdSession.ID == "" {
		t.Fatal("expected session to be reserved before setup failure")
	}
	if adapter.ensureRequest.ConfigRoot != "/custom/codex" {
		t.Fatalf("expected setup to use profile config root, got %q", adapter.ensureRequest.ConfigRoot)
	}
	if repo.updatedStatus != domain.SessionStatusFailed {
		t.Fatalf("expected reserved session to be marked failed, got %q", repo.updatedStatus)
	}
}

func TestConcludeArchitectSessionAppendsEndedEventRawBody(t *testing.T) {
	t.Parallel()

	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "sess-architect",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusRunning,
		CreatedAt:    time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC),
	}
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfig(t),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "sess-architect", domain.ConcludeSessionParams{
		Body: "architect conclusion",
	})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	if event.Raw["body"] != "architect conclusion" {
		t.Fatalf("expected raw body in ended event, got %#v", event.Raw)
	}
	if event.Status != "ended" {
		t.Fatalf("expected ended event, got %#v", event)
	}
	if got, want := strings.Join(operations, ","), "end,event,kill,delete"; got != want {
		t.Fatalf("expected conclude operation order %q, got %q", want, got)
	}
}

func TestConcludeArchitectSessionConflictsWhenWorkerSessionsStillRunning(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "sess-architect",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusRunning,
		CreatedAt:    time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC),
	}
	repo.listedSessions = []domain.Session{
		{
			ID:           "sess-work-2",
			ArchitectKey: "hiveryn",
			SessionType:  string(domain.SessionTypeWork),
			Status:       domain.SessionStatusRunning,
		},
		{
			ID:           "sess-work-1",
			ArchitectKey: "hiveryn",
			SessionType:  string(domain.SessionTypeWork),
			Status:       domain.SessionStatusRunning,
		},
		{
			ID:           "sess-other-architect",
			ArchitectKey: "other",
			SessionType:  string(domain.SessionTypeWork),
			Status:       domain.SessionStatusRunning,
		},
		{
			ID:           "sess-collab",
			ArchitectKey: "hiveryn",
			SessionType:  string(domain.SessionTypeCollab),
			Status:       domain.SessionStatusRunning,
		},
	}
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfigWithPaths(architectPath, t.TempDir()),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "sess-architect", domain.ConcludeSessionParams{
		Body: "architect conclusion",
	})
	if err == nil {
		t.Fatal("expected conflict error")
	}

	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if got, want := conflict.Message, "Cannot conclude architect session: 2 worker session(s) still active: sess-work-1, sess-work-2"; got != want {
		t.Fatalf("expected conflict message %q, got %q", want, got)
	}
	if repo.lastListFilter.Status != domain.SessionStatusRunning {
		t.Fatalf("expected running-session filter, got %#v", repo.lastListFilter)
	}
	if len(operations) != 0 {
		t.Fatalf("expected no conclude operations on conflict, got %#v", operations)
	}
	if _, statErr := os.Stat(filepath.Join(architectPath, "architect-sessions")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected no architect conclusion directory on conflict, got %v", statErr)
	}
}

func TestConcludeWorkSessionAppendsEndedEventRawConclusionData(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	commit := createTestGitCommit(t, repoPath)
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "sess-work",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeWork),
		Status:       domain.SessionStatusRunning,
		TicketID:     "ticket-1",
		CreatedAt:    created,
	}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    testRuntimeConfigWithPaths(architectPath, repoPath),
		repo:   repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusProgress, Created: &created, Updated: &created,
			},
			Body: "body",
		}},
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "sess-work", domain.ConcludeSessionParams{
		Body:            "worker conclusion",
		Commits:         []string{commit},
		Rejected:        true,
		RejectionReason: "needs another pass",
	})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	if event.Raw["body"] != "worker conclusion" {
		t.Fatalf("expected raw body in ended event, got %#v", event.Raw)
	}
	commits, ok := event.Raw["commits"].([]string)
	if !ok || len(commits) != 1 || commits[0] != commit {
		t.Fatalf("expected raw commits in ended event, got %#v", event.Raw["commits"])
	}
	if event.Raw["rejected"] != true {
		t.Fatalf("expected raw rejected in ended event, got %#v", event.Raw)
	}
	if event.Raw["rejection_reason"] != "needs another pass" {
		t.Fatalf("expected raw rejection reason in ended event, got %#v", event.Raw)
	}
	if got, want := strings.Join(operations, ","), "end,event,kill,delete"; got != want {
		t.Fatalf("expected conclude operation order %q, got %q", want, got)
	}
}

func TestCreateTerminalGeneratesUUID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "sess-1",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusRunning,
	}
	terminal := &fakeTerminalManager{}
	cfg := testRuntimeConfig(t)
	cfg.Shell = "/bin/zsh"
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:            cfg,
		repo:           repo,
		terminal:       terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}

	info, err := service.CreateTerminal(context.Background(), "sess-1", domain.CreateTerminalParams{})
	if err != nil {
		t.Fatalf("CreateTerminal failed: %v", err)
	}
	if info.TerminalID == "" {
		t.Fatal("expected generated terminal ID")
	}
	if info.Command != "/bin/zsh" || info.Status != "running" {
		t.Fatalf("unexpected terminal info %#v", info)
	}
	if got := terminal.firstStartSpec(); got.TerminalID != info.TerminalID || got.Command != "/bin/zsh" {
		t.Fatalf("unexpected start spec %#v", got)
	}
	tabs, err := service.ListSessionTabs(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListSessionTabs failed: %v", err)
	}
	if len(tabs) != 1 || tabs[0].TerminalID != info.TerminalID || tabs[0].Command != "/bin/zsh" || tabs[0].Status != "running" {
		t.Fatalf("unexpected tabs after create %#v", tabs)
	}
}

func TestCreateTerminalUsesWorkSessionRepoAndConfiguredShell(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "sess-1",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeWork),
		TicketID:     "ticket-1",
		Status:       domain.SessionStatusRunning,
	}
	terminal := &fakeTerminalManager{}
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:            config.Config{Shell: "/bin/zsh", Architects: map[string]config.ArchitectConfig{"hiveryn": {Path: architectPath, Group: "personal", Repos: map[string]string{"daemon": repoPath}}}},
		repo:           repo,
		tickets:        &fakeTicketService{ticket: domain.Ticket{TicketSummary: domain.TicketSummary{ID: "ticket-1", Repo: "daemon"}}},
		terminal:       terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}

	info, err := service.CreateTerminal(context.Background(), "sess-1", domain.CreateTerminalParams{})
	if err != nil {
		t.Fatalf("CreateTerminal failed: %v", err)
	}
	if info.Command != "/bin/zsh" {
		t.Fatalf("expected configured shell command, got %#v", info)
	}
	if got := terminal.firstStartSpec(); got.Command != "/bin/zsh" || got.Workdir != repoPath || len(got.Args) != 0 {
		t.Fatalf("unexpected start spec %#v", got)
	}
	if terminal.firstStartSpec().TerminalID != info.TerminalID {
		t.Fatalf("expected terminal ID %q in start spec, got %q", info.TerminalID, terminal.firstStartSpec().TerminalID)
	}
}

func TestDefaultShellResolutionOrder(t *testing.T) {
	t.Run("config overrides env", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/fish")
		service := &Service{cfg: config.Config{Shell: "/bin/zsh"}}
		if got := service.defaultShell(); got != "/bin/zsh" {
			t.Fatalf("defaultShell() = %q, want /bin/zsh", got)
		}
	})

	t.Run("env used when config unset", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/fish")
		service := &Service{}
		if got := service.defaultShell(); got != "/bin/fish" {
			t.Fatalf("defaultShell() = %q, want /bin/fish", got)
		}
	})

	t.Run("bash fallback", func(t *testing.T) {
		t.Setenv("SHELL", "")
		service := &Service{}
		if got := service.defaultShell(); got != "bash" {
			t.Fatalf("defaultShell() = %q, want bash", got)
		}
	})
}

func TestKillTerminalRejectsMainTerminalID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "sess-1", Status: domain.SessionStatusRunning}
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{"sess-1": {mainTerminalID: "term-main-1"}},
	}

	err := service.KillTerminal(context.Background(), "sess-1", "term-main-1")
	if err == nil {
		t.Fatal("expected main terminal kill to be rejected")
	}
	if vErr, ok := err.(*domain.ValidationError); !ok || vErr.Field != "terminal_id" {
		t.Fatalf("expected validation error for id, got %T %#v", err, err)
	}
}

func TestKillTerminalRemovesUserCreatedTab(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "sess-1", Status: domain.SessionStatusRunning}
	terminal := &fakeTerminalManager{terminals: []domain.TerminalInfo{{TerminalID: "term-1", SessionID: "sess-1", Command: "yazi", Status: "running"}}}
	service := &Service{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:          repo,
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{
			"sess-1": {
				tabs: []sessionTabState{{
					tab:          domain.SessionTab{Type: "terminal", TerminalID: "term-1", Command: "yazi", Status: "running"},
					removeOnExit: true,
				}},
			},
		},
	}

	if err := service.KillTerminal(context.Background(), "sess-1", "term-1"); err != nil {
		t.Fatalf("KillTerminal failed: %v", err)
	}
	tabs, err := service.ListSessionTabs(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListSessionTabs failed: %v", err)
	}
	if len(tabs) != 0 {
		t.Fatalf("expected user-created tab to be removed, got %#v", tabs)
	}
}

func TestKillTerminalKeepsConfiguredTabExited(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "sess-1", Status: domain.SessionStatusRunning}
	terminal := &fakeTerminalManager{terminals: []domain.TerminalInfo{{TerminalID: "term-1", SessionID: "sess-1", Command: "yazi", Status: "running"}}}
	service := &Service{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:          repo,
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{
			"sess-1": {
				tabs: []sessionTabState{{
					tab: domain.SessionTab{Type: "terminal", TerminalID: "term-1", Command: "yazi", Status: "running"},
				}},
			},
		},
	}

	if err := service.KillTerminal(context.Background(), "sess-1", "term-1"); err != nil {
		t.Fatalf("KillTerminal failed: %v", err)
	}
	tabs, err := service.ListSessionTabs(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListSessionTabs failed: %v", err)
	}
	if len(tabs) != 1 || tabs[0].Status != "exited" || tabs[0].TerminalID != "term-1" {
		t.Fatalf("expected configured terminal tab to persist as exited, got %#v", tabs)
	}
}

func TestAuxTerminalExitRemovesUserCreatedTab(t *testing.T) {
	t.Parallel()

	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		terminalStates: map[string]sessionTerminalState{
			"sess-1": {
				tabs: []sessionTabState{{
					tab:          domain.SessionTab{Type: "terminal", TerminalID: "term-1", Command: "yazi", Status: "running"},
					removeOnExit: true,
				}},
			},
		},
	}

	service.handleAuxTerminalExit(terminalExit{SessionID: "sess-1", TerminalID: "term-1"})
	if tabs := service.sessionTabs("sess-1"); len(tabs) != 0 {
		t.Fatalf("expected user-created terminal tab to be removed on exit, got %#v", tabs)
	}
}

func TestListSessionTabsResolvesTerminalStatus(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "sess-1", Status: domain.SessionStatusRunning}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		terminal: &fakeTerminalManager{terminals: []domain.TerminalInfo{
			{TerminalID: "term-1", SessionID: "sess-1", Command: "yazi", Status: "running"},
		}},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{
			"sess-1": {
				tabs: []sessionTabState{{tab: domain.SessionTab{Type: "kanban"}}, {tab: domain.SessionTab{Type: "terminal", TerminalID: "term-1", Command: "yazi"}}},
			},
		},
	}

	tabs, err := service.ListSessionTabs(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListSessionTabs failed: %v", err)
	}
	if len(tabs) != 2 {
		t.Fatalf("expected 2 tabs, got %#v", tabs)
	}
	if tabs[1].Status != "running" || tabs[1].TerminalID != "term-1" {
		t.Fatalf("unexpected terminal tab %#v", tabs[1])
	}
}

func TestListSessionsHydratesMainTerminalID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedSessions = []domain.Session{{ID: "sess-1", Status: domain.SessionStatusRunning}}
	service := &Service{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminalStates: map[string]sessionTerminalState{"sess-1": {mainTerminalID: "term-main-1"}},
	}

	sessions, err := service.ListSessions(context.Background(), domain.SessionListFilter{Status: domain.SessionStatusRunning})
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0].MainTerminalID != "term-main-1" {
		t.Fatalf("expected hydrated main terminal id, got %#v", sessions)
	}
}

func testRuntimeConfig(t *testing.T) config.Config {
	t.Helper()

	return config.Config{
		Variants: map[string]config.VariantConfig{
			"codex": {
				Agent: "codex",
				Env: map[string]string{
					"CODEX_HOME": "/custom/codex",
				},
			},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {
				Path:  t.TempDir(),
				Group: "personal",
				Repos: map[string]string{},
			},
		},
	}
}

type fakeAdapter struct {
	ensureRequest agentruntime.SetupRequest
	ensureErr     error
	launchRequest agentruntime.StartRequest
}

func (fakeAdapter) Agent() agentruntime.AgentKind {
	return agentruntime.AgentCodex
}

func (f *fakeAdapter) PrepareLaunch(_ context.Context, req agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	f.launchRequest = req
	return agentruntime.LaunchSpec{
		Command: "fake-command",
		Workdir: "/tmp",
	}, nil
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
		f.terminals = append(f.terminals, domain.TerminalInfo{
			TerminalID: spec.TerminalID,
			SessionID:  spec.SessionID,
			Command:    spec.Command,
			Status:     "running",
		})
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

func (f *fakeTerminalManager) Shutdown(context.Context) error {
	return nil
}

type fakeSessionRepository struct {
	createdSession domain.Session
	listedSessions []domain.Session
	lastListFilter domain.SessionListFilter
	updatedStatus  domain.SessionStatus
	appendedEvents []domain.AppendSessionEventParams
	operations     *[]string
}

func newFakeSessionRepository() *fakeSessionRepository {
	return &fakeSessionRepository{}
}

func (f *fakeSessionRepository) CreateSession(_ context.Context, params domain.CreateSessionParams) (domain.Session, error) {
	f.createdSession = domain.Session{
		ID:           params.ID,
		ProfileName:  params.ProfileName,
		ArchitectKey: params.ArchitectKey,
		SessionType:  params.SessionType,
		Prompt:       params.Prompt,
		Instructions: params.Instructions,
		Status:       params.Status,
		TicketID:     params.TicketID,
	}
	return f.createdSession, nil
}

func (f *fakeSessionRepository) GetSession(context.Context, string) (domain.Session, error) {
	return f.createdSession, nil
}

func (f *fakeSessionRepository) ListSessions(_ context.Context, filter domain.SessionListFilter) ([]domain.Session, error) {
	f.lastListFilter = filter
	if f.listedSessions == nil {
		return nil, nil
	}
	return append([]domain.Session(nil), f.listedSessions...), nil
}

func (f *fakeSessionRepository) UpdateSessionStatus(_ context.Context, id string, status domain.SessionStatus) error {
	if id != f.createdSession.ID {
		return &domain.NotFoundError{Resource: "session", ID: id}
	}
	f.updatedStatus = status
	return nil
}

func (f *fakeSessionRepository) UpdateSessionNativeID(context.Context, string, string) error {
	return nil
}

func (f *fakeSessionRepository) EndSession(context.Context, string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "end")
	}
	return nil
}

func (f *fakeSessionRepository) DeleteSession(context.Context, string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete")
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
	return domain.SessionEvent{
		SessionID: params.SessionID,
		Type:      params.Type,
		Status:    params.Status,
		Message:   params.Message,
		Raw:       params.Raw,
		At:        params.At,
	}, nil
}

func (f *fakeSessionRepository) lastAppendedEvent(t *testing.T) domain.AppendSessionEventParams {
	t.Helper()
	if len(f.appendedEvents) == 0 {
		t.Fatal("expected appended session event")
	}
	return f.appendedEvents[len(f.appendedEvents)-1]
}

func (f *fakeSessionRepository) FailRunningSessions(context.Context) error {
	return nil
}

type fakeTicketService struct {
	ticket domain.Ticket
	err    error
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

func (f *fakeTicketService) DeleteTicket(context.Context, string, string) error {
	return nil
}

func (f *fakeTicketService) MoveTicket(context.Context, string, string, domain.MoveTicketParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) ConcludeTicket(context.Context, string, string, domain.TicketConclusion) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func TestSpawnArchitectSessionAddsMCPServer(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
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

	if _, err := service.SpawnArchitectSession(context.Background(), domain.SpawnArchitectSessionRequest{
		ArchitectKey: "hiveryn",
		ProfileName:  "codex",
	}); err != nil {
		t.Fatalf("SpawnArchitectSession failed: %v", err)
	}

	assertMCPServer(t, adapter.launchRequest.MCPServers, domain.SessionTypeArchitect)
}

func TestSpawnWorkSessionAddsMCPServer(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	adapter := &fakeAdapter{}
	service := &Service{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfigWithPaths(architectPath, repoPath),
		repo:    newFakeSessionRepository(),
		tickets: &fakeTicketService{ticket: domain.Ticket{TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusBacklog, Created: &created, Updated: &created, References: []string{}, Warnings: []domain.TicketWarning{}}, Body: "body"}},
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

	if _, err := service.SpawnWorkSession(context.Background(), domain.SpawnWorkSessionRequest{
		ArchitectKey: "hiveryn",
		TicketID:     "ticket-1",
		ProfileName:  "codex",
	}); err != nil {
		t.Fatalf("SpawnWorkSession failed: %v", err)
	}

	assertMCPServer(t, adapter.launchRequest.MCPServers, domain.SessionTypeWork)
}

func assertMCPServer(t *testing.T, servers []agentruntime.MCPServerConfig, sessionType domain.SessionType) {
	t.Helper()

	if len(servers) != 1 {
		t.Fatalf("mcp servers = %#v", servers)
	}
	server := servers[0]
	if server.Name != "hiveryn-daemon" {
		t.Fatalf("name = %q", server.Name)
	}
	if server.Command != "/tmp/hiverynd" {
		t.Fatalf("command = %q", server.Command)
	}
	if len(server.Args) != 5 {
		t.Fatalf("args = %#v", server.Args)
	}
	if server.Args[0] != "mcp" || server.Args[1] != "--architect-key" || server.Args[2] != "hiveryn" || server.Args[3] != "--daemon-url" || server.Args[4] != "http://127.0.0.1:4200" {
		t.Fatalf("args = %#v", server.Args)
	}
	if server.Env["HIVERYN_DAEMON_URL"] != "http://127.0.0.1:4200" {
		t.Fatalf("env = %#v", server.Env)
	}
	if server.Env["HIVERYN_ARCHITECT_KEY"] != "hiveryn" {
		t.Fatalf("env = %#v", server.Env)
	}
	if server.Env["HIVERYN_SESSION_TYPE"] != string(sessionType) {
		t.Fatalf("env = %#v", server.Env)
	}
}

func testRuntimeConfigWithPaths(architectPath, repoPath string) config.Config {
	return config.Config{
		Variants: map[string]config.VariantConfig{
			"codex": {
				Agent: "codex",
				Env: map[string]string{
					"CODEX_HOME": "/custom/codex",
				},
			},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {
				Path:  architectPath,
				Group: "personal",
				Repos: map[string]string{"daemon": repoPath},
			},
		},
	}
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
