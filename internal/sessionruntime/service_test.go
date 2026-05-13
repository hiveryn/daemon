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
	if terminal.startSpec.ID != repo.createdSession.ID {
		t.Fatalf("expected terminal to start reserved session %q, got %q", repo.createdSession.ID, terminal.startSpec.ID)
	}
	if terminal.startSpec.Size.Cols != 132 || terminal.startSpec.Size.Rows != 48 {
		t.Fatalf("expected terminal start size to use requested dimensions, got %#v", terminal.startSpec.Size)
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

	repo := newFakeSessionRepository()
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
}

func TestConcludeWorkSessionAppendsEndedEventRawConclusionData(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	commit := createTestGitCommit(t, repoPath)
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	repo := newFakeSessionRepository()
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
}

func testRuntimeConfig(t *testing.T) config.Config {
	t.Helper()

	return config.Config{
		AgentProfiles: map[string]config.AgentProfileConfig{
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
	startErr  error
	startSpec terminalStartSpec
}

func (f *fakeTerminalManager) Start(_ context.Context, spec terminalStartSpec) error {
	f.startSpec = spec
	return f.startErr
}

func (f *fakeTerminalManager) Attach(context.Context, string) (domain.TerminalAttachment, error) {
	return nil, nil
}

func (f *fakeTerminalManager) Kill(context.Context, string) error {
	return nil
}

func (f *fakeTerminalManager) Shutdown(context.Context) error {
	return nil
}

type fakeSessionRepository struct {
	createdSession domain.Session
	updatedStatus  domain.SessionStatus
	appendedEvents []domain.AppendSessionEventParams
}

func newFakeSessionRepository() *fakeSessionRepository {
	return &fakeSessionRepository{}
}

func (f *fakeSessionRepository) CreateSession(_ context.Context, params domain.CreateSessionParams) (domain.Session, error) {
	f.createdSession = domain.Session{
		ID:           params.ID,
		ProfileName:  params.ProfileName,
		ArchitectKey: params.ArchitectKey,
		Prompt:       params.Prompt,
		Instructions: params.Instructions,
		Status:       params.Status,
	}
	return f.createdSession, nil
}

func (f *fakeSessionRepository) GetSession(context.Context, string) (domain.Session, error) {
	return f.createdSession, nil
}

func (f *fakeSessionRepository) ListSessions(context.Context, domain.SessionListFilter) ([]domain.Session, error) {
	return nil, nil
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
	return nil
}

func (f *fakeSessionRepository) DeleteSession(context.Context, string) error {
	return nil
}

func (f *fakeSessionRepository) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (f *fakeSessionRepository) AppendSessionEvent(_ context.Context, params domain.AppendSessionEventParams) (domain.SessionEvent, error) {
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
		AgentProfiles: map[string]config.AgentProfileConfig{
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
