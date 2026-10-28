package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// terminalTestRepo keeps the fake session repository and adds real SQLite
// conversation persistence (submissions, reservations, the version feed).
type terminalTestRepo struct {
	*fakeSessionRepository
	domain.ConversationRepository
	domain.TerminalConversationRepository
}

// historyAdapter is a message adapter whose native record the test writes.
type historyAdapter struct {
	*fakeMessageAdapter
	mu      sync.Mutex
	history agentruntime.History
	err     error
	reads   []agentruntime.HistoryRequest
}

func (h *historyAdapter) ReadHistory(_ context.Context, req agentruntime.HistoryRequest) (agentruntime.History, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, req)
	return h.history, h.err
}

// PrepareMessage chooses an OpenCode-style provider identity.
func (h *historyAdapter) PrepareMessage(_ context.Context, req agentruntime.DeliverRequest) (string, error) {
	return "msg_" + req.MessageID, nil
}

func (h *historyAdapter) record(items ...agentruntime.HistoryItem) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history.Items = append(h.history.Items, items...)
	for _, item := range items {
		if item.At.After(h.history.RecordedThrough) {
			h.history.RecordedThrough = item.At
		}
	}
}

// holdRecorder records input holds on the live main terminal.
type holdRecorder struct {
	*fakeTerminalManager
	mu       sync.Mutex
	held     []string
	released int
}

func (h *holdRecorder) HoldInput(sessionID, terminalID string, _ time.Duration) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = append(h.held, sessionID+":"+terminalID)
	return func() { h.mu.Lock(); h.released++; h.mu.Unlock() }
}

type terminalFixture struct {
	*messageFixture
	history map[agentruntime.AgentKind]*historyAdapter
}

func newTerminalFixture(t *testing.T) *terminalFixture {
	t.Helper()
	f := newMessageFixture(t)
	interval := terminalHistoryMinInterval
	terminalHistoryMinInterval = 0
	t.Cleanup(func() { terminalHistoryMinInterval = interval })
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "conversation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conv := store.NewSessionStore(db)
	f.service.repo = terminalTestRepo{fakeSessionRepository: f.repo, ConversationRepository: conv, TerminalConversationRepository: conv}
	tf := &terminalFixture{messageFixture: f, history: map[agentruntime.AgentKind]*historyAdapter{}}
	for kind, adapter := range f.adapters {
		h := &historyAdapter{fakeMessageAdapter: adapter}
		tf.history[kind] = h
		f.service.adapters[kind] = h
	}
	return tf
}

func (f *terminalFixture) terminalSession(id string, kind agentruntime.AgentKind) domain.Session {
	s := f.architectSession(id, kind, "native-"+id)
	s.InteractionMode = domain.InteractionTerminal
	s.Prompt = "# Project: hiveryn\n\nKICKOFF"
	s.CurrentRun.MainTerminalID = "term-" + id
	f.setMainTerminal(id, "term-"+id)
	return s
}

// setMainTerminal records the live main terminal, held only in memory.
func (f *terminalFixture) setMainTerminal(sessionID, terminalID string) {
	f.service.terminalStateMu.Lock()
	defer f.service.terminalStateMu.Unlock()
	if f.service.terminalStates == nil {
		f.service.terminalStates = map[string]sessionTerminalState{}
	}
	state := f.service.terminalStates[sessionID]
	state.mainTerminalID = terminalID
	f.service.terminalStates[sessionID] = state
}

func (f *terminalFixture) submission(sessionID, id string) domain.Submission {
	f.t.Helper()
	repo, err := f.service.conversationRepo()
	if err != nil {
		f.t.Fatalf("conversation repo: %v", err)
	}
	sub, err := repo.GetSubmission(context.Background(), sessionID, id)
	if err != nil {
		f.t.Fatalf("get submission: %v", err)
	}
	return sub
}

func (f *terminalFixture) history2(sessionID string) []domain.ConversationItem {
	f.t.Helper()
	page, err := f.service.ReadConversation(context.Background(), sessionID, domain.ConversationQuery{Limit: 200})
	if err != nil {
		f.t.Fatalf("read conversation: %v", err)
	}
	latest := map[string]domain.ConversationItem{}
	var order []string
	for _, item := range page.Items {
		if _, ok := latest[item.ID]; !ok {
			order = append(order, item.ID)
		}
		latest[item.ID] = item
	}
	out := make([]domain.ConversationItem, 0, len(order))
	for _, id := range order {
		out = append(out, latest[id])
	}
	return out
}

func TestClassifyTerminalInputKeepsProvenanceAndTheRecordedKickoff(t *testing.T) {
	session := domain.Session{Prompt: "  # Project: lab\n\nKICKOFF  "}
	subs := map[string]domain.Submission{
		"sub-claude": {ID: "sub-claude", Text: "hello from the phone"},
		"sub-codex":  {ID: "sub-codex", Text: "codex input"},
		"ref:msg_01": {ID: "sub-oc", Text: "opencode input"},
	}
	cases := []struct {
		name string
		item agentruntime.HistoryItem
		want terminalInput
	}{
		{"kickoff with injected context", agentruntime.HistoryItem{Text: "# Project: lab\n\nKICKOFF\n\n<document>overview</document>"}, terminalInput{via: "kickoff", text: "# Project: lab\n\nKICKOFF\n\n<document>overview</document>"}},
		{"kickoff whose injected context quotes a notification", agentruntime.HistoryItem{Text: "# Project: lab\n\nKICKOFF\n\nHiveryn message m-0: quoted"}, terminalInput{via: "kickoff", text: "# Project: lab\n\nKICKOFF\n\nHiveryn message m-0: quoted"}},
		{"text merely containing the kickoff is not the kickoff", agentruntime.HistoryItem{Text: "again: # Project: lab\n\nKICKOFF"}, terminalInput{via: "terminal", text: "again: # Project: lab\n\nKICKOFF"}},
		{"claude rewake behind Claude's own label", agentruntime.HistoryItem{Text: `Stop hook blocking error from command "Stop": ` + claudeInputHeader + "sub-claude — a message from the user ...:\n\nhello from the phone", NativeOrigin: "task_notification"}, terminalInput{via: "api", submission: "sub-claude", text: "hello from the phone"}},
		{"unknown claude header stays terminal text", agentruntime.HistoryItem{Text: claudeInputHeader + "sub-other — x"}, terminalInput{via: "terminal", text: claudeInputHeader + "sub-other — x"}},
		{"codex client id", agentruntime.HistoryItem{Ref: "sub-codex", Text: "codex input"}, terminalInput{via: "api", submission: "sub-codex", text: "codex input"}},
		{"opencode provider ref", agentruntime.HistoryItem{Ref: "msg_01", Text: "opencode input"}, terminalInput{via: "api", submission: "sub-oc", text: "opencode input"}},
		{"outbox notification", agentruntime.HistoryItem{Text: "Hiveryn message m-1: Ticket T has been concluded."}, terminalInput{via: "notification", text: "Hiveryn message m-1: Ticket T has been concluded."}},
		{"claude rewake notification", agentruntime.HistoryItem{Text: `Stop hook blocking error from command "Stop": Hiveryn message m-2: Ticket T has been concluded.`, NativeOrigin: "task_notification"}, terminalInput{via: "notification", text: "Hiveryn message m-2: Ticket T has been concluded."}},
		{"typed on the laptop", agentruntime.HistoryItem{Ref: "tui-client-id", Text: "laptop text"}, terminalInput{via: "terminal", text: "laptop text"}},
	}
	for _, tc := range cases {
		if got := classifyTerminalInput(session, tc.item, subs); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// expectRefused asserts a send was answered with a delivery error code and a
// durable receipt in the given status.
func expectRefused(t *testing.T, sub domain.Submission, err error, code, status string) domain.Submission {
	t.Helper()
	var refused *domain.DeliveryError
	if !errors.As(err, &refused) || refused.Code != code || refused.Submission == nil || refused.Submission.Status != status || sub.Status != status {
		t.Fatalf("send = %+v, %v; want %s with a %s receipt", sub, err, code, status)
	}
	return *refused.Submission
}

func TestCodexTerminalSubmissionQueuesNativelyAndCompletesFromItsTurn(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-1", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	caps := f.service.chatCapabilities(session)
	if !caps.History || !caps.Send || caps.InputRoute != "codex_thread_queue" {
		t.Fatalf("capabilities = %+v", caps)
	}
	at := f.now
	codex := f.history[agentruntime.AgentCodex]
	codex.record(
		agentruntime.HistoryItem{ID: "um0", TurnID: "t0", Role: "user", NativeOrigin: "user", Text: "# Project: hiveryn\n\nKICKOFF\n\ncontext", At: at},
		agentruntime.HistoryItem{ID: "am0", TurnID: "t0", Role: "assistant", Text: "READY", Status: "completed", At: at},
		agentruntime.HistoryItem{ID: "turn:t0", TurnID: "t0", Role: "system", Status: "completed", TurnEnd: true, At: at},
		agentruntime.HistoryItem{ID: "um1", TurnID: "t1", Role: "user", NativeOrigin: "user", Ref: "tui-1", Text: "remember PLUM", At: at},
	)

	// The agent is busy with a laptop turn: Codex's own queue still takes it,
	// and the send answers with that acceptance.
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r-1", Text: "what did I say?"})
	if err != nil || sub.Status != domain.SubmissionAccepted || sub.AcceptedAt == nil || sub.Acceptance != "codex_thread_queue" || !strings.Contains(sub.Error, "Codex's durable thread queue") {
		t.Fatalf("submit = %+v, %v", sub, err)
	}
	deliveries := codex.deliveries()
	if len(deliveries) != 1 || deliveries[0].MessageID != sub.ID || deliveries[0].Text != "what did I say?" || deliveries[0].NativeSessionID != "native-w-1" {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	if replay, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r-1", Text: "what did I say?"}); err != nil || replay.ID != sub.ID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	var conflict *domain.ConflictError
	if _, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r-1", Text: "different"}); !errors.As(err, &conflict) {
		t.Fatalf("different text under the same request id = %v, want conflict", err)
	}
	// Still queued natively: nothing is handed over again.
	codex.setReceipt(sub.ID, agentruntime.MessageReceipt{Queued: true})
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != domain.SubmissionAccepted || !strings.Contains(got.Error, "Queued natively") {
		t.Fatalf("queued = %+v", got)
	}
	// The thread runs it as its own queue turn and completes.
	codex.record(
		agentruntime.HistoryItem{ID: "um2", TurnID: "t2", Role: "user", NativeOrigin: "queue", Ref: sub.ID, Text: "what did I say?", At: at},
		agentruntime.HistoryItem{ID: "am2", TurnID: "t2", Role: "assistant", Text: "PLUM", Status: "completed", At: at},
		agentruntime.HistoryItem{ID: "turn:t2", TurnID: "t2", Role: "system", Status: "completed", TurnEnd: true, At: at},
	)
	f.dispatch()
	done := f.submission(session.ID, sub.ID)
	if done.Status != domain.SubmissionCompleted || done.NativeTurnID != "t2" || done.Error != "" || done.Acceptance != "codex_thread_queue" {
		t.Fatalf("completed = %+v", done)
	}
	if len(codex.deliveries()) != 1 {
		t.Fatalf("an input must be handed over once: %+v", codex.deliveries())
	}

	var got []string
	for _, item := range f.history2(session.ID) {
		got = append(got, strings.Join([]string{item.ID, item.Role, item.Origin, item.Via, item.NativeOrigin, item.SubmissionID, item.Text}, "|"))
	}
	// The recorded kickoff is shown verbatim (injected context included) with
	// kickoff provenance on its whole turn.
	want := []string{
		"um0|user|kickoff|kickoff|user||# Project: hiveryn\n\nKICKOFF\n\ncontext",
		"am0|assistant|kickoff||||READY",
		"turn:t0|system|kickoff||||",
		"um1|user|user|terminal|user||remember PLUM",
		"um2|user|user|api|queue|" + sub.ID + "|what did I say?",
		"am2|assistant|user|||" + sub.ID + "|PLUM",
		"turn:t2|system|user|||" + sub.ID + "|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCodexRefusalIsRejectedAndAmbiguousHandOffIsUnknownNeverResent(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-2", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	codex := f.history[agentruntime.AgentCodex]
	codex.record(agentruntime.HistoryItem{ID: "um1", TurnID: "t1", Role: "user", Text: "laptop", At: f.now})

	// The provider refused it: definitive, and a later idle never sends it.
	codex.deliverErr = agentruntime.NotDelivered(errors.New("thread not found (code -32600)"))
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "refused", Text: "refused"})
	rejected := expectRefused(t, sub, err, domain.DeliveryRejected, domain.SubmissionRejected)
	if !strings.Contains(rejected.Error, "thread not found") {
		t.Fatalf("original context lost: %+v", rejected)
	}

	// Accepted without a response: unknown, reconciled, never resent.
	codex.deliverErr, codex.acceptedOnError = errors.New("app-server exited before answering"), true
	sub, err = f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r", Text: "lost?"})
	unknown := expectRefused(t, sub, err, domain.DeliveryUnknown, domain.SubmissionUnknown)
	codex.deliverErr, codex.acceptedOnError = nil, false
	// The same request (a lost HTTP response retried) reports it again and
	// sends nothing.
	if again, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r", Text: "lost?"}); err == nil || again.ID != unknown.ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	// Codex's queue holds it: accepted after all.
	codex.setReceipt(unknown.ID, agentruntime.MessageReceipt{Queued: true})
	f.dispatch()
	if got := f.submission(session.ID, unknown.ID); got.Status != domain.SubmissionAccepted || got.AcceptedAt == nil || got.ErrorCode != "" {
		t.Fatalf("reconciled = %+v", got)
	}
	// Then it vanishes from the queue and never reaches the record while
	// the agent goes idle well after: uncertain, and never handed over again.
	codex.setReceipt(unknown.ID, agentruntime.MessageReceipt{})
	f.now = f.now.Add(30 * time.Second)
	codex.record(agentruntime.HistoryItem{ID: "am9", TurnID: "t9", Role: "assistant", Text: "other", At: f.now})
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	f.dispatch()
	if got := f.submission(session.ID, unknown.ID); got.Status != domain.SubmissionUncertain || !strings.Contains(got.Error, "not resent") {
		t.Fatalf("lost = %+v", got)
	}
	f.dispatch()
	if n := len(codex.deliveries()); n != 1 {
		t.Fatalf("deliveries = %d, want 1", n)
	}
	// A later input is not held behind the uncertain one: the laptop is not either.
	if next, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r2", Text: "next"}); err != nil || next.Status != domain.SubmissionAccepted {
		t.Fatalf("next = %+v, %v", next, err)
	}
}

func TestClaudeTerminalSubmissionIsAcceptedFromItsRecordOrRefusedWhileBusy(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("a-1", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	claude := f.history[agentruntime.AgentClaude]

	// Busy (the kickoff turn runs; its startup waiter is armed): refused now.
	f.armWaiter(session.ID, "SessionStart", "startup")
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "busy", Text: "too early"})
	busy := expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected)
	// Idle without a waiter: the route is unavailable.
	f.service.messages.mailbox.Close()
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	sub, err = f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "nowaiter", Text: "no waiter"})
	expectRefused(t, sub, err, domain.DeliverySendUnavailable, domain.SubmissionRejected)

	// Idle with a waiter: handed over, and accepted once Claude's record has it.
	stop := f.armWaiter(session.ID, "Stop", "")
	result := make(chan domain.Submission, 1)
	go func() {
		sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "phone-1", Text: "hello from the phone"})
		if err != nil {
			t.Errorf("send: %v", err)
		}
		result <- sub
	}()
	_, body := waiterResponse(t, stop)
	if !strings.Contains(body, claudeInputHeader) || !strings.Contains(body, "hello from the phone") || strings.Contains(body, "too early") {
		t.Fatalf("waiter text = %s", body)
	}
	id := domain.SubmissionIdentity(session.ID, "phone-1")
	at := f.now.Add(time.Second)
	claude.record(agentruntime.HistoryItem{ID: "u4", TurnID: "p4", Role: "user", NativeOrigin: "task_notification", Text: `Stop hook blocking error from command "Stop": ` + claudeInputHeader + id + " — a message from the user:\n\nhello from the phone", At: at})
	accepted := <-result
	if accepted.Status != domain.SubmissionRunning || accepted.Acceptance != "claude_native_record" || accepted.NativeTurnID != "p4" || accepted.AcceptedAt == nil {
		t.Fatalf("accepted = %+v", accepted)
	}
	claude.record(
		agentruntime.HistoryItem{ID: "x4:0", TurnID: "p4", Role: "assistant", Text: "hi", Status: "completed", At: at},
		agentruntime.HistoryItem{ID: "s4", TurnID: "p4", Role: "system", Status: "completed", TurnEnd: true, At: at},
	)
	f.dispatch()
	if got := f.submission(session.ID, accepted.ID); got.Status != domain.SubmissionCompleted || got.NativeTurnID != "p4" {
		t.Fatalf("completed = %+v", got)
	}
	items := f.history2(session.ID)
	if len(items) != 3 || items[0].Via != "api" || items[0].NativeOrigin != "task_notification" || items[0].Text != "hello from the phone" || items[0].SubmissionID != accepted.ID {
		t.Fatalf("history = %+v", items)
	}
	if len(claude.reads) == 0 || claude.reads[0].TranscriptHandle != "/transcripts/claude" {
		t.Fatalf("history requests = %+v", claude.reads)
	}
	// The earlier busy refusal never reached a later idle waiter.
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	f.armWaiter(session.ID, "Stop", "")
	f.dispatch()
	if got := f.submission(session.ID, busy.ID); got.Status != domain.SubmissionRejected {
		t.Fatalf("busy refusal revived: %+v", got)
	}
	if _, armed := f.service.messages.mailbox.Armed(session.ID); !armed {
		t.Fatal("a rejected send took the next waiter")
	}
}

func TestClaudeHandOffWithoutRecordIsUnknownAndConcurrentDuplicatesShareIt(t *testing.T) {
	bound := submitDeliveryBound
	submitDeliveryBound = 800 * time.Millisecond
	t.Cleanup(func() { submitDeliveryBound = bound })
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("a-3", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	claude := f.history[agentruntime.AgentClaude]
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	stop := f.armWaiter(session.ID, "Stop", "")

	// The same request posted concurrently (a reconnecting client) attaches to
	// the one attempt: one hand-off, one outcome.
	type outcome struct {
		sub domain.Submission
		err error
	}
	results := make(chan outcome, 3)
	for range 3 {
		go func() {
			sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "dup", Text: "once"})
			results <- outcome{sub, err}
		}()
	}
	_, body := waiterResponse(t, stop)
	if !strings.Contains(body, "once") {
		t.Fatalf("waiter text = %s", body)
	}
	var id string
	for range 3 {
		r := <-results
		got := expectRefused(t, r.sub, r.err, domain.DeliveryUnknown, domain.SubmissionUnknown)
		if id != "" && got.ID != id {
			t.Fatal("duplicate submissions")
		}
		id = got.ID
	}
	// Late native evidence settles it; nothing is ever handed over again.
	at := f.now.Add(time.Second)
	claude.record(
		agentruntime.HistoryItem{ID: "u5", TurnID: "p5", Role: "user", NativeOrigin: "task_notification", Text: claudeInputHeader + id + " — x:\n\nonce", At: at},
		agentruntime.HistoryItem{ID: "s5", TurnID: "p5", Role: "system", Status: "completed", TurnEnd: true, At: at},
	)
	f.dispatch()
	if got := f.submission(session.ID, id); got.Status != domain.SubmissionCompleted || got.AcceptedAt == nil || got.Acceptance != "native_record" || got.ErrorCode != "" {
		t.Fatalf("reconciled = %+v", got)
	}
}

func TestHandOffSlotIsSharedWithTheOutboxAndTeardownWaitsForIt(t *testing.T) {
	bound := submitDeliveryBound
	submitDeliveryBound = 800 * time.Millisecond
	t.Cleanup(func() { submitDeliveryBound = bound })
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("a-4", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})

	// A notification hand-off in progress: the send is busy, not queued.
	release, ok := f.service.tryHandoff(session.ID)
	if !ok {
		t.Fatal("slot taken")
	}
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "during-notice", Text: "x"})
	expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected)
	// ...and the outbox does not hand over into a slot it does not hold.
	m := f.enqueue("Ticket T has been concluded.")
	stop := f.armWaiter(session.ID, "Stop", "")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "conversation input is being handed")
	release()

	// A notification handed over but not yet in the record still owns
	// Claude's idle slot.
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "waiter")
	waiterResponse(t, stop)
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	f.armWaiter(session.ID, "Stop", "")
	sub, err = f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "after-notice", Text: "y"})
	if got := expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected); !strings.Contains(got.Error, "notification") {
		t.Fatalf("busy reason = %+v", got)
	}
	f.history[agentruntime.AgentClaude].record(agentruntime.HistoryItem{ID: "n", TurnID: "pn", Role: "user", Text: messageText(m), At: f.now})
	f.history[agentruntime.AgentClaude].setReceipt(m.ID, agentruntime.MessageReceipt{Appeared: true})
	f.dispatch()

	// A send in flight holds the slot: the outbox waits for it, and teardown
	// settles only after it finished.
	result := make(chan error, 1)
	go func() {
		_, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "inflight", Text: "z"})
		result <- err
	}()
	waitFor(t, func() bool {
		f.service.deliveries.mu.Lock()
		defer f.service.deliveries.mu.Unlock()
		return f.service.deliveries.handoffs[session.ID] != nil
	})
	next := f.enqueue("Another notice.")
	f.dispatch()
	if got := f.expect(next.ID, domain.AgentMessagePending, ""); got.Attempts != 0 {
		t.Fatalf("notification handed during a send: %+v", got)
	}
	if err := f.service.archiveChat(ctx, session, "concluded"); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Fatal("unconfirmed send reported success")
	}
	id := domain.SubmissionIdentity(session.ID, "inflight")
	if got := f.submission(session.ID, id); got.Status != domain.SubmissionUncertain || got.AcceptedAt != nil {
		t.Fatalf("in-flight send at session end = %+v", got)
	}
}

func TestMergedOpenCodeTurnIsReportedNotCompleted(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-3", agentruntime.AgentOpenCode)
	f.repo.listedSessions = []domain.Session{session}
	if caps := f.service.chatCapabilities(session); caps.Send {
		t.Fatalf("an OpenCode session without a known server endpoint must not advertise send: %+v", caps)
	}
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "noserver", Text: "x"})
	expectRefused(t, sub, err, domain.DeliverySendUnavailable, domain.SubmissionRejected)
	f.service.messages.endpoints[session.ID] = messageEndpoint{port: 1, password: "p"}
	holds := &holdRecorder{fakeTerminalManager: &fakeTerminalManager{}}
	f.service.terminal = holds
	oc := f.history[agentruntime.AgentOpenCode]
	sub, err = f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "busy", Text: "run the long task"})
	expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected)
	if len(oc.deliveries()) != 0 {
		t.Fatal("a busy send reached the provider")
	}
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	sub, err = f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r", Text: "run the long task"})
	if err != nil || sub.Status != domain.SubmissionAccepted || sub.Acceptance != "opencode_prompt_async" {
		t.Fatalf("idle = %+v, %v", sub, err)
	}
	if len(holds.held) != 1 || holds.held[0] != session.ID+":term-"+session.ID || holds.released != 0 {
		t.Fatalf("hand-off must hold laptop input on the live main terminal until the agent works: %+v released %d", holds.held, holds.released)
	}
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusWorking})
	if holds.released != 1 {
		t.Fatalf("working status must release the hold: %d", holds.released)
	}
	ref := oc.deliveries()[0].ProviderRef
	// Laptop input joined the running loop: the final reply belongs to it.
	oc.record(
		agentruntime.HistoryItem{ID: ref, TurnID: ref, Role: "user", NativeOrigin: "prompt", Ref: ref, Text: "run the long task", At: f.now},
		agentruntime.HistoryItem{ID: "msg_l", TurnID: "msg_l", Role: "user", NativeOrigin: "prompt", Ref: "msg_l", Text: "laptop meanwhile", At: f.now},
		agentruntime.HistoryItem{ID: "p", TurnID: "msg_l", Role: "assistant", Text: "both done", At: f.now},
		agentruntime.HistoryItem{ID: "turn:msg_l", TurnID: "msg_l", Role: "system", Status: "completed", TurnEnd: true, At: f.now},
	)
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != domain.SubmissionUncertain || !strings.Contains(got.Error, "merged later input") || got.AcceptedAt == nil {
		t.Fatalf("merged = %+v", got)
	}
}

func TestTerminalHistoryErrorIsNeverAnEmptyPage(t *testing.T) {
	f := newTerminalFixture(t)
	session := f.terminalSession("a-2", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	f.history[agentruntime.AgentClaude].err = errors.New("transcript unreadable")
	if _, err := f.service.ReadConversation(context.Background(), session.ID, domain.ConversationQuery{}); err == nil || !strings.Contains(err.Error(), "transcript unreadable") {
		t.Fatalf("read = %v, want the record error", err)
	}
	session.CurrentRun.NativeID = ""
	f.repo.listedSessions = []domain.Session{session}
	if caps := f.service.chatCapabilities(session); caps.History || caps.Send {
		t.Fatalf("no native identity must advertise nothing: %+v", caps)
	}
	session.Machine = "remote-box"
	if caps := f.service.chatCapabilities(session); caps.ExecutionLocation != "remote" || caps.Send {
		t.Fatalf("remote = %+v", caps)
	}
}

func TestTerminalSessionEndSettlesOpenSubmissionsAndRetainsHistory(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-4", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	codex := f.history[agentruntime.AgentCodex]
	codex.record(agentruntime.HistoryItem{ID: "um1", TurnID: "t1", Role: "user", Text: "laptop", At: f.now})
	accepted, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "a", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.archiveChat(ctx, session, "concluded"); err != nil {
		t.Fatal(err)
	}
	// The teardown that follows must not overwrite the native disposition.
	if err := f.service.stopChat(ctx, session.ID, true); err != nil {
		t.Fatal(err)
	}
	f.repo.listedSessions = nil
	if got := f.submission(session.ID, accepted.ID); got.Status != domain.SubmissionUncertain || got.AcceptedAt == nil {
		t.Fatalf("accepted at end = %+v", got)
	}
	conversations, err := f.service.conversationRepo()
	if err != nil {
		t.Fatal(err)
	}
	if archived, err := conversations.IsArchivedChat(ctx, session.ID); err != nil || !archived {
		t.Fatalf("archived = %v, %v", archived, err)
	}
	if replay, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "a", Text: "first"}); err != nil || replay.ID != accepted.ID {
		t.Fatalf("replay after end = %+v, %v", replay, err)
	}
	var refused *domain.DeliveryError
	if _, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "new", Text: "after end"}); !errors.As(err, &refused) || refused.Code != domain.DeliverySessionArchived {
		t.Fatalf("send after end = %v", err)
	}
}

func TestHeldTerminalInputIsFlushedInOrder(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := newPTYTerminalManager(logger)
	p := &terminalProcess{key: terminalKey("s", "t"), pty: w, logger: logger, done: make(chan struct{})}
	m.processes = map[string]*terminalProcess{p.key: p}
	release := m.HoldInput("s", "t", time.Minute)
	for _, chunk := range []string{"a", "b"} {
		if err := p.write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	release()
	if err := p.write([]byte("c")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "abc" {
		t.Fatalf("flushed %q, want abc", buf)
	}
	if m.HoldInput("s", "other", time.Minute) == nil {
		t.Fatal("holding an unknown terminal must be a no-op release")
	}
}

func TestTurnCutByAProviderRestartIsUncertainAndDoesNotHoldLaterInput(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-5", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	codex := f.history[agentruntime.AgentCodex]
	sub, _ := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "a", Text: "long task"})
	codex.record(agentruntime.HistoryItem{ID: "um", TurnID: "t", Role: "user", Ref: sub.ID, Text: "long task", At: f.now})
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != domain.SubmissionRunning || got.Error != "Running in the native conversation" {
		t.Fatalf("running = %+v", got)
	}
	// The provider process dies mid-turn; Hiveryn resumes a new run.
	f.setMainTerminal(session.ID, "term-resumed")
	f.repo.listedSessions = []domain.Session{session}
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != domain.SubmissionUncertain || !strings.Contains(got.Error, "restarted during this turn") {
		t.Fatalf("cut turn = %+v", got)
	}
	if next, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "b", Text: "after"}); err != nil || next.Status != domain.SubmissionAccepted {
		t.Fatalf("the next input must not stay behind a cut turn: %+v %v", next, err)
	}
}
