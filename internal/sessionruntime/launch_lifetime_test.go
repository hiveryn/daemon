package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/domain"
)

// blockingTerminal holds the main-terminal start until the test releases it,
// standing in for a slow launch step (a remote launch spends seconds in SSH).
type blockingTerminal struct {
	*fakeTerminalManager
	started chan context.Context
	release chan struct{}
}

func (b *blockingTerminal) Start(ctx context.Context, spec terminalStartSpec) error {
	b.started <- ctx
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.fakeTerminalManager.Start(ctx, spec)
}

func newLaunchLifetimeService(t *testing.T, terminal terminalManager) (*Service, *fakeSessionRepository) {
	t.Helper()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		Instructions: "system",
	}
	adapter := &fakeAdapter{}
	return &Service{
		intents:       newIntentStore(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:           testRuntimeConfig(t),
		repo:          repo,
		receiver:      ingest.NewReceiver(adapter),
		adapters:      map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		baseURL:       "http://127.0.0.1:4999",
	}, repo
}

func TestCreateRunOutlivesRequesterAndRejectsConcurrentLaunch(t *testing.T) {
	t.Parallel()
	terminal := &blockingTerminal{fakeTerminalManager: &fakeTerminalManager{}, started: make(chan context.Context, 1), release: make(chan struct{})}
	service, repo := newLaunchLifetimeService(t, terminal)

	requester, stopWaiting := context.WithCancel(context.Background())
	type outcome struct {
		result domain.CreateSessionRunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := service.CreateRun(requester, "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"})
		done <- outcome{result, err}
	}()
	launchCtx := <-terminal.started

	// A second launch of the same session while the first is in flight is a
	// conflict, answered at once rather than racing toward a second worker.
	_, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent launch: %v", err)
	}

	// The requester giving up (desktop timeout, closed dialog) must not reach
	// the launch: a remote launch interrupted mid-preparation is the dangerous
	// state this protects against.
	stopWaiting()
	select {
	case <-launchCtx.Done():
		t.Fatal("requester cancellation reached the launch")
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok := launchCtx.Deadline(); !ok {
		t.Fatal("launch has no bound of its own")
	}
	close(terminal.release)
	got := <-done
	if got.err != nil || got.result.MainTerminalID == "" || repo.createdRun.ID == "" {
		t.Fatalf("launch did not complete after requester left: %+v", got)
	}
	if repo.failedRunID != "" {
		t.Fatalf("completed launch marked failed: %q", repo.failedRunID)
	}
}

func TestCreateRunReportsItsOwnLaunchDeadline(t *testing.T) {
	t.Parallel()
	terminal := &blockingTerminal{fakeTerminalManager: &fakeTerminalManager{}, started: make(chan context.Context, 1), release: make(chan struct{})}
	service, repo := newLaunchLifetimeService(t, terminal)
	service.launchTimeout = 50 * time.Millisecond

	_, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "launch did not complete within 50ms: ") {
		t.Fatalf("deadline error: %v", err)
	}
	if repo.failedRunID != repo.createdRun.ID || repo.failedRunReason != domain.SessionRunFailureLaunchFailed {
		t.Fatalf("timed-out launch left run %q (failed=%q)", repo.createdRun.ID, repo.failedRunID)
	}
	<-terminal.started
	// The guard is released, so a retry is a fresh launch, not a conflict.
	close(terminal.release)
	service.launchTimeout = 0
	repo.createdRun = domain.SessionRun{}
	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
}

func TestFailedRemoteLaunchNeverKillsAServerItDidNotCreate(t *testing.T) {
	t.Parallel()
	// No repository and no SSH: the pre-existing server is left alone and the
	// error says how to recover.
	s := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := s.cleanupFailedRemoteLaunch(domain.Session{ID: "s", Machine: "box", SSH: "box"}, errors.Join(errors.New("create remote worker tmux"), errRemoteServerExists))
	if !errors.Is(err, errRemoteServerExists) || !strings.Contains(err.Error(), "discard the session") {
		t.Fatalf("existing server: %v", err)
	}
}
