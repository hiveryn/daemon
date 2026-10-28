package sessionruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// streamFixture runs the real API handler over a Service backed entirely by
// SQLite (sessions, events and the conversation feed) with native records the
// test writes through historyAdapter.
type streamFixture struct {
	t       *testing.T
	service *Service
	store   *store.SessionStore
	history map[agentruntime.AgentKind]*historyAdapter
	server  *httptest.Server
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	for _, v := range []*time.Duration{&observeInterval, &observeMinGap, &observeRemoteMinGap, &terminalHistoryMinInterval} {
		old := *v
		*v = 10 * time.Millisecond
		t.Cleanup(func() { *v = old })
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewSessionStore(db)
	f := &streamFixture{t: t, store: st, history: map[agentruntime.AgentKind]*historyAdapter{}}
	adapters := map[agentruntime.AgentKind]agentruntime.Adapter{}
	for _, kind := range []agentruntime.AgentKind{agentruntime.AgentClaude, agentruntime.AgentCodex, agentruntime.AgentOpenCode} {
		h := &historyAdapter{fakeMessageAdapter: &fakeMessageAdapter{kind: kind, mode: agentruntime.MessageDeliveryPush}}
		f.history[kind] = h
		adapters[kind] = h
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.service = &Service{
		logger:       logger,
		repo:         st,
		cfg:          config.Config{Architects: map[string]config.ArchitectConfig{"hiveryn": {Name: "Hiveryn", Path: t.TempDir()}}},
		adapters:     adapters,
		intents:      newIntentStore(),
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
		terminal:     &fakeTerminalManager{},
	}
	f.server = httptest.NewServer(api.NewHandler(api.Dependencies{Sessions: f.service, Logger: logger}))
	t.Cleanup(func() {
		f.server.CloseClientConnections()
		f.server.Close()
		f.service.stopConversationStreams(context.Background())
	})
	return f
}

func (f *streamFixture) session(id string, kind agentruntime.AgentKind, machine string) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.store.CreateSession(ctx, domain.CreateSessionParams{ID: id, ArchitectKey: "hiveryn-" + id, SessionType: domain.SessionTypeArchitect, ContextID: "ctx-" + id, Prompt: "SECRET KICKOFF", Instructions: "SECRET SYSTEM", Workdir: "/work", Machine: machine, SSH: machine}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.CreateRun(ctx, domain.CreateSessionRunParams{ID: "run-" + id, SessionID: id, ProfileName: string(kind), NativeID: "native-" + id, Workdir: "/work", StartedAt: time.Now(),
		ProfileSnapshot: domain.AgentProfileSnapshot{Agent: string(kind), Env: map[string]string{"API_TOKEN": "SECRET ENV"}, Args: []string{"--secret-arg"}}}); err != nil {
		f.t.Fatal(err)
	}
}

type sseEvent struct {
	name, id string
	data     json.RawMessage
}

type sseClient struct {
	t      *testing.T
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

func (f *streamFixture) open(id string, header map[string]string) *sseClient {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/api/sessions/"+id+"/stream", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	c := &sseClient{t: f.t, resp: resp, events: make(chan sseEvent, 256), cancel: cancel}
	f.t.Cleanup(c.close)
	if resp.StatusCode != http.StatusOK {
		return c
	}
	go func() {
		defer close(c.events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		var ev sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if ev.name != "" {
					c.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return c
}

func (c *sseClient) close() {
	c.cancel()
	_ = c.resp.Body.Close()
}

// next returns the next event named name, skipping others it is told to.
func (c *sseClient) next(name string, skip ...string) sseEvent {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				c.t.Fatalf("stream closed while waiting for %s", name)
			}
			if ev.name == name {
				return ev
			}
			allowed := false
			for _, s := range skip {
				allowed = allowed || s == ev.name
			}
			if !allowed {
				c.t.Fatalf("got %s %s while waiting for %s", ev.name, ev.data, name)
			}
		case <-timeout:
			c.t.Fatalf("timed out waiting for %s", name)
		}
	}
}

func (c *sseClient) page(name string, skip ...string) domain.ConversationPage {
	c.t.Helper()
	var page domain.ConversationPage
	ev := c.next(name, skip...)
	if err := json.Unmarshal(ev.data, &page); err != nil {
		c.t.Fatal(err)
	}
	if ev.id != fmt.Sprint(page.NextRevision) {
		c.t.Fatalf("event id %q, next_revision %d", ev.id, page.NextRevision)
	}
	return page
}

func itemTexts(page domain.ConversationPage) string {
	var out []string
	for _, item := range page.Items {
		out = append(out, fmt.Sprintf("%s@%d", item.Text, item.Sequence))
	}
	return strings.Join(out, ",")
}

// sync forces a native read into the feed (a recovery read may be throttled).
func (f *streamFixture) sync(id string) {
	f.t.Helper()
	session, err := f.service.GetSession(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.service.syncTerminalHistory(context.Background(), session, true); err != nil {
		f.t.Fatal(err)
	}
}

func (f *streamFixture) observers() int {
	f.service.streams.mu.Lock()
	defer f.service.streams.mu.Unlock()
	return len(f.service.streams.observers)
}

func (f *streamFixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamSendsSnapshotsThenLiveRevisionsIncludingMovedItems(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s1", agentruntime.AgentCodex, "")
	codex := f.history[agentruntime.AgentCodex]
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	codex.record(agentruntime.HistoryItem{ID: "a1", TurnID: "t1", Role: "assistant", Text: "reply", At: at},
		agentruntime.HistoryItem{ID: "b1", TurnID: "t1", Role: "assistant", Text: "later", At: at})

	c := f.open("s1", nil)
	var status domain.ConversationStreamStatus
	if err := json.Unmarshal(c.next(domain.StreamEventStatus).data, &status); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(status)
	if status.State != domain.StreamStateLive || status.Session == nil || status.Session.ID != "s1" || !status.Session.Conversation.History || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("status = %s", raw)
	}
	c.next(domain.StreamEventControls)
	// The native read happens on the observer; the initial page may be empty
	// (replay complete) before the record is first synced.
	page := c.page(domain.StreamEventConversation)
	if len(page.Items) == 0 {
		page = c.page(domain.StreamEventConversation, domain.StreamEventStatus)
	}
	if !strings.HasPrefix(itemTexts(page), "reply@") || len(page.Items) != 2 {
		t.Fatalf("first page = %s", itemTexts(page))
	}
	first, second := page.Items[0], page.Items[1]

	// A kickoff discovered late before the reply arrives live in its native
	// place.
	codex.mu.Lock()
	codex.history.Items = append([]agentruntime.HistoryItem{{ID: "k", TurnID: "t1", Role: "user", Text: "kickoff", At: at}}, codex.history.Items...)
	codex.mu.Unlock()
	page = c.page(domain.StreamEventConversation, domain.StreamEventStatus)
	if len(page.Items) != 1 || page.Items[0].ID != "k" || page.Items[0].Sequence >= first.Sequence {
		t.Fatalf("late kickoff page = %+v (first %+v)", page.Items, first)
	}
	// The record now places the reply after "later" (with a new item): one
	// of the two old items moves as a new revision of itself, and the client's
	// highest-revision-wins view ends in the record's order.
	codex.mu.Lock()
	codex.history.Items = []agentruntime.HistoryItem{codex.history.Items[0], codex.history.Items[2], codex.history.Items[1],
		{ID: "c1", TurnID: "t1", Role: "assistant", Text: "new", At: at}}
	codex.mu.Unlock()
	page = c.page(domain.StreamEventConversation, domain.StreamEventStatus)
	view := map[string]domain.ConversationItem{first.ID: first, second.ID: second}
	moved := false
	for _, item := range page.Items {
		if old, ok := view[item.ID]; ok && item.Revision > old.Revision {
			moved = true
		}
		view[item.ID] = item
	}
	if !moved || view["b1"].Sequence >= view["a1"].Sequence || view["a1"].Sequence >= view["c1"].Sequence {
		t.Fatalf("moved page = %+v", page.Items)
	}
}

func TestStreamReconnectResumesFromCursorAndResetsAForeignCursor(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s2", agentruntime.AgentClaude, "")
	claude := f.history[agentruntime.AgentClaude]
	claude.record(agentruntime.HistoryItem{ID: "u1", TurnID: "t1", Role: "user", Text: "one", At: time.Now()})
	// Populate the feed through a recovery read.
	f.sync("s2")
	first := f.open("s2", nil)
	page := first.page(domain.StreamEventConversation, domain.StreamEventStatus, domain.StreamEventControls)
	cursor := page.NextRevision
	first.close()
	f.eventually("observer released after disconnect", func() bool { return f.observers() == 0 })

	// Changed while disconnected: only the newer revision is replayed.
	claude.record(agentruntime.HistoryItem{ID: "a1", TurnID: "t1", Role: "assistant", Text: "two", At: time.Now()})
	f.sync("s2")
	again := f.open("s2", map[string]string{"Last-Event-ID": fmt.Sprint(cursor)})
	page = again.page(domain.StreamEventConversation, domain.StreamEventStatus, domain.StreamEventControls)
	if itemTexts(page) == "" || strings.Contains(itemTexts(page), "one@") || page.Items[0].Revision <= cursor {
		t.Fatalf("resumed page = %s", itemTexts(page))
	}

	// A cursor from another database: reset, then the full replay.
	foreign := f.open("s2", map[string]string{"Last-Event-ID": "999999"})
	foreign.next(domain.StreamEventReset, domain.StreamEventStatus, domain.StreamEventControls)
	page = foreign.page(domain.StreamEventConversation)
	if !strings.Contains(itemTexts(page), "one@") || !strings.Contains(itemTexts(page), "two@") {
		t.Fatalf("replay after reset = %s", itemTexts(page))
	}

	for _, bad := range []string{"-1", "abc"} {
		c := f.open("s2", map[string]string{"Last-Event-ID": bad})
		if c.resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("cursor %q: HTTP %d", bad, c.resp.StatusCode)
		}
	}
}

func TestStreamControlsFollowQuestionLifecycle(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s3", agentruntime.AgentCodex, "")
	c := f.open("s3", nil)
	var controls domain.ConversationStreamControls
	if err := json.Unmarshal(c.next(domain.StreamEventControls, domain.StreamEventStatus).data, &controls); err != nil || len(controls.Events) != 0 {
		t.Fatalf("initial controls = %+v %v", controls, err)
	}
	q := domain.AgentQuestion{ID: "q1", Question: "Pick", Answers: []string{"a", "b"}, Origin: domain.QuestionOrigin{SessionID: "s3"}, CreatedAt: time.Now()}
	if err := f.service.publishQuestionRequired(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c.next(domain.StreamEventControls, domain.StreamEventStatus, domain.StreamEventConversation).data, &controls); err != nil || len(controls.Events) != 1 || controls.Events[0].Status != "required" {
		t.Fatalf("required controls = %+v %v", controls, err)
	}
	// A reconnect while it is open gets the same authoritative snapshot.
	again := f.open("s3", nil)
	if err := json.Unmarshal(again.next(domain.StreamEventControls, domain.StreamEventStatus).data, &controls); err != nil || len(controls.Events) != 1 {
		t.Fatalf("reconnect controls = %+v %v", controls, err)
	}
	q.Status, q.Answer = domain.QuestionAnswered, "a"
	f.service.publishQuestionResolved(context.Background(), q)
	for _, client := range []*sseClient{c, again} {
		if err := json.Unmarshal(client.next(domain.StreamEventControls, domain.StreamEventStatus, domain.StreamEventConversation).data, &controls); err != nil || len(controls.Events) != 2 || controls.Events[1].Status != "resolved" || controls.Events[1].Raw["answer"] != "a" {
			t.Fatalf("resolved controls = %+v %v", controls, err)
		}
	}
}

func TestStreamReportsHistoryErrorsAndRecovers(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s4", agentruntime.AgentCodex, "")
	codex := f.history[agentruntime.AgentCodex]
	codex.mu.Lock()
	codex.err = errors.New("rollout unreadable: permission denied")
	codex.mu.Unlock()
	c := f.open("s4", nil)
	var status domain.ConversationStreamStatus
	for status.HistoryError == nil {
		if err := json.Unmarshal(c.next(domain.StreamEventStatus, domain.StreamEventControls, domain.StreamEventConversation).data, &status); err != nil {
			t.Fatal(err)
		}
	}
	if status.HistoryError.Code != domain.HistoryReadFailed || !strings.Contains(status.HistoryError.Message, "permission denied") {
		t.Fatalf("history error = %+v", status.HistoryError)
	}
	codex.mu.Lock()
	codex.err = nil
	codex.mu.Unlock()
	codex.record(agentruntime.HistoryItem{ID: "a1", TurnID: "t1", Role: "assistant", Text: "back", At: time.Now()})
	for status.HistoryError != nil {
		status = domain.ConversationStreamStatus{}
		if err := json.Unmarshal(c.next(domain.StreamEventStatus, domain.StreamEventConversation, domain.StreamEventControls).data, &status); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamEndsWithFinalHistoryThenEndedStatus(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s5", agentruntime.AgentCodex, "")
	codex := f.history[agentruntime.AgentCodex]
	c := f.open("s5", nil)
	c.next(domain.StreamEventConversation, domain.StreamEventStatus, domain.StreamEventControls)
	ctx := context.Background()
	session, err := f.store.GetSession(ctx, "s5")
	if err != nil {
		t.Fatal(err)
	}
	// Session end: final record sync, retention record, deletion.
	codex.record(agentruntime.HistoryItem{ID: "final", TurnID: "t1", Role: "assistant", Text: "last words", At: time.Now()})
	f.service.launchMu.Lock()
	f.service.ending = map[string]struct{}{"s5": {}}
	f.service.launchMu.Unlock()
	if _, err := f.service.syncTerminalHistory(ctx, session, true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ArchiveChat(ctx, domain.ArchivedChat{ID: "s5", ArchitectKey: "hiveryn", Agent: "codex", EndedAt: time.Now(), Lifecycle: "concluded"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteSession(ctx, "s5"); err != nil {
		t.Fatal(err)
	}
	for {
		ev := c.next(domain.StreamEventStatus, domain.StreamEventConversation, domain.StreamEventControls)
		var status domain.ConversationStreamStatus
		if err := json.Unmarshal(ev.data, &status); err != nil {
			t.Fatal(err)
		}
		if status.State == domain.StreamStateEnded {
			break
		}
	}
	// After the ended status the stream closes; the final revision preceded it.
	if _, ok := <-c.events; ok {
		t.Fatal("stream stayed open after ended")
	}
	// A client opening the ended session gets the retained history, then ended.
	late := f.open("s5", nil)
	page := late.page(domain.StreamEventConversation)
	if !strings.Contains(itemTexts(page), "last words@") {
		t.Fatalf("late page = %s", itemTexts(page))
	}
	var status domain.ConversationStreamStatus
	if err := json.Unmarshal(late.next(domain.StreamEventStatus).data, &status); err != nil || status.State != domain.StreamStateEnded || status.Session != nil {
		t.Fatalf("late status = %+v %v", status, err)
	}
	f.eventually("observers released", func() bool { return f.observers() == 0 })
}

func TestStreamRefusesUnsupportedAndUnknownSessionsBeforeStreaming(t *testing.T) {
	f := newStreamFixture(t)
	f.session("remote-oc", agentruntime.AgentOpenCode, "box")
	c := f.open("remote-oc", nil)
	body, _ := io.ReadAll(c.resp.Body)
	if c.resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), domain.HistoryUnsupported) {
		t.Fatalf("remote OpenCode: HTTP %d %s", c.resp.StatusCode, body)
	}
	missing := f.open("nope", nil)
	if missing.resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown: HTTP %d", missing.resp.StatusCode)
	}
	if f.observers() != 0 {
		t.Fatal("a refused stream left an observer")
	}
}

// A reader that never reads cannot hold the handler, the observer or the
// feed: its writes time out, it is released, and the feed keeps advancing.
func TestStalledReaderIsCutOffWithoutBlockingObservation(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s6", agentruntime.AgentCodex, "")
	codex := f.history[agentruntime.AgentCodex]
	// Big items so the socket buffers fill quickly.
	big := strings.Repeat("x", 256<<10)
	for i := range 40 {
		codex.record(agentruntime.HistoryItem{ID: fmt.Sprint("a", i), TurnID: "t1", Role: "assistant", Text: big, At: time.Now()})
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	if _, err := fmt.Fprintf(conn, "GET /api/sessions/s6/stream HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	f.eventually("observer started", func() bool { return f.observers() == 1 })
	// Let the handler fill the socket and block in a write.
	time.Sleep(500 * time.Millisecond)
	codex.mu.Lock()
	readsBefore := len(codex.reads)
	codex.mu.Unlock()
	// Ingestion and the observer continue regardless of the stalled reader.
	for i := range 5 {
		codex.record(agentruntime.HistoryItem{ID: fmt.Sprint("late", i), TurnID: "t2", Role: "assistant", Text: "late", At: time.Now()})
		start := time.Now()
		if _, err := f.service.ReadConversation(context.Background(), "s6", domain.ConversationQuery{Limit: 1}); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("a recovery read waited %s behind the stalled stream", time.Since(start))
		}
	}
	f.eventually("observer keeps reading", func() bool {
		codex.mu.Lock()
		defer codex.mu.Unlock()
		return len(codex.reads) > readsBefore+10
	})
	deadline := time.Now().Add(20 * time.Second)
	for f.observers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("stalled reader was never released")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestShutdownEndsStreamsAndObservers(t *testing.T) {
	f := newStreamFixture(t)
	f.session("s7", agentruntime.AgentCodex, "")
	c := f.open("s7", nil)
	c.next(domain.StreamEventStatus)
	f.service.stopConversationStreams(context.Background())
	for range c.events {
	}
	if f.observers() != 0 {
		t.Fatal("observer left after shutdown")
	}
	refused := f.open("s7", nil)
	if refused.resp.StatusCode != http.StatusConflict {
		t.Fatalf("after shutdown: HTTP %d", refused.resp.StatusCode)
	}
}

// Switching between sessions repeatedly leaves one observer per watched
// session at most, none after the last stream closes, and every stream only
// carries its own session.
func TestRepeatedSessionSwitchingReleasesObservers(t *testing.T) {
	f := newStreamFixture(t)
	f.session("x", agentruntime.AgentCodex, "")
	f.session("y", agentruntime.AgentClaude, "")
	f.history[agentruntime.AgentCodex].record(agentruntime.HistoryItem{ID: "cx", TurnID: "t", Role: "assistant", Text: "from x", At: time.Now()})
	f.history[agentruntime.AgentClaude].record(agentruntime.HistoryItem{ID: "cy", TurnID: "t", Role: "assistant", Text: "from y", At: time.Now()})
	for i := range 20 {
		id := []string{"x", "y"}[i%2]
		c := f.open(id, nil)
		var status domain.ConversationStreamStatus
		if err := json.Unmarshal(c.next(domain.StreamEventStatus).data, &status); err != nil || status.Session.ID != id {
			t.Fatalf("status of %s = %+v %v", id, status.Session, err)
		}
		c.close()
	}
	f.eventually("observers released", func() bool { return f.observers() == 0 })
	// Reads continue to be served normally; nothing is still observing.
	f.history[agentruntime.AgentCodex].mu.Lock()
	reads := len(f.history[agentruntime.AgentCodex].reads)
	f.history[agentruntime.AgentCodex].mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	f.history[agentruntime.AgentCodex].mu.Lock()
	after := len(f.history[agentruntime.AgentCodex].reads)
	f.history[agentruntime.AgentCodex].mu.Unlock()
	if after > reads+1 {
		t.Fatalf("native reads continued after every stream closed: %d -> %d", reads, after)
	}
}

// Agent activity (a hook or status event) makes the observer read the record
// at once instead of waiting for its interval; control events do not.
func TestAgentActivityWakesTheObserver(t *testing.T) {
	f := newStreamFixture(t)
	old := observeInterval
	observeInterval = time.Hour
	t.Cleanup(func() { observeInterval = old })
	f.session("w1", agentruntime.AgentCodex, "")
	c := f.open("w1", nil)
	c.page(domain.StreamEventConversation, domain.StreamEventStatus, domain.StreamEventControls)
	f.history[agentruntime.AgentCodex].record(agentruntime.HistoryItem{ID: "a1", TurnID: "t1", Role: "assistant", Text: "after the hook", At: time.Now()})
	f.service.publishEvent("w1", domain.SessionEvent{SessionID: "w1", Type: "agent_status", Status: "idle", At: time.Now()})
	page := c.page(domain.StreamEventConversation, domain.StreamEventStatus, domain.StreamEventControls)
	if !strings.Contains(itemTexts(page), "after the hook@") {
		t.Fatalf("woken page = %s", itemTexts(page))
	}
}
