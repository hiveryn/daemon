package sessionruntime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// architectEventRecorder captures everything published to the architect stream.
// Publishing happens on the intent exec goroutine, so it has to be safe to read
// from the test goroutine.
type architectEventRecorder struct {
	mu     sync.Mutex
	events []domain.ArchitectEvent
}

func (r *architectEventRecorder) publish(_ string, event domain.ArchitectEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *architectEventRecorder) snapshot() []domain.ArchitectEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.ArchitectEvent(nil), r.events...)
}

func (r *architectEventRecorder) withReason(reason domain.ArchitectEventReason) []domain.ArchitectEvent {
	var matched []domain.ArchitectEvent
	for _, event := range r.snapshot() {
		if event.Reason == reason {
			matched = append(matched, event)
		}
	}
	return matched
}

// waitForReason polls until at least one event with the reason arrives. The
// spawn intent resolves on a detached goroutine, so the publish can land just
// after the blocking call returns.
func (r *architectEventRecorder) waitForReason(t *testing.T, reason domain.ArchitectEventReason) []domain.ArchitectEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if matched := r.withReason(reason); len(matched) > 0 {
			return matched
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for a %q architect event; got %#v", reason, r.snapshot())
	return nil
}

func assertSessionStarted(t *testing.T, event domain.ArchitectEvent, architectKey, sessionID, ticketID string) {
	t.Helper()
	if event.Type != domain.ArchitectEventType {
		t.Fatalf("session_started type = %q, want %q", event.Type, domain.ArchitectEventType)
	}
	if event.ArchitectKey != architectKey {
		t.Fatalf("session_started architect_key = %q, want %q", event.ArchitectKey, architectKey)
	}
	if event.SessionID != sessionID {
		t.Fatalf("session_started session_id = %q, want %q", event.SessionID, sessionID)
	}
	if event.TicketID != ticketID {
		t.Fatalf("session_started ticket_id = %q, want %q", event.TicketID, ticketID)
	}
	if event.At.IsZero() {
		t.Fatal("session_started carried a zero timestamp")
	}
}

// A running session is only discoverable by a client that did not create it if
// CreateRun announces it, so pin that it does — with the session id the client
// needs in order to subscribe.
func TestCreateRunAnnouncesTicketSessionOnArchitectStream(t *testing.T) {
	t.Parallel()

	recorder := &architectEventRecorder{}
	service := newRunnableService(t, domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "Work ticket-1",
		Workdir:      t.TempDir(),
	}, recorder)

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	started := recorder.withReason(domain.ArchitectEventSessionStarted)
	if len(started) != 1 {
		t.Fatalf("expected exactly one session_started, got %d: %#v", len(started), recorder.snapshot())
	}
	assertSessionStarted(t, started[0], "hiveryn", "session-1", "ticket-1")

	// The board still needs its own signal — session discovery must not have
	// replaced the ticket move.
	moved := recorder.withReason(domain.ArchitectEventTicketMoved)
	if len(moved) != 1 || moved[0].TicketID != "ticket-1" || moved[0].SessionID != "session-1" {
		t.Fatalf("expected one ticket_moved for ticket-1/session-1, got %#v", moved)
	}
}

// Architect and freeform sessions have no ticket, so they must be announced
// without claiming one — ContextID is not a ticket id for them.
func TestCreateRunAnnouncesFreeformSessionWithoutTicket(t *testing.T) {
	t.Parallel()

	recorder := &architectEventRecorder{}
	service := newRunnableService(t, domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-13-1600-investigate-login-failure",
		Prompt:       "Investigate login failure",
		Workdir:      t.TempDir(),
	}, recorder)

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	started := recorder.withReason(domain.ArchitectEventSessionStarted)
	if len(started) != 1 {
		t.Fatalf("expected exactly one session_started, got %d: %#v", len(started), recorder.snapshot())
	}
	assertSessionStarted(t, started[0], "hiveryn", "session-1", "")

	if moved := recorder.withReason(domain.ArchitectEventTicketMoved); len(moved) != 0 {
		t.Fatalf("freeform session must not move a ticket, got %#v", moved)
	}
}

// The reported bug: an architect MCP spawn created a real running session that
// never appeared in the desktop. The desktop can only learn about it from the
// architect stream, so the spawn path must emit session_started — exactly once,
// since emission used to be duplicated between the HTTP handler and this path.
func TestApprovedSpawnIntentAnnouncesSessionExactlyOnce(t *testing.T) {
	t.Parallel()

	recorder := &architectEventRecorder{}
	service := newSpawnableService(t, recorder, 20)

	done := make(chan domain.IntentResolution[domain.SpawnTicketSessionResult], 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", "codex")
		done <- res
		errCh <- err
	}()

	intentID := awaitPendingIntent(t, service, "architect-session")
	if _, err := service.ApproveIntent(context.Background(), "architect-session", intentID); err != nil {
		t.Fatalf("approve intent: %v", err)
	}

	res := <-done
	if err := <-errCh; err != nil {
		t.Fatalf("spawn request returned error: %v", err)
	}
	if res.Outcome != domain.IntentOutcomeApproved {
		t.Fatalf("spawn outcome = %q, want %q", res.Outcome, domain.IntentOutcomeApproved)
	}
	if res.Result.SessionID == "" {
		t.Fatal("approved spawn returned no session id")
	}

	started := recorder.waitForReason(t, domain.ArchitectEventSessionStarted)
	if len(started) != 1 {
		t.Fatalf("expected exactly one session_started, got %d: %#v", len(started), recorder.snapshot())
	}
	assertSessionStarted(t, started[0], "hiveryn", res.Result.SessionID, "ticket-1")
}

// spawnTicketSession is wait-then-allow, so a spawn nobody answers still creates
// a real session. That is precisely the case that went invisible, so the
// timeout path must announce the session too.
func TestAutoApprovedSpawnIntentAnnouncesSession(t *testing.T) {
	t.Parallel()

	recorder := &architectEventRecorder{}
	service := newSpawnableService(t, recorder, 1)

	res, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", "codex")
	if err != nil {
		t.Fatalf("spawn request returned error: %v", err)
	}
	if res.Outcome != domain.IntentOutcomeAutoApproved {
		t.Fatalf("spawn outcome = %q, want %q", res.Outcome, domain.IntentOutcomeAutoApproved)
	}

	started := recorder.waitForReason(t, domain.ArchitectEventSessionStarted)
	if len(started) != 1 {
		t.Fatalf("expected exactly one session_started, got %d: %#v", len(started), recorder.snapshot())
	}
	assertSessionStarted(t, started[0], "hiveryn", res.Result.SessionID, "ticket-1")
}

// A denied spawn creates nothing, so it must announce nothing — otherwise a
// desktop would open a tab for a session that does not exist.
func TestDeniedSpawnIntentAnnouncesNoSession(t *testing.T) {
	t.Parallel()

	recorder := &architectEventRecorder{}
	service := newSpawnableService(t, recorder, 20)

	done := make(chan domain.IntentResolution[domain.SpawnTicketSessionResult], 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", "codex")
		done <- res
		errCh <- err
	}()

	intentID := awaitPendingIntent(t, service, "architect-session")
	if err := service.DenyIntent(context.Background(), "architect-session", intentID, "not now"); err != nil {
		t.Fatalf("deny intent: %v", err)
	}
	if res := <-done; res.Outcome != domain.IntentOutcomeDeniedByUser {
		t.Fatalf("spawn outcome = %q, want %q", res.Outcome, domain.IntentOutcomeDeniedByUser)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("spawn request returned error: %v", err)
	}

	if started := recorder.withReason(domain.ArchitectEventSessionStarted); len(started) != 0 {
		t.Fatalf("denied spawn announced a session: %#v", started)
	}
}

// tempGitRepo is a temp dir that passes the daemon's repo-path check, which
// requires a .git directory before a session may be scoped to it.
func tempGitRepo(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Mkdir(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return path
}

func awaitPendingIntent(t *testing.T, service *Service, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := service.intents.PendingForSession(sessionID); len(ids) == 1 {
			return ids[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("intent for session %s was never created", sessionID)
	return ""
}

// newRunnableService builds a Service wired with the launch dependencies
// CreateRun needs, plus an architect publisher.
func newRunnableService(t *testing.T, session domain.Session, recorder *architectEventRecorder) *Service {
	t.Helper()
	repo := newFakeSessionRepository()
	repo.createdSession = session
	adapter := &fakeAdapter{}
	return &Service{
		intents:          newIntentStore(),
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:              testRuntimeConfig(t),
		repo:             repo,
		tickets:          &fakeTicketService{},
		receiver:         ingest.NewReceiver(adapter),
		adapters:         map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter},
		terminal:         &fakeTerminalManager{},
		eventStreams:     map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:    map[string]func(){},
		publishArchitect: recorder.publish,
	}
}

// newSpawnableService builds a Service that can both run the spawn intent flow
// and actually launch the resulting run, so the intent's Exec reaches CreateRun.
func newSpawnableService(t *testing.T, recorder *architectEventRecorder, waitTimeout int) *Service {
	t.Helper()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID: "architect-session", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect,
		CurrentRun: &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	adapter := &fakeAdapter{}
	return &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:    repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID: "ticket-1", Title: "Implement spawn", Status: domain.TicketStatusBacklog,
				Repo: "daemon", AdditionalRepos: []string{"shared"},
			},
		}},
		cfg: config.Config{
			IntentWaitTimeout: waitTimeout,
			Variants:          map[string]config.VariantConfig{"codex": {Agent: "codex"}},
			Architects: map[string]config.ArchitectConfig{"hiveryn": {
				Path:  t.TempDir(),
				Repos: map[string]string{"daemon": tempGitRepo(t), "shared": tempGitRepo(t)},
			}},
		},
		receiver:         ingest.NewReceiver(adapter),
		adapters:         map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter},
		terminal:         &fakeTerminalManager{},
		eventStreams:     map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:    map[string]func(){},
		publishArchitect: recorder.publish,
	}
}
