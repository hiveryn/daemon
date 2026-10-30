package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	artcodex "github.com/hiveryn/agentruntime/adapter/codex"
	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// remoteCompanionFixture: machine ka (fake SSH, fake tmux, stub provider
// CLIs) with repos r00 and r01, a Claude and a Codex variant for ka, and one
// companion of each provider. The provider adapters are the real ones, so
// their remote setup and launch preparation run against ka's home.
type remoteCompanionFixture struct {
	*remoteLaunchFixture
	companions *store.CompanionStore
	publisher  *fakePublisher
}

func newRemoteCompanionFixture(t *testing.T) *remoteCompanionFixture {
	t.Helper()
	f := &remoteCompanionFixture{remoteLaunchFixture: newRemoteLaunchFixture(t, 2)}
	ackWait := companionAckWait
	companionAckWait = 0
	t.Cleanup(func() { companionAckWait = ackWait })
	if err := os.WriteFile(filepath.Join(f.stubs, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.service.cfg.Variants["ka-codex"] = config.VariantConfig{Agent: "codex", Machine: "ka"}
	f.service.adapters[agentruntime.AgentCodex] = artcodex.New(artcodex.DefaultOptions())
	architect := f.service.cfg.Architects["hiveryn"]
	architect.Companions = map[string]config.CompanionConfig{
		"rc": {Repo: "r00", Variant: "ka-claude"},
		"rx": {Repo: "r01", Variant: "ka-codex"},
	}
	f.service.cfg.Architects["hiveryn"] = architect
	f.service.actions = newActionStore()
	f.service.questions = newQuestionStore()
	f.publisher = &fakePublisher{}
	f.service.notifier = f.publisher
	f.companions = store.NewCompanionStore(f.db)
	f.service.SetCompanionRepository(f.companions)
	t.Cleanup(func() {
		for _, id := range []string{"rc", "rx"} {
			if c, err := f.companions.GetCompanion(context.Background(), f.architect.ID, id); err == nil {
				f.service.closeRemote(c.SessionID)
			}
		}
	})
	return f
}

func (f *remoteCompanionFixture) record(t *testing.T, name string) domain.Companion {
	t.Helper()
	c, err := f.companions.GetCompanion(context.Background(), f.architect.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *remoteCompanionFixture) workerFiles(t *testing.T, sessionID string) string {
	t.Helper()
	dir := filepath.Join(f.home, ".local/state/hiveryn-workers", sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("remote runtime directory of %s: %v", sessionID, err)
	}
	var all strings.Builder
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	return all.String()
}

func gatewayCall(t *testing.T, port int, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/mcp", port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	b, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(b)
}

// Remote Claude and Codex companions launch on their repository's machine
// through SSH/tmux with no Hiveryn installation there, behind a gateway that
// serves exactly the companion tools, and keep the approved instructions
// unchanged.
func TestRemoteCompanionsLaunchOnTheirMachineWithCompanionScopedGateway(t *testing.T) {
	f := newRemoteCompanionFixture(t)
	f.service.startCompanions(context.Background(), f.architect)
	f.service.companionRT.wg.Wait()

	for name, agent := range map[string]string{"rc": "claude", "rx": "codex"} {
		c := f.record(t, name)
		if c.Status != domain.CompanionRunning || c.Machine != "ka" || c.Agent != agent {
			t.Fatalf("%s = %+v", name, c)
		}
		session, err := f.sessions.GetSession(context.Background(), c.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.Machine != "ka" || session.SSH != "ka" || len(session.RemoteToken) != 64 || session.CurrentRun.ProfileSnapshot.Machine != "ka" {
			t.Fatalf("%s session location = machine %q ssh %q snapshot %+v", name, session.Machine, session.SSH, session.CurrentRun.ProfileSnapshot)
		}
		launch := f.workerFiles(t, c.SessionID)
		if !strings.Contains(launch, "You are a companion agent") || strings.Contains(launch, "If Hiveryn MCP becomes unreachable") {
			t.Fatalf("%s remote instructions are not the approved companion text unchanged", name)
		}
		if !strings.Contains(launch, "Session started: ") || strings.Contains(launch, "hiverynd") {
			t.Fatalf("%s remote launch: kickoff missing or a Hiveryn binary referenced", name)
		}
		f.service.remoteMu.Lock()
		w := f.service.remotes[c.SessionID]
		f.service.remoteMu.Unlock()
		if w == nil {
			t.Fatalf("%s has no gateway", name)
		}
		status, tools := gatewayCall(t, w.port, session.RemoteToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		if status != 200 {
			t.Fatalf("%s gateway: %d %s", name, status, tools)
		}
		for _, tool := range []string{"sendMessageToArchitect", "replyToArchitectMessage", "requestAction", "completeCompanionShutdown"} {
			if !strings.Contains(tools, `"name":"`+tool+`"`) {
				t.Fatalf("%s gateway lacks %s", name, tool)
			}
		}
		for _, tool := range []string{"readTicket", "concludeTicketSession", "spawnTicketWorker", "sendMessageToCompanion", "requestCompanionShutdown", "askQuestion", "notify", "readContext"} {
			if strings.Contains(tools, `"name":"`+tool+`"`) {
				t.Fatalf("%s gateway leaks %s", name, tool)
			}
		}
		if status, _ := gatewayCall(t, w.port, strings.Repeat("0", 64), `{}`); status != 401 {
			t.Fatalf("%s gateway accepted a wrong token: %d", name, status)
		}
	}
	if f.count("new-session") != 2 {
		t.Fatalf("tmux creations = %d", f.count("new-session"))
	}
}

// A remote shutdown is complete only once termination is confirmed over SSH;
// an unreachable machine leaves it stop_failed (blocking the conclusion)
// until the user's discard confirms it.
func TestRemoteCompanionShutdownConfirmsTermination(t *testing.T) {
	companionStopGrace = 0
	f := newRemoteCompanionFixture(t)
	ctx := context.Background()
	f.service.startCompanions(ctx, f.architect)
	f.service.companionRT.wg.Wait()

	rc, rx := f.record(t, "rc"), f.record(t, "rx")
	idRC, idRX := shutdownID(), shutdownID()
	for name, id := range map[string]string{"rc": idRC, "rx": idRX} {
		if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: name, RequestID: id}); err != nil {
			t.Fatal(err)
		}
	}
	before := f.count(remoteSocket(rc.SessionID))
	if _, err := f.service.CompleteCompanionShutdown(ctx, rc.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: idRC, Handoff: "rc done"}); err != nil {
		t.Fatal(err)
	}
	f.service.companionRT.wg.Wait()
	if c := f.record(t, "rc"); c.Status != domain.CompanionStopped {
		t.Fatalf("rc = %+v", c)
	}
	if f.count(remoteSocket(rc.SessionID)) <= before {
		t.Fatal("no remote termination command for rc")
	}

	f.ssh.SetDown(t, "ka", true)
	if _, err := f.service.CompleteCompanionShutdown(ctx, rx.SessionID, domain.CompleteCompanionShutdownRequest{RequestID: idRX, Handoff: "rx done"}); err != nil {
		t.Fatal(err)
	}
	f.service.companionRT.wg.Wait()
	c := f.record(t, "rx")
	if c.Status != domain.CompanionStopFailed || !strings.Contains(c.Error, "not confirmed") {
		t.Fatalf("rx with ka down = %+v", c)
	}
	if _, err := f.sessions.GetSession(ctx, rx.SessionID); err != nil {
		t.Fatalf("unconfirmed companion session not retained: %v", err)
	}
	if err := f.service.companionConclusionConflict(ctx, f.architect.ID); err == nil || !strings.Contains(err.Error(), "rx (stop_failed)") {
		t.Fatalf("conclusion guard = %v", err)
	}
	all, _ := f.messages.ListAgentMessages(ctx, "hiveryn", 100)
	reported := false
	for _, m := range all {
		if m.Kind == domain.AgentMessageCompanionShutdownOutcome && strings.Contains(m.Body, idRX) && strings.Contains(m.Body, "NOT confirmed") {
			reported = true
		}
	}
	if !reported {
		t.Fatal("unconfirmed termination not reported to the architect")
	}
	// The user's discard retries the termination once ka is back.
	if err := f.service.DiscardCompanion(ctx, rx.SessionID); err == nil {
		t.Fatal("discard confirmed termination while ka is down")
	}
	f.ssh.SetDown(t, "ka", false)
	if err := f.service.DiscardCompanion(ctx, rx.SessionID); err != nil {
		t.Fatal(err)
	}
	if c := f.record(t, "rx"); c.Status != domain.CompanionStopped {
		t.Fatalf("rx after discard = %+v", c)
	}
	if err := f.service.companionConclusionConflict(ctx, f.architect.ID); err != nil {
		t.Fatalf("conclusion still blocked: %v", err)
	}
}

// A remote companion whose machine is unreachable at launch fails to start
// and leaves nothing it could not account for.
func TestRemoteCompanionStartFailureOnUnreachableMachine(t *testing.T) {
	f := newRemoteCompanionFixture(t)
	f.ssh.SetDown(t, "ka", true)
	f.service.startCompanions(context.Background(), f.architect)
	f.service.companionRT.wg.Wait()
	for _, name := range []string{"rc", "rx"} {
		c := f.record(t, name)
		switch c.Status {
		case domain.CompanionStartFailed:
			if c.SessionID != "" {
				t.Fatalf("%s start_failed but kept session %s", name, c.SessionID)
			}
		case domain.CompanionStopFailed:
			// Cleanup could not be confirmed with ka down: visible and kept.
			if _, err := f.sessions.GetSession(context.Background(), c.SessionID); err != nil {
				t.Fatalf("%s stop_failed without its session: %v", name, err)
			}
		default:
			t.Fatalf("%s with ka down = %+v", name, c)
		}
		if !strings.Contains(c.Error, "launch failed") {
			t.Fatalf("%s error = %q", name, c.Error)
		}
	}
}

// The outbox reaches a remote Codex session through app-server on its own
// machine and confirms the message from the mirrored remote record; a remote
// Claude session is reachable only through its live attachment.
func TestOutboxDeliversToRemoteSessions(t *testing.T) {
	f := newRemoteConversationFixture(t)
	installRemoteCodex(t)
	codexHome := filepath.Join(f.home, "codex-companion")
	session := f.remoteSession("rx-companion", agentruntime.AgentCodex, map[string]string{"CODEX_HOME": codexHome})
	rollout := filepath.Join(codexHome, "sessions", "2026", "10", "30", "rollout-2026-10-30T10-00-00-"+remoteNative+".jsonl")
	writeTranscript(t, rollout,
		`{"timestamp":"2026-10-30T10:00:00Z","type":"session_meta","payload":{"id":"`+remoteNative+`","timestamp":"2026-10-30T10:00:00Z"}}`)
	m, err := f.service.EnqueueAgentMessage(context.Background(), AgentMessageParams{
		Kind: domain.AgentMessageArchitectMessage, Recipient: "session:" + session.ID, ArchitectKey: "hiveryn", DedupeKey: "t:1", Body: "From the architect: go",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	handed := f.get(m.ID)
	if handed.State != domain.AgentMessageHanded || handed.TargetConfigRoot != "remote:ka" {
		t.Fatalf("handed = %+v", handed)
	}
	b, err := os.ReadFile(filepath.Join(codexHome, "queue.json"))
	if err != nil || !strings.Contains(string(b), m.ID) || !strings.Contains(string(b), `"home":"`+f.home+`"`) {
		t.Fatalf("remote queue = %s %v", b, err)
	}
	// Still queued on ka: not lost, not redelivered.
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageHanded || got.Attempts != 1 {
		t.Fatalf("queued = %+v", got)
	}
	appendTranscript(t, rollout,
		`{"timestamp":"2026-10-30T10:01:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1","turn_attribution":{"turn_trigger":"queue"}}}`,
		`{"timestamp":"2026-10-30T10:01:00Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"um1","client_id":"`+m.ID+`","content":[{"type":"text","text":"Hiveryn message `+m.ID+`: From the architect: go"}]}}}`)
	f.dispatch()
	if got := f.get(m.ID); got.State != domain.AgentMessageAppeared {
		t.Fatalf("appeared = %+v", got)
	}

	claudeSession := f.remoteSession("rc-companion", agentruntime.AgentClaude, map[string]string{})
	cm, err := f.service.EnqueueAgentMessage(context.Background(), AgentMessageParams{
		Kind: domain.AgentMessageArchitectMessage, Recipient: "session:" + claudeSession.ID, ArchitectKey: "hiveryn", DedupeKey: "t:2", Body: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	if got := f.get(cm.ID); got.State != domain.AgentMessagePending || !strings.Contains(got.Condition, "SSH attachment") {
		t.Fatalf("remote claude without attachment = %+v", got)
	}
}

// A remote companion's stop is released only by the written response of its
// own accepted completeCompanionShutdown for the bound request_id. The real
// API answers that completion only when the test lets it, so the gateway's
// response stays unwritten while another MCP call of the same companion and a
// completion with an unknown request_id are answered; neither releases the
// stop. A completion whose caller disconnected before its response was
// written releases nothing either: the stop waits out its bounded wait and
// says so.
func TestRemoteCompanionStopReleasedOnlyByItsCompletionResponse(t *testing.T) {
	f := newRemoteCompanionFixture(t)
	companionStopGrace, companionAckWait = 0, time.Hour
	t.Cleanup(func() { companionStopGrace = 0 })

	// The daemon API the gateways call, holding completion responses for
	// held request ids after the service has accepted them.
	var heldMu sync.Mutex
	held := map[string]chan struct{}{}
	entered := make(chan string, 4)
	apiHandler := api.NewHandler(api.Dependencies{Config: f.service.cfg, ConfigSource: config.StaticSource(f.service.cfg), Logger: f.service.logger, Sessions: f.service, Companions: f.service})
	daemonAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/companion/shutdown-completion") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req domain.CompleteCompanionShutdownRequest
		_ = json.Unmarshal(body, &req)
		heldMu.Lock()
		release := held[req.RequestID]
		heldMu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if release == nil {
			apiHandler.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		apiHandler.ServeHTTP(rec, r)
		entered <- req.RequestID
		<-release
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(daemonAPI.Close)
	f.service.baseURL = daemonAPI.URL

	ctx := context.Background()
	f.service.startCompanions(ctx, f.architect)
	f.service.companionRT.wg.Wait()
	rc, rx := f.record(t, "rc"), f.record(t, "rx")
	gateway := func(c domain.Companion) (int, string) {
		f.service.remoteMu.Lock()
		defer f.service.remoteMu.Unlock()
		w := f.service.remotes[c.SessionID]
		if w == nil {
			t.Fatalf("%s has no gateway", c.Name)
		}
		session, err := f.sessions.GetSession(ctx, c.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return w.port, session.RemoteToken
	}
	call := func(callCtx context.Context, port int, token, tool, args string) (int, string, error) {
		return gatewayPost(callCtx, port, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	}
	ackPending := func(sessionID string) bool {
		f.service.companionRT.mu.Lock()
		defer f.service.companionRT.mu.Unlock()
		_, ok := f.service.companionRT.acks[sessionID]
		return ok
	}
	outcome := func(requestID string) string {
		all, _ := f.messages.ListAgentMessages(ctx, "hiveryn", 100)
		for _, m := range all {
			if m.Kind == domain.AgentMessageCompanionShutdownOutcome && strings.Contains(m.Body, requestID) {
				return m.Body
			}
		}
		return ""
	}

	idRC, idRX := shutdownID(), shutdownID()
	for name, id := range map[string]string{"rc": idRC, "rx": idRX} {
		if _, err := f.service.RequestCompanionShutdown(ctx, f.architect.ID, domain.CompanionShutdownRequest{Companion: name, RequestID: id}); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(requestID string) func() {
		release := make(chan struct{})
		var once sync.Once
		heldMu.Lock()
		held[requestID] = release
		heldMu.Unlock()
		free := func() { once.Do(func() { close(release) }) }
		t.Cleanup(free)
		return free
	}
	release := hold(idRC)

	port, token := gateway(rc)
	type result struct {
		status int
		body   string
		err    error
	}
	completion := make(chan result, 1)
	go func() {
		status, body, err := call(ctx, port, token, "completeCompanionShutdown", `{"request_id":"`+idRC+`","handoff":"rc handoff"}`)
		completion <- result{status, body, err}
	}()
	if got := <-entered; got != idRC {
		t.Fatalf("held completion %q", got)
	}
	if !ackPending(rc.SessionID) {
		t.Fatal("the accepted completion is not waiting for its acknowledgement")
	}
	// Other MCP requests of the same companion complete meanwhile: a
	// successful tools/list and a refused tool call.
	if status, body, err := gatewayPost(ctx, port, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); err != nil || status != http.StatusOK || !strings.Contains(body, "completeCompanionShutdown") {
		t.Fatalf("concurrent tools/list: %d %s %v", status, body, err)
	}
	if status, body, err := call(ctx, port, token, "sendMessageToArchitect", `{"message":"still here"}`); err != nil || status != http.StatusOK || !strings.Contains(body, "is stopping") {
		t.Fatalf("concurrent call: %d %s %v", status, body, err)
	}
	// A completion with a request_id that is not this companion's is refused
	// and releases nothing.
	if status, body, err := call(ctx, port, token, "completeCompanionShutdown", `{"request_id":"`+shutdownID()+`","handoff":"rc handoff"}`); err != nil || status != http.StatusOK || !strings.Contains(body, `"isError":true`) {
		t.Fatalf("unknown request_id: %d %s %v", status, body, err)
	}
	// So does another companion's request_id.
	if _, body, _ := call(ctx, port, token, "completeCompanionShutdown", `{"request_id":"`+idRX+`","handoff":"rc handoff"}`); !strings.Contains(body, `"isError":true`) {
		t.Fatalf("another companion's request_id accepted: %s", body)
	}
	time.Sleep(100 * time.Millisecond)
	if !ackPending(rc.SessionID) {
		t.Fatal("stop released before its completion response was written")
	}
	if _, err := f.sessions.GetSession(ctx, rc.SessionID); err != nil {
		t.Fatalf("rc session removed before its acknowledgement: %v", err)
	}
	if c := f.record(t, "rc"); c.Status != domain.CompanionStopping {
		t.Fatalf("rc before its acknowledgement = %+v", c)
	}
	release()
	res := <-completion
	if res.err != nil || res.status != http.StatusOK || !strings.Contains(res.body, `"status":"accepted"`) || !strings.Contains(res.body, idRC) {
		t.Fatalf("completion response: %d %s %v", res.status, res.body, res.err)
	}
	f.service.companionRT.wg.Wait()
	if c := f.record(t, "rc"); c.Status != domain.CompanionStopped {
		t.Fatalf("rc after its acknowledgement = %+v", c)
	}
	if body := outcome(idRC); body == "" || strings.Contains(body, "not observed") {
		t.Fatalf("rc outcome = %q", body)
	}

	// Interrupted transport: the caller disconnects while the accepted
	// completion's response is still unwritten.
	companionAckWait = 300 * time.Millisecond
	release = hold(idRX)
	port, token = gateway(rx)
	callCtx, cancel := context.WithCancel(ctx)
	go func() {
		status, body, err := call(callCtx, port, token, "completeCompanionShutdown", `{"request_id":"`+idRX+`","handoff":"rx handoff"}`)
		completion <- result{status, body, err}
	}()
	if got := <-entered; got != idRX {
		t.Fatalf("held completion %q", got)
	}
	cancel()
	if res := <-completion; res.err == nil {
		t.Fatalf("the cancelled call got a response: %d %s", res.status, res.body)
	}
	release()
	f.service.companionRT.wg.Wait()
	if c := f.record(t, "rx"); c.Status != domain.CompanionStopped {
		t.Fatalf("rx after the bounded wait = %+v", c)
	}
	if body := outcome(idRX); !strings.Contains(body, "not observed written") || !strings.Contains(body, "may not have received it") {
		t.Fatalf("rx outcome does not report the unobserved acknowledgement: %q", body)
	}
}

func gatewayPost(ctx context.Context, port int, token, body string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/mcp", port), strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()
	b, err := io.ReadAll(response.Body)
	return response.StatusCode, string(b), err
}
