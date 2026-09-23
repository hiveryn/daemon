package sessionruntime

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
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
