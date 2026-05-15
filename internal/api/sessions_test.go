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
	"github.com/hiveryn/daemon/internal/archevents"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestArchitectSpawnEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		spawnResult: domain.SpawnArchitectSessionResult{
			Session:        domain.Session{ID: "sess-1"},
			MainTerminalID: "term-main-1",
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/architects/hiveryn/spawn", strings.NewReader(`{"profile_name":"claude-sonnet","cols":120,"rows":40}`))
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]string
	decodeEnvelopeData(t, body, &payload)
	if payload["session_id"] != "sess-1" {
		t.Fatalf("unexpected session id payload: %#v", payload)
	}
	if payload["main_terminal_id"] != "term-main-1" {
		t.Fatalf("unexpected main terminal id payload: %#v", payload)
	}
	if payload["ws_url"] != "ws://example.com/ws/session/sess-1/terminal/term-main-1" {
		t.Fatalf("unexpected ws url payload: %#v", payload)
	}
	if service.lastSpawn.ArchitectKey != "hiveryn" || service.lastSpawn.ProfileName != "claude-sonnet" {
		t.Fatalf("unexpected spawn request: %#v", service.lastSpawn)
	}
	if service.lastSpawn.Cols != 120 || service.lastSpawn.Rows != 40 {
		t.Fatalf("unexpected spawn dimensions: %#v", service.lastSpawn)
	}
}

func TestWorkerSpawnEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/architects/hiveryn/tickets/ticket-1/spawn", strings.NewReader(`{"profile_name":"codex-personal","mode":"normal","cols":100,"rows":30}`))
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]string
	decodeEnvelopeData(t, body, &payload)
	if payload["session_id"] != "ticket-1-session" {
		t.Fatalf("unexpected session id: %q", payload["session_id"])
	}
	if payload["main_terminal_id"] != "term-main-1" {
		t.Fatalf("unexpected main terminal id: %q", payload["main_terminal_id"])
	}
	if payload["ws_url"] != "ws://example.com/ws/session/ticket-1-session/terminal/term-main-1" {
		t.Fatalf("unexpected ws url: %q", payload["ws_url"])
	}
}

func TestSessionsListAndDeleteEndpointRemoved(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		sessions: []domain.Session{
			{ID: "sess-1", Status: domain.SessionStatusRunning, MainTerminalID: "term-main-1"},
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
	if listed.Sessions[0].MainTerminalID != "term-main-1" {
		t.Fatalf("unexpected main terminal id in list payload: %#v", listed.Sessions[0])
	}
	if service.lastListFilter.Status != domain.SessionStatusRunning {
		t.Fatalf("unexpected list filter: %#v", service.lastListFilter)
	}

	deleteStatus, _ := request(t, handler, http.MethodDelete, "/api/sessions/sess-1", nil)
	if deleteStatus != http.StatusMethodNotAllowed {
		t.Fatalf("expected delete status %d, got %d", http.StatusMethodNotAllowed, deleteStatus)
	}
}

func TestSessionWebSocketBridge(t *testing.T) {
	t.Parallel()

	attachment := newFakeTerminalAttachment()
	service := &fakeSessionService{
		attachTerminal: func(context.Context, string, string) (domain.TerminalAttachment, error) {
			return attachment, nil
		},
	}

	server := httptest.NewServer(newSessionTestHandler(t, service))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/session/sess-1/terminal/term-1"
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

func TestSessionTabsEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		sessionTabs: []domain.SessionTab{
			{Type: "kanban"},
			{Type: "terminal", TerminalID: "term-1", Command: "yazi", Status: "running"},
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodGet, "/api/sessions/sess-1/tabs", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var tabs []map[string]any
	decodeEnvelopeData(t, body, &tabs)
	if len(tabs) != 2 {
		t.Fatalf("expected 2 tabs, got %#v", tabs)
	}
	if tabs[1]["id"] != "term-1" || tabs[1]["command"] != "yazi" || tabs[1]["status"] != "running" {
		t.Fatalf("unexpected terminal tab payload: %#v", tabs[1])
	}
	if _, ok := tabs[1]["terminal_id"]; ok {
		t.Fatalf("expected tabs payload to use id, got %#v", tabs[1])
	}
}

func newSessionTestHandler(t *testing.T, sessions domain.SessionService) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(Dependencies{
		Config:   testConfig(),
		Logger:   logger,
		Sessions: sessions,
		Tickets:  &fakeTicketService{},
	})
}

type fakeSessionService struct {
	spawnResult           domain.SpawnArchitectSessionResult
	spawnErr              error
	lastSpawn             domain.SpawnArchitectSessionRequest
	sessions              []domain.Session
	lastListFilter        domain.SessionListFilter
	attachTerminal        func(context.Context, string, string) (domain.TerminalAttachment, error)
	getSessionResult      domain.Session
	sessionTabs           []domain.SessionTab
	concludeResult        domain.ConcludeSessionResult
	concludeErr           error
	lastConcludeSessionID string
	lastConcludeParams    domain.ConcludeSessionParams
	readConclusionResult  domain.ArchitectConclusion
	readConclusionErr     error
}

func (f *fakeSessionService) SpawnArchitectSession(_ context.Context, req domain.SpawnArchitectSessionRequest) (domain.SpawnArchitectSessionResult, error) {
	f.lastSpawn = req
	return f.spawnResult, f.spawnErr
}

func (f *fakeSessionService) SpawnWorkSession(_ context.Context, req domain.SpawnWorkSessionRequest) (domain.SpawnWorkSessionResult, error) {
	return domain.SpawnWorkSessionResult{
		Session:        domain.Session{ID: req.TicketID + "-session"},
		MainTerminalID: "term-main-1",
	}, nil
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

func (f *fakeSessionService) AttachTerminal(ctx context.Context, sessionID, name string) (domain.TerminalAttachment, error) {
	if f.attachTerminal != nil {
		return f.attachTerminal(ctx, sessionID, name)
	}
	return nil, nil
}

func (f *fakeSessionService) ConcludeSession(_ context.Context, id string, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	f.lastConcludeSessionID = id
	f.lastConcludeParams = params
	return f.concludeResult, f.concludeErr
}

func (f *fakeSessionService) ReadConclusion(_ context.Context, architectKey, id string) (domain.ArchitectConclusion, error) {
	return f.readConclusionResult, f.readConclusionErr
}

func (f *fakeSessionService) ReadRecentConclusion(_ context.Context, architectKey string) (domain.ArchitectConclusion, error) {
	return f.readConclusionResult, f.readConclusionErr
}

func (f *fakeSessionService) ListConclusions(_ context.Context, key string, limit int) ([]domain.ConclusionSummary, error) {
	return nil, nil
}

func (f *fakeSessionService) CreateTerminal(context.Context, string, domain.CreateTerminalParams) (domain.TerminalInfo, error) {
	return domain.TerminalInfo{}, nil
}

func (f *fakeSessionService) ListTerminals(context.Context, string) ([]domain.TerminalInfo, error) {
	return nil, nil
}

func (f *fakeSessionService) ListSessionTabs(context.Context, string) ([]domain.SessionTab, error) {
	return f.sessionTabs, nil
}

func (f *fakeSessionService) KillTerminal(context.Context, string, string) error {
	return nil
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

type fakeTicketService struct{}

func (f *fakeTicketService) ListTickets(context.Context, string) (domain.TicketBoard, error) {
	return domain.TicketBoard{}, nil
}

func (f *fakeTicketService) GetTicket(context.Context, string, string) (domain.Ticket, error) {
	return domain.Ticket{}, nil
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

func TestConcludeSessionArchitectSuccess(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeResult: domain.ConcludeSessionResult{SessionID: "sess-1", ArchitectKey: "hiveryn"},
		getSessionResult: domain.Session{
			ID:           "sess-1",
			ArchitectKey: "hiveryn",
			SessionType:  string(domain.SessionTypeArchitect),
			Status:       domain.SessionStatusRunning,
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-1/conclude", map[string]any{
		"body": "All tasks completed.",
	})
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]any
	decodeEnvelopeData(t, body, &payload)
	if payload["success"] != true {
		t.Fatalf("expected success=true, got %#v", payload)
	}
	if payload["session_id"] != "sess-1" {
		t.Fatalf("expected session_id=sess-1, got %#v", payload)
	}
	if service.lastConcludeSessionID != "sess-1" {
		t.Fatalf("expected conclude session id sess-1, got %q", service.lastConcludeSessionID)
	}
	if service.lastConcludeParams.Body != "All tasks completed." {
		t.Fatalf("expected body, got %#v", service.lastConcludeParams)
	}
}

func TestConcludeSessionWorkerSuccess(t *testing.T) {
	t.Parallel()

	hub := archevents.New()
	service := &fakeSessionService{
		concludeResult: domain.ConcludeSessionResult{SessionID: "sess-1", ArchitectKey: "hiveryn", TicketID: "ticket-1"},
		getSessionResult: domain.Session{
			ID:           "sess-1",
			ArchitectKey: "hiveryn",
			SessionType:  string(domain.SessionTypeWork),
			Status:       domain.SessionStatusRunning,
			TicketID:     "ticket-1",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(Dependencies{
		Config:          testConfig(),
		Logger:          logger,
		Sessions:        service,
		Tickets:         &fakeTicketService{},
		ArchitectEvents: hub,
	})

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-1/conclude", map[string]any{
		"body":    "Implemented feature X.",
		"commits": []string{"abc123"},
	})
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]any
	decodeEnvelopeData(t, body, &payload)
	if payload["success"] != true {
		t.Fatalf("expected success=true, got %#v", payload)
	}
	if payload["ticket_id"] != "ticket-1" {
		t.Fatalf("expected ticket_id=ticket-1, got %#v", payload)
	}
}

func TestConcludeSessionMissingBody(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeErr: &domain.ValidationError{Field: "body", Message: "is required"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-1/conclude", map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
}

func TestConcludeSessionNotFound(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeErr: &domain.NotFoundError{Resource: "session", ID: "sess-missing"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-missing/conclude", map[string]any{
		"body": "done",
	})
	if status != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, status, string(body))
	}
}

func TestConcludeSessionWrongType(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeErr: &domain.ValidationError{Field: "session_type", Message: "cannot conclude session of type collab"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-collab/conclude", map[string]any{
		"body": "done",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
}

func TestConcludeSessionAlreadyCompleted(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeErr: &domain.ValidationError{Field: "session_id", Message: "session is not running"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-done/conclude", map[string]any{
		"body": "done",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
}

func TestConcludeSessionWorkerMissingCommits(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeErr: &domain.ValidationError{Field: "commits", Message: "are required when not rejected"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-1/conclude", map[string]any{
		"body": "done",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
}

func TestConcludeSessionWorkerRejectedWithoutReason(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeErr: &domain.ValidationError{Field: "rejection_reason", Message: "is required when rejected is true"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/sess-1/conclude", map[string]any{
		"body":     "nothing done",
		"rejected": true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
}
