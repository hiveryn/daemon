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

func TestCreateIntentArchitectEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		createIntentResult: domain.SessionIntent{
			ID:           "intent-1",
			ArchitectKey: "hiveryn",
			SessionType:  domain.SessionTypeArchitect,
			ContextID:    "2026-05-13-1500",
			Prompt:       "kickoff",
			Workdir:      "/tmp/architect",
			CreatedBy:    domain.SessionCreatedByDesktop,
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/sessions", strings.NewReader(`{"session_type":"architect","architect_key":"hiveryn"}`))
	if status != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, status, string(body))
	}

	var payload domain.SessionIntent
	decodeEnvelopeData(t, body, &payload)
	if payload.ID != "intent-1" || payload.SessionType != domain.SessionTypeArchitect {
		t.Fatalf("unexpected payload %#v", payload)
	}
	if service.lastCreateIntent.ArchitectKey != "hiveryn" || service.lastCreateIntent.SessionType != domain.SessionTypeArchitect {
		t.Fatalf("unexpected create intent request %#v", service.lastCreateIntent)
	}
}

func TestCreateIntentFreeformEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		createIntentResult: domain.SessionIntent{
			ID:           "intent-2",
			ArchitectKey: "hiveryn",
			SessionType:  domain.SessionTypeFreeform,
			ContextID:    "2026-05-13-1500-investigate-login-failure",
			Prompt:       "Investigate login failure and report root cause",
			Workdir:      "/tmp/service-a",
			CreatedBy:    domain.SessionCreatedByDesktop,
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/sessions", strings.NewReader(`{"session_type":"freeform","architect_key":"hiveryn","prompt":"Investigate login failure and report root cause","workdir":"/tmp/service-a","slug":"investigate-login-failure"}`))
	if status != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, status, string(body))
	}

	var payload domain.SessionIntent
	decodeEnvelopeData(t, body, &payload)
	if payload.SessionType != domain.SessionTypeFreeform || payload.ContextID != "2026-05-13-1500-investigate-login-failure" {
		t.Fatalf("unexpected payload %#v", payload)
	}
	if service.lastCreateIntent.Workdir != "/tmp/service-a" || service.lastCreateIntent.Slug != "investigate-login-failure" {
		t.Fatalf("unexpected create intent request %#v", service.lastCreateIntent)
	}
}

func TestCreateRunEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		createRunResult: domain.CreateSessionRunResult{
			Run: domain.SessionRun{
				ID:              "run-1",
				SessionIntentID: "intent-1",
				Status:          domain.SessionRunStatusRunning,
				ProfileName:     "codex-work",
				Workdir:         "/tmp/repo",
			},
			MainTerminalID: "term-main-1",
		},
		getIntentResult: domain.SessionIntent{
			ID:           "intent-1",
			ArchitectKey: "hiveryn",
			SessionType:  domain.SessionTypeTicket,
			ContextID:    "ticket-1",
			Prompt:       "kickoff",
			Workdir:      "/tmp/repo",
		},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/sessions/intent-1/runs", strings.NewReader(`{"profile_name":"codex-work","cols":120,"rows":40}`))
	if status != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, status, string(body))
	}

	var payload struct {
		Run            domain.SessionRun `json:"run"`
		MainTerminalID string            `json:"main_terminal_id"`
		WSURL          string            `json:"ws_url"`
	}
	decodeEnvelopeData(t, body, &payload)
	if payload.Run.ID != "run-1" || payload.Run.SessionIntentID != "intent-1" {
		t.Fatalf("unexpected payload %#v", payload)
	}
	if payload.MainTerminalID != "term-main-1" {
		t.Fatalf("expected main terminal id, got %#v", payload)
	}
	if payload.WSURL != "ws://example.com/ws/session/intent-1/terminal/term-main-1" {
		t.Fatalf("unexpected ws url %#v", payload)
	}
	if service.lastCreateRunIntentID != "intent-1" {
		t.Fatalf("unexpected create run intent id %q", service.lastCreateRunIntentID)
	}
	if service.lastCreateRun.ProfileName != "codex-work" || service.lastCreateRun.Cols != 120 || service.lastCreateRun.Rows != 40 {
		t.Fatalf("unexpected create run request %#v", service.lastCreateRun)
	}
}

func TestSessionsListEndpoint(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		intents: []domain.SessionIntent{{
			ID:           "intent-1",
			ArchitectKey: "hiveryn",
			SessionType:  domain.SessionTypeTicket,
			ContextID:    "ticket-1",
			Prompt:       "kickoff",
			Workdir:      "/tmp/repo",
			CurrentRun: &domain.SessionRun{
				ID:             "run-1",
				Status:         domain.SessionRunStatusRunning,
				MainTerminalID: "term-main-1",
			},
		}},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodGet, "/api/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var listed struct {
		Sessions []domain.SessionIntent `json:"sessions"`
	}
	decodeEnvelopeData(t, body, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != "intent-1" {
		t.Fatalf("unexpected sessions payload %#v", listed)
	}
	if listed.Sessions[0].CurrentRun == nil || listed.Sessions[0].CurrentRun.MainTerminalID != "term-main-1" {
		t.Fatalf("expected hydrated current run, got %#v", listed.Sessions[0])
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

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/session/intent-1/terminal/term-1"
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

	status, body := request(t, handler, http.MethodGet, "/api/sessions/intent-1/tabs", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var tabs []map[string]any
	decodeEnvelopeData(t, body, &tabs)
	if len(tabs) != 2 {
		t.Fatalf("expected 2 tabs, got %#v", tabs)
	}
	if tabs[1]["id"] != "term-1" || tabs[1]["command"] != "yazi" || tabs[1]["status"] != "running" {
		t.Fatalf("unexpected terminal tab payload %#v", tabs[1])
	}
}

func TestCreateTerminalEndpointAllowsEmptyBody(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		createTerminalResult: domain.TerminalInfo{TerminalID: "term-1", SessionID: "intent-1", Command: "/bin/zsh", Status: "running"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := request(t, handler, http.MethodPost, "/api/sessions/intent-1/terminals", strings.NewReader(`{}`))
	if status != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, status, string(body))
	}
	if service.lastCreateTerminalID != "intent-1" {
		t.Fatalf("unexpected session id %q", service.lastCreateTerminalID)
	}

	var terminal domain.TerminalInfo
	decodeEnvelopeData(t, body, &terminal)
	if terminal.TerminalID != "term-1" || terminal.Command != "/bin/zsh" {
		t.Fatalf("unexpected terminal payload %#v", terminal)
	}
}

func TestCreateTerminalEndpointRejectsCommandField(t *testing.T) {
	t.Parallel()

	handler := newSessionTestHandler(t, &fakeSessionService{})

	status, body := request(t, handler, http.MethodPost, "/api/sessions/intent-1/terminals", strings.NewReader(`{"command":"yazi"}`))
	if status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, status, string(body))
	}
	if !strings.Contains(string(body), "unknown field") {
		t.Fatalf("expected unknown field error, got %s", string(body))
	}
}

func TestSessionEventsSSEBacklog(t *testing.T) {
	t.Parallel()

	service := &fakeSessionServiceWithEvents{
		backlog: []domain.SessionEvent{{ID: "evt-1", SessionIntentID: "intent-1", Type: "status", Status: "working"}},
	}
	handler := newSessionTestHandler(t, service)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/intent-1/events", nil)
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

func TestConcludeSessionWorkerSuccess(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeResult: domain.ConcludeSessionResult{SessionID: "intent-1", ArchitectKey: "hiveryn", TicketID: "ticket-1"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/intent-1/conclude", map[string]any{
		"body":    "Implemented feature X.",
		"commits": []any{map[string]any{"sha": "abc123", "repo": "daemon"}},
	})
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]any
	decodeEnvelopeData(t, body, &payload)
	if payload["ticket_id"] != "ticket-1" {
		t.Fatalf("expected ticket_id=ticket-1, got %#v", payload)
	}
	if len(service.lastConcludeParams.Commits) != 1 || service.lastConcludeParams.Commits[0] != (domain.CommitRef{SHA: "abc123", Repo: "daemon"}) {
		t.Fatalf("expected object commit ref payload, got %#v", service.lastConcludeParams.Commits)
	}
}

func TestConcludeSessionEmptyBodySucceeds(t *testing.T) {
	t.Parallel()

	service := &fakeSessionService{
		concludeResult: domain.ConcludeSessionResult{SessionID: "intent-1", ArchitectKey: "hiveryn"},
	}
	handler := newSessionTestHandler(t, service)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/sessions/intent-1/conclude", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]any
	decodeEnvelopeData(t, body, &payload)
	if payload["session_id"] != "intent-1" {
		t.Fatalf("expected session_id=intent-1, got %#v", payload)
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
	createIntentResult      domain.SessionIntent
	createIntentErr         error
	lastCreateIntent        domain.CreateSessionIntentRequest
	createRunResult         domain.CreateSessionRunResult
	createRunErr            error
	lastCreateRunIntentID   string
	lastCreateRun           domain.CreateSessionRunRequest
	intents                 []domain.SessionIntent
	getIntentResult         domain.SessionIntent
	attachTerminal          func(context.Context, string, string) (domain.TerminalAttachment, error)
	createTerminalResult    domain.TerminalInfo
	createTerminalErr       error
	lastCreateTerminalID    string
	sessionTabs             []domain.SessionTab
	concludeResult          domain.ConcludeSessionResult
	concludeErr             error
	lastConcludeSessionID   string
	lastConcludeParams      domain.ConcludeSessionParams
	requestConclusionResult domain.ConcludeSessionResult
	requestConclusionErr    error
	approveConclusionResult domain.ConcludeSessionResult
	approveConclusionErr    error
	rejectConclusionErr     error
	readConclusionResult    domain.ArchitectConclusion
	readConclusionErr       error
}

func (f *fakeSessionService) CreateIntent(_ context.Context, req domain.CreateSessionIntentRequest) (domain.SessionIntent, error) {
	f.lastCreateIntent = req
	return f.createIntentResult, f.createIntentErr
}

func (f *fakeSessionService) CreateRun(_ context.Context, id string, req domain.CreateSessionRunRequest) (domain.CreateSessionRunResult, error) {
	f.lastCreateRunIntentID = id
	f.lastCreateRun = req
	return f.createRunResult, f.createRunErr
}

func (f *fakeSessionService) ConcludeSession(_ context.Context, id string, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	f.lastConcludeSessionID = id
	f.lastConcludeParams = params
	return f.concludeResult, f.concludeErr
}

func (f *fakeSessionService) RequestConclusion(_ context.Context, id string, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	f.lastConcludeSessionID = id
	f.lastConcludeParams = params
	return f.requestConclusionResult, f.requestConclusionErr
}

func (f *fakeSessionService) MoveTicketToDone(_ context.Context, architectKey, ticketID string, params domain.MoveTicketToDoneParams) (domain.MoveTicketToDoneResult, error) {
	return domain.MoveTicketToDoneResult{}, nil
}

func (f *fakeSessionService) ApproveConclusion(_ context.Context, id string) (domain.ConcludeSessionResult, error) {
	return f.approveConclusionResult, f.approveConclusionErr
}

func (f *fakeSessionService) RejectConclusion(_ context.Context, id string, reason string) error {
	return f.rejectConclusionErr
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

func (f *fakeSessionService) GetIntent(_ context.Context, id string) (domain.SessionIntent, error) {
	if f.getIntentResult.ID != "" {
		return f.getIntentResult, nil
	}
	for _, intent := range f.intents {
		if intent.ID == id {
			return intent, nil
		}
	}
	return domain.SessionIntent{ID: id}, nil
}

func (f *fakeSessionService) ListIntents(context.Context) ([]domain.SessionIntent, error) {
	return f.intents, nil
}

func (f *fakeSessionService) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (f *fakeSessionService) SubscribeSessionEvents(context.Context, string) (domain.SessionEventSubscription, error) {
	return &fakeEventSubscription{ch: make(chan domain.SessionEvent)}, nil
}

func (f *fakeSessionService) AttachTerminal(ctx context.Context, sessionID, terminalID string) (domain.TerminalAttachment, error) {
	if f.attachTerminal != nil {
		return f.attachTerminal(ctx, sessionID, terminalID)
	}
	return nil, nil
}

func (f *fakeSessionService) CreateTerminal(_ context.Context, id string, params domain.CreateTerminalParams) (domain.TerminalInfo, error) {
	f.lastCreateTerminalID = id
	return f.createTerminalResult, f.createTerminalErr
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

type fakeSessionServiceWithEvents struct {
	fakeSessionService
	backlog []domain.SessionEvent
}

func (f *fakeSessionServiceWithEvents) GetIntent(context.Context, string) (domain.SessionIntent, error) {
	return domain.SessionIntent{ID: "intent-1"}, nil
}

func (f *fakeSessionServiceWithEvents) ListSessionEvents(context.Context, string) ([]domain.SessionEvent, error) {
	return f.backlog, nil
}

func (f *fakeSessionServiceWithEvents) SubscribeSessionEvents(context.Context, string) (domain.SessionEventSubscription, error) {
	return &fakeEventSubscription{ch: make(chan domain.SessionEvent)}, nil
}
