package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
}

func (fakeAdapter) Agent() agentruntime.AgentKind {
	return agentruntime.AgentCodex
}

func (fakeAdapter) PrepareLaunch(context.Context, agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
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

func (f *fakeSessionRepository) DeleteSession(context.Context, string) error {
	return nil
}

func (f *fakeSessionRepository) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (f *fakeSessionRepository) AppendSessionEvent(context.Context, domain.AppendSessionEventParams) (domain.SessionEvent, error) {
	return domain.SessionEvent{}, nil
}

func (f *fakeSessionRepository) FailRunningSessions(context.Context) error {
	return nil
}
