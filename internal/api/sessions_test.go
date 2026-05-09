package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestArchitectSpawnEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		spawnResult: domain.SpawnArchitectSessionResult{
			Session: domain.Session{ID: "sess-1"},
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/architects/hiveryn/spawn", strings.NewReader(`{"profile_name":"claude-sonnet"}`))
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]string
	decodeEnvelopeData(t, body, &payload)
	if payload["session_id"] != "sess-1" {
		t.Fatalf("unexpected session id payload: %#v", payload)
	}
	if payload["ws_url"] != "ws://example.com/ws/session/sess-1" {
		t.Fatalf("unexpected ws url payload: %#v", payload)
	}
	if service.lastSpawn.ArchitectKey != "hiveryn" || service.lastSpawn.ProfileName != "claude-sonnet" {
		t.Fatalf("unexpected spawn request: %#v", service.lastSpawn)
	}
}

func TestSessionsListAndDeleteEndpoints(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		sessions: []domain.Session{
			{ID: "sess-1", Status: domain.SessionStatusRunning},
		},
	}
	handler := newSessionTestHandler(t, service)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/sessions?status=running", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		Sessions []domain.Session `json:"sessions"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != "sess-1" {
		t.Fatalf("unexpected session list payload: %#v", listed)
	}
	if service.lastListFilter.Status != domain.SessionStatusRunning {
		t.Fatalf("unexpected list filter: %#v", service.lastListFilter)
	}

	deleteStatus, _ := request(t, handler, http.MethodDelete, "/api/sessions/sess-1", nil)
	if deleteStatus != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d", http.StatusNoContent, deleteStatus)
	}
	if service.deletedID != "sess-1" {
		t.Fatalf("unexpected deleted session id %q", service.deletedID)
	}
}

func TestSessionWebSocketBridge(t *testing.T) {
	t.Parallel()

	attachment := newFakeTerminalAttachment()
	service := &fakeSessionService{
		attachTerminal: func(context.Context, string) (domain.TerminalAttachment, error) {
			return attachment, nil
		},
	}

	server := httptest.NewServer(newSessionTestHandler(t, service))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/session/sess-1"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	attachment.output <- []byte("hello")

	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if string(message) != "hello" {
		t.Fatalf("unexpected websocket payload %q", string(message))
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ls\n")); err != nil {
		t.Fatalf("write websocket input: %v", err)
	}
	if got := <-attachment.input; string(got) != "ls\n" {
		t.Fatalf("unexpected terminal input %q", string(got))
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("write websocket resize: %v", err)
	}

	select {
	case resize := <-attachment.resize:
		if resize.cols != 120 || resize.rows != 40 {
			t.Fatalf("unexpected resize payload %#v", resize)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resize message")
	}
}

func newSessionTestHandler(t *testing.T, sessions domain.SessionService) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(Dependencies{
		Config:   testConfig(),
		Logger:   logger,
		Sessions: sessions,
	})
}

type fakeSessionService struct {
	spawnResult      domain.SpawnArchitectSessionResult
	spawnErr         error
	lastSpawn        domain.SpawnArchitectSessionRequest
	sessions         []domain.Session
	lastListFilter   domain.SessionListFilter
	deletedID        string
	attachTerminal   func(context.Context, string) (domain.TerminalAttachment, error)
	getSessionResult domain.Session
}

func (f *fakeSessionService) SpawnArchitectSession(_ context.Context, req domain.SpawnArchitectSessionRequest) (domain.SpawnArchitectSessionResult, error) {
	f.lastSpawn = req
	return f.spawnResult, f.spawnErr
}

func (f *fakeSessionService) TerminateSession(_ context.Context, id string) error {
	f.deletedID = id
	return nil
}

func (f *fakeSessionService) GetSession(_ context.Context, id string) (domain.Session, error) {
	if f.getSessionResult.ID != "" {
		return f.getSessionResult, nil
	}
	for _, session := range f.sessions {
		if session.ID == id {
			return session, nil
		}
	}
	return domain.Session{ID: id}, nil
}

func (f *fakeSessionService) ListSessions(_ context.Context, filter domain.SessionListFilter) ([]domain.Session, error) {
	f.lastListFilter = filter
	return f.sessions, nil
}

func (f *fakeSessionService) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (f *fakeSessionService) SubscribeSessionEvents(context.Context, string) (domain.SessionEventSubscription, error) {
	return &fakeEventSubscription{ch: make(chan domain.SessionEvent)}, nil
}

func (f *fakeSessionService) AttachTerminal(ctx context.Context, id string) (domain.TerminalAttachment, error) {
	if f.attachTerminal != nil {
		return f.attachTerminal(ctx, id)
	}
	return nil, nil
}

type fakeEventSubscription struct {
	ch chan domain.SessionEvent
}

func (s *fakeEventSubscription) C() <-chan domain.SessionEvent { return s.ch }

func (s *fakeEventSubscription) Close() { close(s.ch) }

type fakeTerminalAttachment struct {
	output chan []byte
	input  chan []byte
	resize chan terminalResize
	once   sync.Once
}

type terminalResize struct {
	cols uint16
	rows uint16
}

func newFakeTerminalAttachment() *fakeTerminalAttachment {
	return &fakeTerminalAttachment{
		output: make(chan []byte, 4),
		input:  make(chan []byte, 4),
		resize: make(chan terminalResize, 4),
	}
}

func (t *fakeTerminalAttachment) Output() <-chan []byte { return t.output }

func (t *fakeTerminalAttachment) Write(data []byte) error {
	copied := append([]byte(nil), data...)
	t.input <- copied
	return nil
}

func (t *fakeTerminalAttachment) Resize(cols, rows uint16) error {
	t.resize <- terminalResize{cols: cols, rows: rows}
	return nil
}

func (t *fakeTerminalAttachment) Close() error {
	t.once.Do(func() {
		close(t.output)
	})
	return nil
}

func TestSessionEventsSSEBacklog(t *testing.T) {
	t.Parallel()

	service := &fakeSessionServiceWithEvents{
		backlog: []domain.SessionEvent{
			{ID: "evt-1", SessionID: "sess-1", Type: "status", Status: "working"},
		},
	}
	handler := newSessionTestHandler(t, service)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/sess-1/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, `"status":"working"`) {
		t.Fatalf("expected SSE backlog in body, got %q", body)
	}
}

type fakeSessionServiceWithEvents struct {
	fakeSessionService
	backlog []domain.SessionEvent
}

func (f *fakeSessionServiceWithEvents) GetSession(context.Context, string) (domain.Session, error) {
	return domain.Session{ID: "sess-1"}, nil
}

func (f *fakeSessionServiceWithEvents) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return f.backlog, nil
}

func (f *fakeSessionServiceWithEvents) SubscribeSessionEvents(context.Context, string) (domain.SessionEventSubscription, error) {
	return &fakeEventSubscription{ch: make(chan domain.SessionEvent)}, nil
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"profile_name":"x","extra":true}`))
	var dst struct {
		ProfileName string `json:"profile_name"`
	}
	if err := decodeJSON(req, &dst); err == nil {
		t.Fatal("expected decodeJSON to reject unknown field")
	}
}

func TestWriteSSEEvent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if err := writeSSEEvent(rec, domain.SessionEvent{ID: "evt-1", Type: "status"}); err != nil {
		t.Fatalf("writeSSEEvent: %v", err)
	}

	var payload domain.SessionEvent
	raw := strings.TrimPrefix(strings.TrimSpace(rec.Body.String()), "data: ")
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode sse payload: %v", err)
	}
	if payload.ID != "evt-1" {
		t.Fatalf("unexpected payload %#v", payload)
	}
}
