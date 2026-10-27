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
	sub, err := f.service.GetSubmission(context.Background(), sessionID, id)
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

func TestClassifyTerminalInputKeepsProvenanceAndHidesTheKickoff(t *testing.T) {
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
		{"kickoff with injected context", agentruntime.HistoryItem{Text: "# Project: lab\n\nKICKOFF\n\n<document>overview</document>"}, terminalInput{via: "kickoff"}},
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

	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r-1", Text: "what did I say?"})
	if err != nil || sub.Status != "queued" {
		t.Fatalf("submit = %+v, %v", sub, err)
	}
	if replay, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r-1", Text: "what did I say?"}); err != nil || replay.ID != sub.ID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	var conflict *domain.ConflictError
	if _, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r-1", Text: "different"}); !errors.As(err, &conflict) {
		t.Fatalf("different text under the same request id = %v, want conflict", err)
	}

	f.dispatch()
	started := f.submission(session.ID, sub.ID)
	if started.Status != "started" || !strings.Contains(started.Error, "Queued natively") {
		t.Fatalf("after hand-off = %+v", started)
	}
	deliveries := codex.deliveries()
	if len(deliveries) != 1 || deliveries[0].MessageID != sub.ID || deliveries[0].Text != "what did I say?" || deliveries[0].NativeSessionID != "native-w-1" {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	// Still queued natively: no second hand-off of anything.
	codex.setReceipt(sub.ID, agentruntime.MessageReceipt{Queued: true})
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "started" || !strings.Contains(got.Error, "Queued natively") {
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
	if done.Status != "completed" || done.NativeTurnID != "t2" || done.Error != "" {
		t.Fatalf("completed = %+v", done)
	}
	if len(codex.deliveries()) != 1 {
		t.Fatalf("an input must be handed over once: %+v", codex.deliveries())
	}

	var got []string
	for _, item := range f.history2(session.ID) {
		got = append(got, strings.Join([]string{item.ID, item.Role, item.Via, item.NativeOrigin, item.SubmissionID, item.Text}, "|"))
	}
	want := []string{
		"am0|assistant||||READY",
		"turn:t0|system||||",
		"um1|user|terminal|user||remember PLUM",
		"um2|user|api|queue|" + sub.ID + "|what did I say?",
		"am2|assistant|||" + sub.ID + "|PLUM",
		"turn:t2|system|||" + sub.ID + "|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestClaudeTerminalSubmissionWaitsForAnIdleWaiterAndIsCorrelatedByItsHeader(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("a-1", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	claude := f.history[agentruntime.AgentClaude]

	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "phone-1", Text: "hello from the phone"})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "queued" || !strings.Contains(got.Error, "no armed input waiter") {
		t.Fatalf("without a waiter = %+v", got)
	}
	// A startup waiter is armed while the kickoff runs: busy.
	f.armWaiter(session.ID, "SessionStart", "startup")
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "queued" || !strings.Contains(got.Error, "busy") {
		t.Fatalf("while busy = %+v", got)
	}
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	stop := f.armWaiter(session.ID, "Stop", "")
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "started" {
		t.Fatalf("idle with a waiter = %+v", got)
	}
	_, body := waiterResponse(t, stop)
	if !strings.Contains(body, claudeInputHeader+sub.ID) || !strings.Contains(body, "hello from the phone") {
		t.Fatalf("waiter text = %s", body)
	}
	at := f.now.Add(time.Second)
	claude.record(
		agentruntime.HistoryItem{ID: "u4", TurnID: "p4", Role: "user", NativeOrigin: "task_notification", Text: `Stop hook blocking error from command "Stop": ` + claudeInputHeader + sub.ID + " — a message from the user:\n\nhello from the phone", At: at},
		agentruntime.HistoryItem{ID: "x4:0", TurnID: "p4", Role: "assistant", Text: "hi", Status: "completed", At: at},
		agentruntime.HistoryItem{ID: "s4", TurnID: "p4", Role: "system", Status: "completed", TurnEnd: true, At: at},
	)
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "completed" || got.NativeTurnID != "p4" {
		t.Fatalf("completed = %+v", got)
	}
	items := f.history2(session.ID)
	if len(items) != 3 || items[0].Via != "api" || items[0].NativeOrigin != "task_notification" || items[0].Text != "hello from the phone" || items[0].SubmissionID != sub.ID {
		t.Fatalf("history = %+v", items)
	}
	if len(claude.reads) == 0 || claude.reads[0].TranscriptHandle != "/transcripts/claude" {
		t.Fatalf("history requests = %+v", claude.reads)
	}
}

func TestTerminalInputMissingAfterIdleIsUncertainAndNeverResent(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-2", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	codex := f.history[agentruntime.AgentCodex]
	codex.record(agentruntime.HistoryItem{ID: "um1", TurnID: "t1", Role: "user", Text: "laptop", At: f.now})
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r", Text: "lost?"})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	// The record moves past the hand-off and the agent goes idle well after it,
	// yet the input is nowhere: uncertain, and never handed over again.
	f.now = f.now.Add(30 * time.Second)
	codex.record(agentruntime.HistoryItem{ID: "am9", TurnID: "t9", Role: "assistant", Text: "other", At: f.now})
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	f.dispatch()
	got := f.submission(session.ID, sub.ID)
	if got.Status != "uncertain" || !strings.Contains(got.Error, "not resent") {
		t.Fatalf("lost = %+v", got)
	}
	f.dispatch()
	if n := len(codex.deliveries()); n != 1 {
		t.Fatalf("deliveries = %d, want 1", n)
	}
	// A later input is not held behind the uncertain one: the laptop is not either.
	next, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r2", Text: "next"})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	if got := f.submission(session.ID, next.ID); got.Status != "started" {
		t.Fatalf("next = %+v", got)
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
	f.service.messages.endpoints[session.ID] = messageEndpoint{port: 1, password: "p"}
	holds := &holdRecorder{fakeTerminalManager: &fakeTerminalManager{}}
	f.service.terminal = holds
	oc := f.history[agentruntime.AgentOpenCode]
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "r", Text: "run the long task"})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "queued" || !strings.Contains(got.Error, "busy") {
		t.Fatalf("not idle = %+v", got)
	}
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "started" {
		t.Fatalf("idle = %+v", got)
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
	if got := f.submission(session.ID, sub.ID); got.Status != "uncertain" || !strings.Contains(got.Error, "merged later input") {
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
	handed, _ := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "a", Text: "first"})
	f.dispatch()
	queued, _ := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "b", Text: "second"})
	if err := f.service.archiveChat(ctx, session, "concluded"); err != nil {
		t.Fatal(err)
	}
	// The teardown that follows must not overwrite the native disposition.
	if err := f.service.stopChat(ctx, session.ID, true); err != nil {
		t.Fatal(err)
	}
	f.repo.listedSessions = nil
	if got := f.submission(session.ID, handed.ID); got.Status != "uncertain" {
		t.Fatalf("handed at end = %+v", got)
	}
	if got := f.submission(session.ID, queued.ID); got.Status != "failed" {
		t.Fatalf("queued at end = %+v", got)
	}
	page, err := f.service.ListArchivedChats(ctx, domain.SequenceQuery{})
	if err != nil || len(page.Chats) != 1 || page.Chats[0].InteractionMode != domain.InteractionTerminal {
		t.Fatalf("archive = %+v, %v", page, err)
	}
	if replay, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "a", Text: "first"}); err != nil || replay.ID != handed.ID {
		t.Fatalf("replay after end = %+v, %v", replay, err)
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

func TestTurnCutByAProviderRestartIsUncertainAndReleasesTheQueue(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("w-5", agentruntime.AgentCodex)
	f.repo.listedSessions = []domain.Session{session}
	codex := f.history[agentruntime.AgentCodex]
	sub, _ := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "a", Text: "long task"})
	f.dispatch()
	codex.record(agentruntime.HistoryItem{ID: "um", TurnID: "t", Role: "user", Ref: sub.ID, Text: "long task", At: f.now})
	next, _ := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "b", Text: "after"})
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "started" || got.Error != "Running in the native conversation" {
		t.Fatalf("running = %+v", got)
	}
	// The provider process dies mid-turn; Hiveryn resumes a new run.
	f.setMainTerminal(session.ID, "term-resumed")
	f.repo.listedSessions = []domain.Session{session}
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != "uncertain" || !strings.Contains(got.Error, "restarted during this turn") {
		t.Fatalf("cut turn = %+v", got)
	}
	if got := f.submission(session.ID, next.ID); got.Status != "started" {
		t.Fatalf("the next input must not stay behind a cut turn: %+v", got)
	}
}
