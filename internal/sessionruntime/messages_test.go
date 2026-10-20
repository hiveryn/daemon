package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// fakeMessageAdapter is an agentruntime adapter that also delivers messages,
// configurable per provider mode.
type fakeMessageAdapter struct {
	fakeAdapter
	kind       agentruntime.AgentKind
	mode       agentruntime.MessageDeliveryMode
	gateOnIdle bool

	mu              sync.Mutex
	delivered       []agentruntime.DeliverRequest
	deliverErr      error
	acceptedOnError bool
	providerRef     string
	receipts        map[string]agentruntime.MessageReceipt
	confirmErr      error
	locateErr       error
	confirmed       []agentruntime.ConfirmRequest
}

func (f *fakeMessageAdapter) Agent() agentruntime.AgentKind { return f.kind }

func (f *fakeMessageAdapter) LocateTranscript(context.Context, agentruntime.LocateRequest) (string, error) {
	if f.locateErr != nil {
		return "", f.locateErr
	}
	return "/transcripts/" + string(f.kind), nil
}

func (f *fakeMessageAdapter) MessageCapability() agentruntime.MessageCapability {
	return agentruntime.MessageCapability{Mechanism: string(f.kind) + " fake route", Mode: f.mode, GateOnIdle: f.gateOnIdle}
}

func (f *fakeMessageAdapter) DeliverMessage(_ context.Context, req agentruntime.DeliverRequest) (agentruntime.DeliverResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deliverErr != nil {
		if f.acceptedOnError {
			f.delivered = append(f.delivered, req)
		}
		return agentruntime.DeliverResult{}, f.deliverErr
	}
	f.delivered = append(f.delivered, req)
	ref := f.providerRef
	if ref == "" {
		ref = "ref-" + req.MessageID
	}
	return agentruntime.DeliverResult{Outcome: agentruntime.DeliverAccepted, ProviderRef: ref}, nil
}

func (f *fakeMessageAdapter) ConfirmMessage(_ context.Context, req agentruntime.ConfirmRequest) (agentruntime.MessageReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmed = append(f.confirmed, req)
	if f.confirmErr != nil {
		return agentruntime.MessageReceipt{}, f.confirmErr
	}
	return f.receipts[req.MessageID], nil
}

func (f *fakeMessageAdapter) setReceipt(id string, r agentruntime.MessageReceipt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.receipts == nil {
		f.receipts = map[string]agentruntime.MessageReceipt{}
	}
	f.receipts[id] = r
}

func (f *fakeMessageAdapter) deliveries() []agentruntime.DeliverRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentruntime.DeliverRequest(nil), f.delivered...)
}

type messageFixture struct {
	t        *testing.T
	dbPath   string
	service  *Service
	repo     *fakeSessionRepository
	messages *store.AgentMessageStore
	tickets  *fakeTicketService
	adapters map[agentruntime.AgentKind]*fakeMessageAdapter
	now      time.Time
	mu       sync.Mutex
	events   []domain.ArchitectEvent
}

func newMessageFixture(t *testing.T) *messageFixture {
	t.Helper()
	return newMessageFixtureAt(t, filepath.Join(t.TempDir(), "daemon.db"))
}

// newMessageFixtureAt builds a fixture over the SQLite file at dbPath; a
// second fixture over the same file is a restarted daemon.
func newMessageFixtureAt(t *testing.T, dbPath string) *messageFixture {
	t.Helper()
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &messageFixture{t: t, dbPath: dbPath, repo: newFakeSessionRepository(), messages: store.NewAgentMessageStore(db), tickets: &fakeTicketService{},
		now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	f.adapters = map[agentruntime.AgentKind]*fakeMessageAdapter{
		agentruntime.AgentClaude:   {kind: agentruntime.AgentClaude, mode: agentruntime.MessageDeliveryPull, gateOnIdle: true},
		agentruntime.AgentCodex:    {kind: agentruntime.AgentCodex, mode: agentruntime.MessageDeliveryPush},
		agentruntime.AgentOpenCode: {kind: agentruntime.AgentOpenCode, mode: agentruntime.MessageDeliveryPush, gateOnIdle: true},
	}
	adapters := map[agentruntime.AgentKind]agentruntime.Adapter{}
	for kind, adapter := range f.adapters {
		adapters[kind] = adapter
	}
	f.service = &Service{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:     f.repo,
		tickets:  f.tickets,
		cfg:      config.Config{Architects: map[string]config.ArchitectConfig{"hiveryn": {Name: "Hiveryn", Path: t.TempDir()}}},
		adapters: adapters,
		intents:  newIntentStore(),
	}
	f.service.SetAgentMessageRepository(f.messages)
	f.service.messages.now = func() time.Time { return f.now }
	f.service.SetArchitectPublisher(func(_ string, event domain.ArchitectEvent) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, event)
	})
	return f
}

func (f *messageFixture) architectSession(id string, kind agentruntime.AgentKind, nativeID string) domain.Session {
	return domain.Session{
		ID: id, ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect, ContextID: "2026-10-20-1200",
		CurrentRun: &domain.SessionRun{ID: "run-" + id, SessionID: id, Status: domain.SessionRunStatusRunning, NativeID: nativeID, Workdir: "/work",
			ProfileSnapshot: &domain.AgentProfileSnapshot{Agent: string(kind), Env: map[string]string{"CODEX_HOME": "/codex-home"}}},
	}
}

func (f *messageFixture) enqueue(body string) domain.AgentMessage {
	f.t.Helper()
	m, err := f.service.EnqueueAgentMessage(context.Background(), AgentMessageParams{
		Kind: domain.AgentMessageTicketConcluded, Recipient: "architect:hiveryn", ArchitectKey: "hiveryn",
		DedupeKey: "test:" + body, Body: body,
	})
	if err != nil {
		f.t.Fatalf("enqueue: %v", err)
	}
	return m
}

func (f *messageFixture) dispatch() {
	f.t.Helper()
	if err := f.service.dispatchAgentMessages(context.Background()); err != nil {
		f.t.Fatalf("dispatch: %v", err)
	}
}

func (f *messageFixture) get(id string) domain.AgentMessage {
	f.t.Helper()
	m, err := f.messages.GetAgentMessage(context.Background(), id)
	if err != nil {
		f.t.Fatalf("get message: %v", err)
	}
	return m
}

func (f *messageFixture) expect(id string, state domain.AgentMessageState, condition string) domain.AgentMessage {
	f.t.Helper()
	m := f.get(id)
	if m.State != state || !strings.Contains(m.Condition, condition) {
		f.t.Fatalf("message %s = state %q condition %q (attempts %d, last error %q); want %q containing %q", id, m.State, m.Condition, m.Attempts, m.LastError, state, condition)
	}
	return m
}

// armWaiter posts a Claude waiter to the mailbox the way the hook does and
// returns a channel with the response it eventually gets.
func (f *messageFixture) armWaiter(sessionID, hookEvent, source string) <-chan *http.Response {
	f.t.Helper()
	server := httptest.NewServer(f.service.messages.mailbox.Handler())
	// Close blocks on open long polls: release the waiters first.
	f.t.Cleanup(server.Close)
	f.t.Cleanup(f.service.messages.mailbox.Close)
	hook := map[string]any{"hook_event_name": hookEvent, "session_id": "native-" + sessionID}
	if source != "" {
		hook["source"] = source
	}
	body, _ := json.Marshal(map[string]any{"hook": hook, "env": map[string]string{"AGENTRUNTIME_SESSION_ID": sessionID}})
	out := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(server.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			out <- nil
			return
		}
		out <- resp
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := f.service.messages.mailbox.Armed(sessionID); ok {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("waiter never armed")
	return out
}

func waiterResponse(t *testing.T, ch <-chan *http.Response) (int, string) {
	t.Helper()
	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("waiter request failed")
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	case <-time.After(5 * time.Second):
		t.Fatal("waiter got no response")
		return 0, ""
	}
}

func TestAgentMessageIsRetainedWithoutARunningRecipientAndNeverLaunchesOne(t *testing.T) {
	f := newMessageFixture(t)
	m := f.enqueue("Ticket T-1 has been concluded.")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "no running architect session for hiveryn")
	if len(f.repo.events()) != 0 || f.repo.createdRun.ID != "" {
		t.Fatal("dispatcher touched sessions")
	}
	// A completed (not running) session is not a recipient either.
	ended := f.architectSession("arch-1", agentruntime.AgentClaude, "native-arch-1")
	ended.CurrentRun.Status = domain.SessionRunStatusCompleted
	f.repo.listedSessions = []domain.Session{ended}
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "no running architect session")
	// A listing shows the open message with its condition.
	listed, err := f.service.ListAgentMessages(context.Background(), "hiveryn")
	if err != nil || len(listed) != 1 || listed[0].ID != m.ID {
		t.Fatalf("list = %+v %v", listed, err)
	}
}

func TestClaudeMessageIsHandedOnlyToAnArmedWaiterWhileIdle(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentClaude, "native-arch-1")}
	m := f.enqueue("Ticket T-1 has been concluded. Outcome: completed.")

	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "no waiter armed")

	// A startup SessionStart arms a waiter but the kickoff turn follows: busy.
	startup := f.armWaiter("arch-1", "SessionStart", "startup")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "agent busy")

	// The turn ends: the Stop hook reports idle and arms a new waiter (the
	// startup one is superseded with 204).
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle, NativeSessionRole: agentruntime.NativeSessionRolePrimary})
	stop := f.armWaiter("arch-1", "Stop", "")
	if status, _ := waiterResponse(t, startup); status != http.StatusNoContent {
		t.Fatalf("superseded waiter status %d", status)
	}
	f.dispatch()
	handed := f.expect(m.ID, domain.AgentMessageHanded, "handed to the Stop waiter")
	if handed.TargetSessionID != "arch-1" || handed.TargetAgent != "claude" || handed.TargetNativeID != "native-arch-1" || handed.Attempts != 1 {
		t.Fatalf("handed = %+v", handed)
	}
	status, body := waiterResponse(t, stop)
	if status != http.StatusOK || !strings.Contains(body, `"text":"Hiveryn message `+m.ID+`: Ticket T-1 has been concluded. Outcome: completed."`) {
		t.Fatalf("waiter got %d %s", status, body)
	}
	// The woken turn is running: a second message waits.
	if idle, _ := f.service.messageIdle("arch-1"); idle {
		t.Fatal("hand-off did not mark the session busy")
	}
	second := f.enqueue("second")
	f.dispatch()
	f.expect(second.ID, domain.AgentMessagePending, "waiting for the previous message")

	// The provider's record shows it: appeared, and acknowledged by a reply.
	f.adapters[agentruntime.AgentClaude].setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true, Acknowledged: true})
	f.dispatch()
	final := f.get(m.ID)
	if final.State != domain.AgentMessageAcknowledged || final.AppearedAt == nil || final.AcknowledgedAt == nil || final.EndedAt == nil || final.Condition != "" {
		t.Fatalf("final = %+v", final)
	}
	confirmed := f.adapters[agentruntime.AgentClaude].confirmed
	if len(confirmed) == 0 || confirmed[len(confirmed)-1].TranscriptHandle != "/transcripts/claude" || confirmed[len(confirmed)-1].MessageID != m.ID {
		t.Fatalf("confirm requests = %+v", confirmed)
	}
	// With the first appeared, the second is handed to the armed waiter now
	// that the session is idle again.
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.armWaiter("arch-1", "Stop", "")
	f.dispatch()
	f.expect(second.ID, domain.AgentMessageHanded, "handed to the Stop waiter")
}

func TestClaudeLostMessageIsRedeliveredUnderTheSameIdentity(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentClaude, "native-arch-1")}
	m := f.enqueue("lost")
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.armWaiter("arch-1", "Stop", "")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "handed")

	// Claude was killed after enqueueing and resumed without replaying: its
	// next turn ends, the transcript lacks the message.
	f.now = f.now.Add(time.Minute)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.dispatch()
	// Back to pending in the same pass, and immediately retried: no waiter
	// is armed (the previous one was consumed), so it waits for the next.
	again := f.expect(m.ID, domain.AgentMessagePending, "no waiter armed")
	if again.Attempts != 1 || !strings.Contains(again.LastError, "not found in the agent's record") || again.HandedAt != nil {
		t.Fatalf("repended = %+v", again)
	}
	// Idle just after the hand-off is the woken turn's own start, not a loss.
	resumed := f.armWaiter("arch-1", "SessionStart", "resume")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "handed to the SessionStart waiter")
	if status, body := waiterResponse(t, resumed); status != http.StatusOK || !strings.Contains(body, m.ID) {
		t.Fatalf("resumed waiter got %d %s", status, body)
	}
	f.now = f.now.Add(2 * time.Second)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageHanded || got.Attempts != 2 {
		t.Fatalf("after quick idle = %+v", got)
	}
}

func TestCodexMessageIsQueuedWithoutIdleGateAndReportsTheQueue(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	codex := f.adapters[agentruntime.AgentCodex]
	m := f.enqueue("codex")
	f.dispatch()
	handed := f.expect(m.ID, domain.AgentMessageHanded, "queued by the provider")
	deliveries := codex.deliveries()
	if len(deliveries) != 1 || deliveries[0].MessageID != m.ID || deliveries[0].NativeSessionID != "thread-1" || deliveries[0].Env["CODEX_HOME"] != "/codex-home" ||
		deliveries[0].Text != "Hiveryn message "+m.ID+": codex" || handed.ProviderRef != "ref-"+m.ID || handed.TargetConfigRoot != "/codex-home" {
		t.Fatalf("deliveries %+v handed %+v", deliveries, handed)
	}
	// Still queued (busy thread, or interrupted turn): not lost.
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{Queued: true})
	f.now = f.now.Add(time.Hour)
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "after an interrupted turn")
	// Dispatched: appeared, no reply yet.
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true})
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageAppeared || got.AcknowledgedAt != nil {
		t.Fatalf("appeared = %+v", got)
	}
	// The reply arrives later: upgraded within the acknowledgement window.
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true, Acknowledged: true})
	f.now = f.now.Add(time.Minute)
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageAcknowledged || got.AcknowledgedAt == nil {
		t.Fatalf("acknowledged = %+v", got)
	}
	confirmed := codex.confirmed[len(codex.confirmed)-1]
	if confirmed.Env["CODEX_HOME"] != "/codex-home" || confirmed.NativeSessionID != "thread-1" || confirmed.ProviderRef != "ref-"+m.ID {
		t.Fatalf("confirm = %+v", confirmed)
	}
}

func TestCodexMessageNeitherQueuedNorRecordedIsRedelivered(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	m := f.enqueue("codex-lost")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "queued")
	f.now = f.now.Add(messageHandedCap + time.Second)
	f.dispatch()
	// Returned to pending and re-queued in the same pass under the same id.
	got := f.expect(m.ID, domain.AgentMessageHanded, "queued")
	deliveries := f.adapters[agentruntime.AgentCodex].deliveries()
	if got.Attempts != 2 || len(deliveries) != 2 || deliveries[1].MessageID != m.ID || deliveries[1].ProviderRef != "ref-"+m.ID {
		t.Fatalf("redelivered = %+v deliveries %+v", got, deliveries)
	}
}

func TestOpenCodeMessageNeedsIdleAndTheLaunchEndpoint(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentOpenCode, "ses_1")}
	m := f.enqueue("opencode")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "agent busy")
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "server endpoint is unknown")

	delivery, err := f.service.messageDeliveryFor("arch-1", agentruntime.AgentOpenCode)
	if err != nil || delivery.ServerPort == 0 || len(delivery.ServerPassword) != 32 {
		t.Fatalf("delivery = %+v %v", delivery, err)
	}
	f.dispatch()
	handed := f.expect(m.ID, domain.AgentMessageHanded, "accepted by the provider")
	deliveries := f.adapters[agentruntime.AgentOpenCode].deliveries()
	if len(deliveries) != 1 || deliveries[0].ServerPort != delivery.ServerPort || deliveries[0].ServerPassword != delivery.ServerPassword || handed.ProviderRef == "" {
		t.Fatalf("deliveries %+v handed %+v", deliveries, handed)
	}
	if idle, _ := f.service.messageIdle("arch-1"); idle {
		t.Fatal("push did not mark the session busy")
	}
	// The session ending forgets its endpoint and gate.
	f.service.forgetMessageSession("arch-1")
	if _, ok := f.service.messageEndpoint("arch-1"); ok {
		t.Fatal("endpoint kept after session end")
	}
	// Claude and Codex need no endpoint.
	if d, err := f.service.messageDeliveryFor("x", agentruntime.AgentClaude); err != nil || d == nil || d.ServerPort != 0 {
		t.Fatalf("claude delivery = %+v %v", d, err)
	}
}

func TestAmbiguousDeliveryWaitsForReceiptAndConsumesBudget(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	codex := f.adapters[agentruntime.AgentCodex]
	codex.deliverErr = errors.New("codex app-server exited: boom")
	m := f.enqueue("retry")
	for attempt := 1; attempt <= messageMaxHandoffs; attempt++ {
		f.dispatch()
		got := f.expect(m.ID, domain.AgentMessageHanded, "acceptance unknown")
		if got.Attempts != attempt || got.HandedAt == nil || got.TargetNativeID != "thread-1" || got.LastError != codex.deliverErr.Error() {
			t.Fatalf("attempt: %+v", got)
		}
		f.now = f.now.Add(time.Second)
		f.dispatch()
		if got := f.get(m.ID); got.Attempts != attempt {
			t.Fatalf("early retry: %+v", got)
		}
		f.now = f.now.Add(messageHandedCap + time.Second)
	}
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageFailed || got.Attempts != messageMaxHandoffs {
		t.Fatalf("not bounded: %+v", got)
	}
}

func TestBeginAndCommitDedupeAndGateDelivery(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	params := AgentMessageParams{Kind: domain.AgentMessageTicketConcluded, Recipient: "architect:hiveryn", ArchitectKey: "hiveryn", DedupeKey: "k1", Body: "b"}
	first, err := f.service.BeginAgentMessage(context.Background(), params)
	if err != nil || first.State != domain.AgentMessagePendingCommit {
		t.Fatalf("begin = %+v %v", first, err)
	}
	again, err := f.service.BeginAgentMessage(context.Background(), params)
	if err != nil || again.ID != first.ID {
		t.Fatalf("second begin = %+v %v (want %s)", again, err, first.ID)
	}
	f.dispatch()
	if len(f.adapters[agentruntime.AgentCodex].deliveries()) != 0 {
		t.Fatal("pending_commit message was delivered")
	}
	if err := f.service.CommitAgentMessage(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.CommitAgentMessage(context.Background(), first.ID); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	f.dispatch()
	f.expect(first.ID, domain.AgentMessageHanded, "queued")
	if _, err := f.service.BeginAgentMessage(context.Background(), AgentMessageParams{Kind: "x", Recipient: "nobody", DedupeKey: "k", Body: "b"}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("invalid recipient err = %v", err)
	}
	f.mu.Lock()
	changed := 0
	for _, e := range f.events {
		if e.Reason == domain.ArchitectEventAgentMessageChanged && e.ArchitectKey == "hiveryn" {
			changed++
		}
	}
	f.mu.Unlock()
	if changed < 3 {
		t.Fatalf("agent_message_changed events = %d", changed)
	}
}

func TestSessionRecipientGoneFailsAndRemoteRecipientIsRetained(t *testing.T) {
	f := newMessageFixture(t)
	gone, err := f.service.EnqueueAgentMessage(context.Background(), AgentMessageParams{Kind: "note", Recipient: "session:missing", DedupeKey: "g", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	remoteSession := f.architectSession("remote-1", agentruntime.AgentClaude, "n")
	remoteSession.SessionType = domain.SessionTypeTicket
	remoteSession.Machine = "bk"
	f.repo.listedSessions = []domain.Session{remoteSession}
	remote, err := f.service.EnqueueAgentMessage(context.Background(), AgentMessageParams{Kind: "note", Recipient: "session:remote-1", DedupeKey: "r", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	if got := f.get(gone.ID); got.State != domain.AgentMessageFailed || !strings.Contains(got.LastError, "no longer exists") || got.EndedAt == nil {
		t.Fatalf("gone = %+v", got)
	}
	f.expect(remote.ID, domain.AgentMessagePending, "remote sessions is not available")
}

func TestHandedMessageWhoseTargetEndedGoesToTheNextSession(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	m := f.enqueue("moving")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "queued")
	// The architect concluded before the queue ran; its successor runs Claude.
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-2", agentruntime.AgentClaude, "native-2")}
	f.dispatch()
	moved := f.expect(m.ID, domain.AgentMessagePending, "no waiter armed")
	if !strings.Contains(moved.LastError, "target session ended") || moved.TargetSessionID != "arch-2" || moved.ProviderRef != "" || moved.TargetAgent != "claude" {
		t.Fatalf("moved = %+v", moved)
	}
}

func TestReconcilePendingCommitAgainstTheTicket(t *testing.T) {
	concludedAt := time.Date(2026, 10, 20, 11, 0, 0, 123456789, time.UTC)
	cases := []struct {
		name      string
		ticket    domain.Ticket
		err       error
		state     domain.AgentMessageState
		condition string
		lastError string
	}{
		{name: "done with this conclusion", ticket: domain.Ticket{TicketSummary: domain.TicketSummary{Status: domain.TicketStatusDone}, Conclusion: &domain.TicketConclusion{ConcludedAt: concludedAt}}, state: domain.AgentMessagePending},
		{name: "done with another conclusion", ticket: domain.Ticket{TicketSummary: domain.TicketSummary{Status: domain.TicketStatusDone}, Conclusion: &domain.TicketConclusion{ConcludedAt: concludedAt.Add(time.Hour)}}, state: domain.AgentMessageFailed, lastError: "superseded"},
		{name: "ticket gone", err: &domain.NotFoundError{Resource: "ticket", ID: "T-1"}, state: domain.AgentMessageFailed, lastError: "no longer exists"},
		{name: "conclusion written but not moved", ticket: domain.Ticket{TicketSummary: domain.TicketSummary{Status: domain.TicketStatusProgress}, Conclusion: &domain.TicketConclusion{ConcludedAt: concludedAt}}, state: domain.AgentMessagePendingCommit, condition: "partial conclusion write"},
		{name: "nothing persisted", ticket: domain.Ticket{TicketSummary: domain.TicketSummary{Status: domain.TicketStatusProgress}}, state: domain.AgentMessagePendingCommit, condition: "conclusion not persisted; the ticket is in progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMessageFixture(t)
			f.tickets.ticket, f.tickets.err = tc.ticket, tc.err
			session := domain.Session{ID: "w-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "T-1"}
			m, err := f.service.beginTicketConcludedMessage(context.Background(), session, domain.TicketConclusion{ConcludedAt: concludedAt, Outcome: domain.TicketOutcomeCompleted})
			if err != nil {
				t.Fatal(err)
			}
			if m.Body != "Ticket T-1 has been concluded. Outcome: completed. Use readTicket to review its conclusion." || m.Recipient != "architect:hiveryn" ||
				m.DedupeKey != "ticket_concluded:hiveryn:T-1:2026-10-20T11:00:00.123456789Z" || m.Payload["ticket_id"] != "T-1" {
				t.Fatalf("notice = %+v", m)
			}
			// A young row is left alone by the periodic pass, settled at startup.
			if err := f.service.reconcileAgentMessages(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			f.expect(m.ID, domain.AgentMessagePendingCommit, "waiting for the announced change")
			if err := f.service.reconcileAgentMessages(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			got := f.expect(m.ID, tc.state, tc.condition)
			if !strings.Contains(got.LastError, tc.lastError) {
				t.Fatalf("last error %q, want %q", got.LastError, tc.lastError)
			}
			if tc.state == domain.AgentMessagePending && got.CommittedAt == nil {
				t.Fatal("committed_at not set")
			}
		})
	}
}

func TestConcludeTicketSessionRecordsTheNoticeAroundTheConclusionWrite(t *testing.T) {
	f := newMessageFixture(t)
	architectPath := t.TempDir()
	repoPath := t.TempDir()
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	operations := []string{}
	f.repo.operations = &operations
	f.repo.createdSession = domain.Session{
		ID: "session-work", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-1", Prompt: "kickoff", Workdir: repoPath, CreatedAt: created,
		CurrentRun: &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning, Workdir: repoPath, StartedAt: &created, ProfileName: "codex"},
	}
	cfg := testRuntimeConfigWithPaths(architectPath, repoPath)
	cfg.Architects["hiveryn"] = config.ArchitectConfig{Path: architectPath, Repos: map[string]string{"daemon": repoPath}}
	f.service.cfg = cfg
	f.tickets.ticket = domain.Ticket{TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusProgress, Created: &created, Updated: &created}}
	f.service.terminal = &fakeTerminalManager{operations: &operations}
	f.service.eventStreams = map[string]map[uint64]chan domain.SessionEvent{}

	if _, err := f.service.ConcludeSession(context.Background(), "session-work", domain.ConcludeSessionParams{Body: "done", Outcome: domain.TicketOutcomeExploratory}); err != nil {
		t.Fatalf("ConcludeSession: %v", err)
	}
	listed, err := f.service.ListAgentMessages(context.Background(), "hiveryn")
	if err != nil || len(listed) != 1 {
		t.Fatalf("messages = %+v %v", listed, err)
	}
	notice := listed[0]
	if notice.State != domain.AgentMessagePending || notice.Kind != domain.AgentMessageTicketConcluded || notice.Recipient != "architect:hiveryn" ||
		notice.Body != "Ticket ticket-1 has been concluded. Outcome: exploratory. Use readTicket to review its conclusion." || notice.CommittedAt == nil {
		t.Fatalf("notice = %+v", notice)
	}
	if !strings.HasPrefix(notice.DedupeKey, "ticket_concluded:hiveryn:ticket-1:") {
		t.Fatalf("dedupe key %q", notice.DedupeKey)
	}

	// A conclusion that fails on the filesystem leaves the notice uncommitted,
	// never deliverable.
	g := newMessageFixture(t)
	g.repo.operations = &operations
	g.repo.createdSession = f.repo.createdSession
	g.repo.createdSession.CurrentRun = &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning, Workdir: repoPath, StartedAt: &created, ProfileName: "codex"}
	g.service.cfg = cfg
	g.tickets.ticket = f.tickets.ticket
	g.tickets.concludeErr = errors.New("disk full")
	g.service.terminal = &fakeTerminalManager{operations: &operations}
	g.service.eventStreams = map[string]map[uint64]chan domain.SessionEvent{}
	if _, err := g.service.ConcludeSession(context.Background(), "session-work", domain.ConcludeSessionParams{Body: "done", Outcome: domain.TicketOutcomeExploratory}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	listed, _ = g.service.ListAgentMessages(context.Background(), "hiveryn")
	if len(listed) != 1 || listed[0].State != domain.AgentMessagePendingCommit {
		t.Fatalf("uncommitted notice = %+v", listed)
	}
	g.dispatch()
	if g.get(listed[0].ID).State != domain.AgentMessagePendingCommit {
		t.Fatal("pending_commit notice progressed")
	}
}

func TestDispatcherLoopKicksAndStops(t *testing.T) {
	f := newMessageFixture(t)
	f.service.messages.tick = 20 * time.Millisecond
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	if err := f.service.StartMessageDelivery(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := f.enqueue("loop")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.get(m.ID).State == domain.AgentMessageHanded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.expect(m.ID, domain.AgentMessageHanded, "queued")
	f.service.stopMessageDelivery()
	select {
	case <-f.service.messages.done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not stop")
	}
}

func TestStaleRecordIsNotALossUntilItReachesPastTheHandoff(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	codex := f.adapters[agentruntime.AgentCodex]
	m := f.enqueue("stale")
	f.dispatch()
	handed := f.expect(m.ID, domain.AgentMessageHanded, "queued")
	// The lookup reads a record that ends hours before the hand-off (an old
	// rollout segment): the thread going idle proves nothing about the
	// message, so it is not redelivered.
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{RecordedThrough: handed.HandedAt.Add(-3 * time.Hour)})
	f.now = f.now.Add(time.Minute)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle, NativeSessionRole: agentruntime.NativeSessionRolePrimary})
	f.dispatch()
	if got := f.expect(m.ID, domain.AgentMessageHanded, "ends before the hand-off"); got.Attempts != 1 || len(codex.deliveries()) != 1 {
		t.Fatalf("stale = %+v deliveries %d", got, len(codex.deliveries()))
	}
	// Once the record reaches past the hand-off, the same idle makes the
	// absence authoritative: redelivered under the same id.
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{RecordedThrough: f.now})
	f.dispatch()
	if got := f.expect(m.ID, domain.AgentMessageHanded, "queued"); got.Attempts != 2 || len(codex.deliveries()) != 2 {
		t.Fatalf("redelivered = %+v", got)
	}
	// A record that stays stale for the handed cap is redelivered too,
	// visibly: the lookup may be reading the wrong file.
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{RecordedThrough: handed.HandedAt.Add(-3 * time.Hour)})
	f.now = f.now.Add(messageHandedCap + time.Second)
	f.dispatch()
	if got := f.expect(m.ID, domain.AgentMessageHanded, "queued"); got.Attempts != 3 {
		t.Fatalf("after cap = %+v", got)
	}
	f.now = f.now.Add(time.Second)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle, NativeSessionRole: agentruntime.NativeSessionRolePrimary})
	f.dispatch()
	if got := f.expect(m.ID, domain.AgentMessageHanded, "ends before the hand-off"); got.Attempts != 3 {
		t.Fatalf("stale again = %+v", got)
	}
	codex.setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true, Acknowledged: true, RecordedThrough: f.now})
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageAcknowledged {
		t.Fatalf("final = %+v", got)
	}
}

func TestUnconfirmedHandoffsAreBoundedAndFailVisibly(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	codex := f.adapters[agentruntime.AgentCodex]
	m := f.enqueue("vanishing")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "queued")
	// The record never shows it and the thread keeps going idle: redelivered
	// under the same id, but only up to the cap.
	for handoff := 2; handoff <= messageMaxHandoffs; handoff++ {
		f.now = f.now.Add(time.Minute)
		f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle, NativeSessionRole: agentruntime.NativeSessionRolePrimary})
		f.dispatch()
		if got := f.expect(m.ID, domain.AgentMessageHanded, "queued"); got.Attempts != handoff || len(codex.deliveries()) != handoff {
			t.Fatalf("hand-off %d = %+v deliveries %d", handoff, got, len(codex.deliveries()))
		}
	}
	f.now = f.now.Add(time.Minute)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle, NativeSessionRole: agentruntime.NativeSessionRolePrimary})
	f.dispatch()
	failed := f.get(m.ID)
	if failed.State != domain.AgentMessageFailed || failed.EndedAt == nil || failed.HandedAt != nil || failed.Attempts != messageMaxHandoffs || len(codex.deliveries()) != messageMaxHandoffs ||
		!strings.Contains(failed.LastError, "attempted 3 times without a confirmed receipt") || !strings.Contains(failed.LastError, "not found in the agent's record after its next turn ended") {
		t.Fatalf("failed = %+v deliveries %d", failed, len(codex.deliveries()))
	}
	// Terminal: later passes and idles leave it alone.
	f.now = f.now.Add(time.Hour)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle, NativeSessionRole: agentruntime.NativeSessionRolePrimary})
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageFailed || len(codex.deliveries()) != messageMaxHandoffs {
		t.Fatalf("after failure = %+v", got)
	}

}

func TestReceiptIsCheckedBeforeRedelivery(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentClaude, "native-arch-1")}
	claude := f.adapters[agentruntime.AgentClaude]
	m := f.enqueue("already there")
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.armWaiter("arch-1", "Stop", "")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "handed")
	// Lost by the next idle (the transcript could not show it), returned to
	// pending; no waiter is armed, so nothing is sent in this pass.
	f.now = f.now.Add(time.Minute)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "no waiter armed")
	// The record now shows it after all (it was flushed late, or became
	// readable): the next hand-off opportunity closes it instead of sending.
	claude.setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true, RecordedThrough: f.now})
	stop := f.armWaiter("arch-1", "Stop", "")
	f.dispatch()
	got := f.get(m.ID)
	if got.State != domain.AgentMessageAppeared || got.Attempts != 1 || got.AppearedAt == nil || got.Condition != "" || got.LastError != "" {
		t.Fatalf("closed before redelivery = %+v", got)
	}
	if _, armed := f.service.messages.mailbox.Armed("arch-1"); !armed {
		t.Fatal("the waiter was consumed although nothing was sent")
	}
	select {
	case resp := <-stop:
		t.Fatalf("waiter answered: %+v", resp)
	default:
	}
	// A record that still cannot be read does not block a bounded
	// redelivery.
	lost := f.enqueue("unreadable")
	f.dispatch()
	f.expect(lost.ID, domain.AgentMessageHanded, "handed")
	f.now = f.now.Add(time.Minute)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "arch-1", Status: agentruntime.StatusIdle})
	f.dispatch()
	f.expect(lost.ID, domain.AgentMessagePending, "no waiter armed")
	claude.confirmErr = errors.New("transcript not found")
	f.armWaiter("arch-1", "Stop", "")
	f.dispatch()
	if got := f.expect(lost.ID, domain.AgentMessageHanded, "handed"); got.Attempts != 2 {
		t.Fatalf("redelivered despite unreadable record = %+v", got)
	}
}

func TestHandedMessageFoundInTheEndedTargetRecordIsNotMoved(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	m := f.enqueue("ran before the end")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "queued")
	// The architect session ended, but its rollout on disk holds the receipt.
	f.adapters[agentruntime.AgentCodex].setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true, Acknowledged: true})
	f.repo.listedSessions = []domain.Session{f.architectSession("arch-2", agentruntime.AgentClaude, "native-2")}
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageAcknowledged || got.TargetSessionID != "arch-1" || got.Attempts != 1 {
		t.Fatalf("got = %+v", got)
	}
}

func TestRestartReconcilesOpenMessagesAgainstTheRecordBeforeSendingAgain(t *testing.T) {
	first := newMessageFixture(t)
	first.repo.listedSessions = []domain.Session{first.architectSession("arch-1", agentruntime.AgentCodex, "thread-1")}
	codex := first.adapters[agentruntime.AgentCodex]
	handed := first.enqueue("handed before restart")
	// Open messages are dispatched oldest first; same-instant rows tie on id.
	first.now = first.now.Add(time.Second)
	repended := first.enqueue("repended before restart")
	first.dispatch()
	first.expect(handed.ID, domain.AgentMessageHanded, "queued")
	first.expect(repended.ID, domain.AgentMessagePending, "waiting for the previous message")
	// The first appears; the second is handed, then lost at the cap and its
	// redelivery fails on transport, leaving a pending row with one hand-off.
	codex.setReceipt(handed.ID, agentruntime.MessageReceipt{Appeared: true})
	first.dispatch()
	first.expect(repended.ID, domain.AgentMessageHanded, "queued")
	first.now = first.now.Add(messageHandedCap + time.Second)
	codex.deliverErr = errors.New("app-server down")
	first.dispatch()
	if got := first.expect(repended.ID, domain.AgentMessageHanded, "acceptance unknown"); got.Attempts != 2 {
		t.Fatalf("before restart = %+v", got)
	}
	// Both appeared in the record the old daemon could not read: a third
	// message is handed and still open at restart.
	third := first.enqueue("open at restart")
	codex.deliverErr = nil
	first.now = first.now.Add(messageRetryBackoff)
	// The repended one is sent again first (its record still lacks it), then
	// the third waits behind it.
	first.dispatch()
	first.expect(repended.ID, domain.AgentMessageHanded, "awaiting")
	first.expect(third.ID, domain.AgentMessagePending, "waiting for the previous message")

	// Restart: a new daemon over the same database, whose lookup now finds
	// the receipts. Nothing is sent again.
	second := newMessageFixtureAt(t, first.dbPath)
	second.now = first.now.Add(time.Minute)
	second.repo.listedSessions = first.repo.listedSessions
	restarted := second.adapters[agentruntime.AgentCodex]
	restarted.setReceipt(repended.ID, agentruntime.MessageReceipt{Appeared: true, Acknowledged: true, RecordedThrough: second.now})
	if err := second.service.StartMessageDelivery(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.service.stopMessageDelivery)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := second.get(repended.ID); got.State == domain.AgentMessageAcknowledged {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := second.get(repended.ID); got.State != domain.AgentMessageAcknowledged || got.Attempts != 2 {
		t.Fatalf("reconciled = %+v", got)
	}
	if got := second.get(handed.ID); got.State != domain.AgentMessageAppeared {
		t.Fatalf("appeared before restart = %+v", got)
	}
	// The third is now delivered once, through the restarted dispatcher.
	for time.Now().Before(deadline) {
		if got := second.get(third.ID); got.State == domain.AgentMessageHanded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := second.get(third.ID); got.State != domain.AgentMessageHanded || got.Attempts != 1 || len(restarted.deliveries()) != 1 || restarted.deliveries()[0].MessageID != third.ID {
		t.Fatalf("third = %+v deliveries %+v", got, restarted.deliveries())
	}
}

// failingMessageWrites injects storage failures at exact side-effect boundaries.
type failingMessageWrites struct {
	domain.AgentMessageRepository
	updates    int
	failUpdate func(domain.AgentMessage, int) bool
	failCreate bool
}

func (r *failingMessageWrites) UpdateAgentMessage(ctx context.Context, m domain.AgentMessage) error {
	r.updates++
	if r.failUpdate != nil && r.failUpdate(m, r.updates) {
		return errors.New("injected outbox write failure")
	}
	return r.AgentMessageRepository.UpdateAgentMessage(ctx, m)
}
func (r *failingMessageWrites) CreateAgentMessage(ctx context.Context, m domain.AgentMessage) error {
	if r.failCreate {
		return errors.New("injected outbox insert failure")
	}
	return r.AgentMessageRepository.CreateAgentMessage(ctx, m)
}

func TestAttemptMustBeDurableBeforeSend(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch", agentruntime.AgentCodex, "thread")}
	m := f.enqueue("reserve failure")
	r := &failingMessageWrites{AgentMessageRepository: f.messages, failUpdate: func(m domain.AgentMessage, _ int) bool { return m.State == domain.AgentMessageHanded }}
	f.service.messages.repo = r
	f.dispatch()
	if len(f.adapters[agentruntime.AgentCodex].deliveries()) != 0 {
		t.Fatal("sent without durable reservation")
	}
	if got := f.get(m.ID); got.Attempts != 0 || !strings.Contains(got.LastError, "injected") {
		t.Fatalf("not recoverable: %+v", got)
	}
	r.failUpdate = nil
	f.now = f.now.Add(time.Minute)
	f.dispatch()
	if got := f.get(m.ID); got.Attempts != 1 || len(f.adapters[agentruntime.AgentCodex].deliveries()) != 1 {
		t.Fatalf("not recovered: %+v", got)
	}
}

func TestAcceptedSendWriteFailureAndLostResponseSurviveRestart(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		t.Run(fmt.Sprint(lostResponse), func(t *testing.T) {
			f := newMessageFixture(t)
			f.repo.listedSessions = []domain.Session{f.architectSession("arch", agentruntime.AgentCodex, "thread")}
			m := f.enqueue("accepted")
			// First write reserves; all post-send writes fail, including receipt storage.
			f.service.messages.repo = &failingMessageWrites{AgentMessageRepository: f.messages, failUpdate: func(_ domain.AgentMessage, n int) bool { return n > 1 }}
			adapter := f.adapters[agentruntime.AgentCodex]
			if lostResponse {
				adapter.acceptedOnError = true
				adapter.deliverErr = errors.New("accepted but response lost")
			}
			f.dispatch()
			durable := f.get(m.ID)
			if durable.State != domain.AgentMessageHanded || durable.Attempts != 1 || durable.TargetNativeID != "thread" || durable.HandedAt == nil {
				t.Fatalf("missing attempt: %+v", durable)
			}
			restarted := newMessageFixtureAt(t, f.dbPath)
			restarted.now = f.now.Add(time.Hour)
			restarted.repo.listedSessions = f.repo.listedSessions
			restarted.adapters[agentruntime.AgentCodex].setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true})
			restarted.dispatch()
			if got := restarted.get(m.ID); got.State != domain.AgentMessageAppeared || got.Attempts != 1 {
				t.Fatalf("receipt not recovered: %+v", got)
			}
			if len(restarted.adapters[agentruntime.AgentCodex].deliveries()) != 0 {
				t.Fatal("resent accepted message")
			}
		})
	}
}

func TestConfirmedReceiptWriteFailureNeverResendsInProcess(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch", agentruntime.AgentCodex, "thread")}
	m := f.enqueue("receipt write failure")
	adapter := f.adapters[agentruntime.AgentCodex]
	f.dispatch()
	adapter.setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true})
	writes := &failingMessageWrites{AgentMessageRepository: f.messages, failUpdate: func(_ domain.AgentMessage, _ int) bool { return true }}
	f.service.messages.repo = writes
	f.dispatch()
	adapter.confirmErr = errors.New("record now unavailable")
	f.now = f.now.Add(time.Hour)
	if err := f.service.dispatchAgentMessages(context.Background()); err == nil {
		t.Fatal("failed flush should stop dispatch")
	}
	writes.failUpdate = nil
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageAppeared || len(adapter.deliveries()) != 1 {
		t.Fatalf("receipt forgotten: %+v", got)
	}
}

func TestAmbiguousIdleGatedSendRecoversWithoutAnotherIdleEvent(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch", agentruntime.AgentOpenCode, "native")}
	f.service.setMessageIdle("arch", true)
	f.service.messages.endpoints["arch"] = messageEndpoint{port: 1234, password: "fixture"}
	adapter := f.adapters[agentruntime.AgentOpenCode]
	adapter.deliverErr = errors.New("response lost")
	m := f.enqueue("idle gated ambiguity")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "acceptance unknown")
	// If the send never arrived there is no new idle hook. Absence still needs
	// bounded recovery, without interrupting a busy recipient.
	f.now = f.now.Add(messageHandedCap + time.Second)
	adapter.deliverErr = nil
	f.service.setMessageIdle("arch", false)
	f.dispatch()
	if got := f.expect(m.ID, domain.AgentMessagePending, "agent busy"); got.Attempts != 1 {
		t.Fatalf("busy retry: %+v", got)
	}
	f.service.setMessageIdle("arch", true)
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageHanded || got.Attempts != 2 || len(adapter.deliveries()) != 1 {
		t.Fatalf("not recovered: %+v", got)
	}
}

type preparedMessageAdapter struct {
	*fakeMessageAdapter
	beforeSend func(agentruntime.DeliverRequest)
}

func (a *preparedMessageAdapter) PrepareMessage(context.Context, agentruntime.DeliverRequest) (string, error) {
	return "prepared-provider-id", nil
}
func (a *preparedMessageAdapter) DeliverMessage(ctx context.Context, req agentruntime.DeliverRequest) (agentruntime.DeliverResult, error) {
	a.beforeSend(req)
	return a.fakeMessageAdapter.DeliverMessage(ctx, req)
}

func TestPreparedProviderIdentityIsPersistedBeforeOpenCodeSend(t *testing.T) {
	f := newMessageFixture(t)
	f.repo.listedSessions = []domain.Session{f.architectSession("arch", agentruntime.AgentOpenCode, "native")}
	f.service.setMessageIdle("arch", true)
	f.service.messages.endpoints["arch"] = messageEndpoint{port: 1234, password: "fixture"}
	m := f.enqueue("prepared identity")
	adapter := f.adapters[agentruntime.AgentOpenCode]
	adapter.deliverErr = errors.New("accepted, response lost")
	adapter.acceptedOnError = true
	f.service.adapters[agentruntime.AgentOpenCode] = &preparedMessageAdapter{fakeMessageAdapter: adapter, beforeSend: func(req agentruntime.DeliverRequest) {
		durable := f.get(m.ID)
		if durable.ProviderRef != "prepared-provider-id" || req.ProviderRef != durable.ProviderRef || durable.Attempts != 1 || durable.State != domain.AgentMessageHanded {
			t.Fatalf("send preceded reservation: %+v request %+v", durable, req)
		}
	}}
	f.dispatch()
	restarted := newMessageFixtureAt(t, f.dbPath)
	restarted.repo.listedSessions = f.repo.listedSessions
	reader := restarted.adapters[agentruntime.AgentOpenCode]
	reader.setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true})
	restarted.dispatch()
	if got := restarted.get(m.ID); got.State != domain.AgentMessageAppeared || len(reader.confirmed) != 1 || reader.confirmed[0].ProviderRef != "prepared-provider-id" || len(reader.deliveries()) != 0 {
		t.Fatalf("restart lost identity: %+v", got)
	}
}
