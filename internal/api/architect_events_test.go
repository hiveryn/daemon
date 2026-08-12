package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/archevents"
	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/domain"
)

func newArchitectEventsHandlerWithHub(t *testing.T) (http.Handler, *archevents.Hub) {
	t.Helper()

	cfg := testConfig()
	cfg.Architects = cloneArchitects(cfg.Architects)
	architect := cfg.Architects["hiveryn"]
	architect.Path = t.TempDir()
	cfg.Architects["hiveryn"] = architect

	hub := archevents.New(nil)
	handler := NewHandler(Dependencies{
		Config:          cfg,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tickets:         architectfs.NewTicketService(),
		ArchitectEvents: hub,
	})
	return handler, hub
}

// A session tab in the desktop is created from the session_id on this stream, so
// the field has to survive JSON serialization and SSE framing. Without it the
// desktop can see that something changed but not what session to open.
func TestArchitectSSECarriesSessionLifecycle(t *testing.T) {
	t.Parallel()

	handler, hub := newArchitectEventsHandlerWithHub(t)
	srv := httptest.NewServer(handler)
	defer srv.CloseClientConnections()
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/architects/hiveryn/events", nil)
	if err != nil {
		t.Fatalf("create SSE request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, string(body))
	}

	hub.Publish("hiveryn", domain.ArchitectEvent{
		Type:         domain.ArchitectEventType,
		ArchitectKey: "hiveryn",
		Reason:       domain.ArchitectEventSessionStarted,
		TicketID:     "2026-08-12-0235-simplify-capacity-dashboard",
		SessionID:    "92813733-df99-4f73-971d-e7ad5cc7f29b",
		At:           time.Now().UTC(),
	})

	event := readArchitectSSEEvent(t, resp.Body)
	if event.Type != domain.ArchitectEventType {
		t.Fatalf("type = %q, want %q", event.Type, domain.ArchitectEventType)
	}
	if event.ArchitectKey != "hiveryn" {
		t.Fatalf("architect_key = %q, want hiveryn", event.ArchitectKey)
	}
	if event.Reason != domain.ArchitectEventSessionStarted {
		t.Fatalf("reason = %q, want %q", event.Reason, domain.ArchitectEventSessionStarted)
	}
	if event.SessionID != "92813733-df99-4f73-971d-e7ad5cc7f29b" {
		t.Fatalf("session_id = %q, want the published session id", event.SessionID)
	}
	if event.TicketID != "2026-08-12-0235-simplify-capacity-dashboard" {
		t.Fatalf("ticket_id = %q, want the published ticket id", event.TicketID)
	}
	if event.At.IsZero() {
		t.Fatal("expected non-zero at timestamp")
	}
}

// The stream is architect-scoped, so one architect's spawn must never open a tab
// in another architect's window.
func TestArchitectSSEDoesNotLeakSessionsAcrossArchitects(t *testing.T) {
	t.Parallel()

	handler, hub := newArchitectEventsHandlerWithHub(t)
	srv := httptest.NewServer(handler)
	defer srv.CloseClientConnections()
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/architects/hiveryn/events", nil)
	if err != nil {
		t.Fatalf("create SSE request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	hub.Publish("other-architect", domain.ArchitectEvent{
		Type:         domain.ArchitectEventType,
		ArchitectKey: "other-architect",
		Reason:       domain.ArchitectEventSessionStarted,
		SessionID:    "foreign-session",
		At:           time.Now().UTC(),
	})
	hub.Publish("hiveryn", domain.ArchitectEvent{
		Type:         domain.ArchitectEventType,
		ArchitectKey: "hiveryn",
		Reason:       domain.ArchitectEventSessionEnded,
		SessionID:    "own-session",
		At:           time.Now().UTC(),
	})

	// The foreign event was published first; if scoping leaked it would arrive
	// first too.
	event := readArchitectSSEEvent(t, resp.Body)
	if event.SessionID != "own-session" || event.Reason != domain.ArchitectEventSessionEnded {
		t.Fatalf("expected only this architect's session_ended, got %#v", event)
	}
}

// readArchitectSSEEvent reads one `data:` frame, skipping keep-alive comments.
func readArchitectSSEEvent(t *testing.T, body io.Reader) domain.ArchitectEvent {
	t.Helper()

	reader := bufio.NewReader(body)
	eventCh := make(chan domain.ArchitectEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				errCh <- fmt.Errorf("expected data: prefix, got %q", line)
				return
			}
			var event domain.ArchitectEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				errCh <- err
				return
			}
			eventCh <- event
			return
		}
	}()

	select {
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	case err := <-errCh:
		t.Fatalf("read SSE event: %v", err)
	case event := <-eventCh:
		return event
	}
	return domain.ArchitectEvent{}
}
