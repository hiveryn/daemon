package sessionruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/adapter/claude"
	artcodex "github.com/hiveryn/agentruntime/adapter/codex"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

const remoteNative = "6f1c2b7e-0d4a-4c1b-9b8e-3f2a1d0c9e88"

// remoteFixture is a terminal-conversation fixture whose sessions run on the
// fake SSH machine "ka" with the real Claude and Codex readers.
type remoteFixture struct {
	*terminalFixture
	ssh  *sshtest.Fake
	home string
}

func newRemoteConversationFixture(t *testing.T) *remoteFixture {
	t.Helper()
	f := &remoteFixture{terminalFixture: newTerminalFixture(t), ssh: sshtest.Install(t)}
	f.home = f.ssh.Home("ka")
	// Control sockets need a short directory.
	tmp, err := os.MkdirTemp("/tmp", "hvc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	// A laptop provider home that must never be read for a remote session.
	laptop := t.TempDir()
	t.Setenv("HOME", laptop)
	writeTranscript(t, filepath.Join(laptop, ".claude", "projects", "laptop", remoteNative+".jsonl"), claudeUser("laptop-u", "p0", "human", "LAPTOP RECORD"))
	f.service.adapters[agentruntime.AgentClaude] = claude.New(claude.DefaultOptions())
	f.service.adapters[agentruntime.AgentCodex] = artcodex.New(artcodex.DefaultOptions())
	t.Cleanup(func() {
		for id := range f.service.remoteConvs {
			f.service.closeRemoteConversation(id)
		}
	})
	return f
}

func (f *remoteFixture) remoteSession(id string, kind agentruntime.AgentKind, env map[string]string) domain.Session {
	s := f.terminalSession(id, kind)
	s.SessionType = domain.SessionTypeTicket
	s.Machine, s.SSH = "ka", "ka"
	s.CurrentRun.NativeID = remoteNative
	s.CurrentRun.ProfileSnapshot.Env = env
	f.repo.listedSessions = append(f.repo.listedSessions, s)
	return s
}

func claudeUser(uuid, prompt, origin, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "promptId": prompt, "turnOrigin": origin, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

func claudeAssistant(uuid, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	return string(b)
}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func texts(items []domain.ConversationItem) string {
	var out []string
	for _, item := range items {
		out = append(out, item.Role+":"+item.Via+":"+item.Text)
	}
	return strings.Join(out, "|")
}

// History of a remote Claude worker is the transcript on its machine, under
// the root its frozen environment selects there, fetched incrementally over
// one reused SSH master; the laptop's own provider home is never read.
func TestRemoteClaudeHistoryIsReadFromTheWorkerMachine(t *testing.T) {
	f := newRemoteConversationFixture(t)
	session := f.remoteSession("rc-1", agentruntime.AgentClaude, map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(f.home, "claude-work")})
	caps := f.service.chatCapabilities(session)
	if !caps.History || !caps.Send || caps.ExecutionLocation != "remote" || caps.Machine != "ka" || caps.InputRoute != "claude_async_rewake" || !strings.Contains(caps.Limitation, "over SSH") {
		t.Fatalf("capabilities = %+v", caps)
	}
	transcript := filepath.Join(f.home, "claude-work", "projects", "-work", remoteNative+".jsonl")
	// A same-named transcript under the remote default root is not this
	// session's: the frozen CLAUDE_CONFIG_DIR selects the root.
	writeTranscript(t, filepath.Join(f.home, ".claude", "projects", "-work", remoteNative+".jsonl"), claudeUser("default-u", "p0", "human", "WRONG ROOT"))
	writeTranscript(t, transcript, claudeUser("u0", "p0", "human", "# Project: hiveryn\n\nKICKOFF and context"), claudeAssistant("a0", "READY"))

	items := f.history2(session.ID)
	if got := texts(items); got != "user:kickoff:# Project: hiveryn\n\nKICKOFF and context|assistant::READY" {
		t.Fatalf("history = %q", got)
	}
	// Appended lines arrive on the next read, over the same master.
	appendTranscript(t, transcript, claudeUser("u1", "p1", "human", "typed on the laptop"), claudeAssistant("a1", "noted"))
	items = f.history2(session.ID)
	if got := texts(items); !strings.HasSuffix(got, "|user:terminal:typed on the laptop|assistant::noted") {
		t.Fatalf("history after append = %q", got)
	}
	if fresh, masters, _ := f.ssh.Connections("ka"); fresh != 0 || masters != 1 {
		t.Fatalf("connections: fresh=%d masters=%d, want one reused master", fresh, masters)
	}
	for _, script := range f.ssh.Commands("ka") {
		if strings.Contains(script, ".jsonl") && !strings.Contains(script, "tail -c +") {
			t.Fatalf("record fetched without the incremental tail: %s", script)
		}
	}

	// An unreachable machine is an explicit read error, never an empty page.
	f.service.closeRemoteConversation(session.ID)
	f.ssh.SetDown(t, "ka", true)
	_, err := f.service.ReadConversation(context.Background(), session.ID, domain.ConversationQuery{Limit: 200})
	var convErr *domain.ConversationError
	if !errors.As(err, &convErr) || convErr.Code != domain.HistoryReadFailed || !strings.Contains(convErr.Message, "ka") {
		t.Fatalf("unreachable read = %v", err)
	}
	f.ssh.SetDown(t, "ka", false)
	if got := texts(f.history2(session.ID)); strings.Contains(got, "LAPTOP") || strings.Contains(got, "WRONG ROOT") {
		t.Fatalf("read the wrong record: %q", got)
	}
}

// The mirror moves only appended bytes, refetches a file rewritten in place or
// truncated, drops a vanished file and fetches a large backlog in chunks.
func TestRemoteRecordMirrorFollowsTheRemoteFiles(t *testing.T) {
	ssh := sshtest.Install(t)
	home := ssh.Home("ka")
	tmp, err := os.MkdirTemp("/tmp", "hvm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	chunk := remoteRecordChunk
	remoteRecordChunk = 100
	t.Cleanup(func() { remoteRecordChunk = chunk })
	codex := artcodex.New(artcodex.DefaultOptions())
	location := codex.RecordLocation(remoteNative)
	rc := &remoteConversation{files: map[string]int64{}}
	t.Cleanup(func() { _ = os.RemoveAll(rc.dir) })
	rel := "sessions/2026/10/28/rollout-2026-10-28T10-00-00-" + remoteNative + ".jsonl"
	remote := filepath.Join(home, ".codex", filepath.FromSlash(rel))
	content := strings.Repeat("a", 250) + "\n"
	writeTranscript(t, remote, strings.TrimSuffix(content, "\n"))
	other := filepath.Join(home, ".codex", "sessions", "2026", "10", "28", "rollout-2026-10-28T10-00-00-other.jsonl")
	writeTranscript(t, other, "not this thread")

	sync := func() []string {
		t.Helper()
		files, err := rc.syncMirror(context.Background(), "ka", location, nil)
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	mirrored := func() string {
		b, err := os.ReadFile(filepath.Join(rc.dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if files := sync(); len(files) != 1 || files[0] != rel || mirrored() != content {
		t.Fatalf("first sync files=%v content=%d bytes", files, len(mirrored()))
	}
	if calls := len(ssh.Commands("ka")); calls != 3 {
		t.Fatalf("251 bytes at a 100-byte chunk took %d calls, want 3", calls)
	}
	appendTranscript(t, remote, "bbb")
	sync()
	if mirrored() != content+"bbb\n" {
		t.Fatalf("append not mirrored: %q", mirrored()[240:])
	}
	// Same size, different bytes before the mirrored end: refetched whole.
	rewritten := strings.Repeat("c", 250) + "\nbbb\n"
	if err := os.WriteFile(remote, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	sync()
	if mirrored() != rewritten {
		t.Fatal("in-place rewrite not refetched")
	}
	if err := os.WriteFile(remote, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sync()
	if mirrored() != "short\n" {
		t.Fatalf("truncation not refetched: %q", mirrored())
	}
	if err := os.Remove(remote); err != nil {
		t.Fatal(err)
	}
	if files := sync(); len(files) != 0 {
		t.Fatalf("vanished file still listed: %v", files)
	}
	if _, err := os.Stat(filepath.Join(rc.dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("vanished file still mirrored: %v", err)
	}
	if !recordPathAllowed(rel, location.Patterns) || recordPathAllowed("sessions/../../etc/passwd", location.Patterns) || recordPathAllowed("/etc/passwd", location.Patterns) {
		t.Fatal("record path filter")
	}
}

// Remote OpenCode has no conversation route: capabilities say so and history
// and send fail explicitly instead of returning nothing or accepting input.
func TestRemoteOpenCodeConversationIsExplicitlyUnsupported(t *testing.T) {
	f := newRemoteConversationFixture(t)
	session := f.remoteSession("ro-1", agentruntime.AgentOpenCode, nil)
	caps := f.service.chatCapabilities(session)
	if caps.History || caps.Send || caps.InputRoute != "" || caps.ExecutionLocation != "remote" || caps.Machine != "ka" || !strings.Contains(caps.Limitation, "Remote opencode conversations are unsupported") {
		t.Fatalf("capabilities = %+v", caps)
	}
	_, err := f.service.ReadConversation(context.Background(), session.ID, domain.ConversationQuery{Limit: 10})
	var convErr *domain.ConversationError
	if !errors.As(err, &convErr) || convErr.Code != domain.HistoryUnsupported {
		t.Fatalf("history = %v", err)
	}
	sub, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "r", Text: "hello"})
	expectRefused(t, sub, err, domain.DeliverySendUnsupported, domain.SubmissionRejected)
	if len(f.ssh.Commands("ka")) != 0 {
		t.Fatal("an unsupported route reached the machine")
	}
}

// A remote Claude send hands the input to the waiter armed through the
// worker's gateway and is accepted once the transcript on the worker's
// machine has it; a remote worker's reconnect is not a provider restart.
func TestRemoteClaudeSendIsAcceptedFromTheRemoteRecord(t *testing.T) {
	f := newRemoteConversationFixture(t)
	session := f.remoteSession("rc-2", agentruntime.AgentClaude, nil)
	transcript := filepath.Join(f.home, ".claude", "projects", "-work", remoteNative+".jsonl")
	writeTranscript(t, transcript, claudeUser("u0", "p0", "human", "# Project: hiveryn\n\nKICKOFF"), claudeAssistant("a0", "READY"))
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	stop := f.armWaiter(session.ID, "Stop", "")
	// The waiter is reachable only through the attachment's tunnel: while it
	// is down (or not held by this daemon) a send is definitively refused.
	refused, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "detached", Text: "while detached"})
	expectRefused(t, refused, err, domain.DeliverySendUnavailable, domain.SubmissionRejected)
	worker := &remoteWorker{session: session, connection: "disconnected"}
	f.service.remoteMu.Lock()
	if f.service.remotes == nil {
		f.service.remotes = map[string]*remoteWorker{}
	}
	f.service.remotes[session.ID] = worker
	f.service.remoteMu.Unlock()
	refused, err = f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "disconnected", Text: "while disconnected"})
	expectRefused(t, refused, err, domain.DeliverySendUnavailable, domain.SubmissionRejected)
	if !strings.Contains(refused.Error, "attachment is disconnected") {
		t.Fatalf("refusal = %+v", refused)
	}
	worker.mu.Lock()
	worker.connection = "connected"
	worker.mu.Unlock()
	result := make(chan error, 1)
	var sub domain.Submission
	go func() {
		var err error
		sub, err = f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "phone", Text: "hello from the phone"})
		result <- err
	}()
	status, body := waiterResponse(t, stop)
	if status != http.StatusOK || !strings.Contains(body, claudeInputHeader) {
		t.Fatalf("waiter = %d %s", status, body)
	}
	var msg struct{ Text string }
	_ = json.Unmarshal([]byte(body), &msg)
	appendTranscript(t, transcript, claudeUser("u1", "p1", "task_notification", "<system-reminder>Stop hook blocking error from command: "+msg.Text+"</system-reminder>"))
	if err := <-result; err != nil || sub.Acceptance != "claude_native_record" || sub.NativeTurnID != "p1" {
		t.Fatalf("send = %+v, %v", sub, err)
	}
	items := f.history2(session.ID)
	last := items[len(items)-1]
	if last.Via != "api" || last.NativeOrigin != "task_notification" || last.Text != "hello from the phone" || last.SubmissionID != sub.ID {
		t.Fatalf("history = %s", texts(items))
	}
	// The SSH attachment was replaced by a reconnect: the provider kept
	// running in its tmux pane, so the open turn is not "restarted".
	f.setMainTerminal(session.ID, "term-reconnected")
	open := domain.TerminalSubmission{Submission: sub, HandedTerminal: "term-" + session.ID}
	if f.service.processReplaced(session, &open) {
		t.Fatal("a remote reconnect counted as a provider restart")
	}
}

// fakeAppServerEnv makes this test binary act as `codex app-server` (see
// TestHelperCodexAppServer); the fake machine's PATH finds it as `codex`.
const fakeAppServerEnv = "HIVERYN_TEST_FAKE_CODEX_APP_SERVER"

func installRemoteCodex(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = app-server ] || exit 2\n" + fakeAppServerEnv + "=1 exec " + os.Args[0] + " -test.run='^TestHelperCodexAppServer$'\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestHelperCodexAppServer is a minimal app-server over stdio: initialize,
// thread/queue/list and thread/queue/add against $CODEX_HOME/queue.json. A
// $CODEX_HOME/lose-add file makes it queue the item and exit without answering.
func TestHelperCodexAppServer(t *testing.T) {
	if os.Getenv(fakeAppServerEnv) != "1" {
		t.Skip("helper process")
	}
	home := os.Getenv("CODEX_HOME")
	queuePath := filepath.Join(home, "queue.json")
	var queue []map[string]string
	if b, err := os.ReadFile(queuePath); err == nil {
		_ = json.Unmarshal(b, &queue)
	}
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "thread/queue/list":
			var data []map[string]string
			for _, item := range queue {
				if item["threadId"] == req.Params["threadId"] {
					data = append(data, map[string]string{"id": item["id"], "clientUserMessageId": item["clientUserMessageId"]})
				}
			}
			result = map[string]any{"data": data, "nextCursor": nil}
		case "thread/queue/add":
			item := map[string]string{"id": fmt.Sprintf("qs-%d", len(queue)+1), "threadId": fmt.Sprint(req.Params["threadId"]), "clientUserMessageId": fmt.Sprint(req.Params["clientUserMessageId"]), "home": os.Getenv("HOME")}
			queue = append(queue, item)
			b, _ := json.Marshal(queue)
			_ = os.WriteFile(queuePath, b, 0o600)
			if _, err := os.Stat(filepath.Join(home, "lose-add")); err == nil {
				os.Exit(0)
			}
			result = map[string]any{"queuedSubmission": map[string]string{"id": item["id"]}}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
	os.Exit(0)
}

// A remote Codex send runs app-server on the worker's machine under its frozen
// CODEX_HOME there; a response lost after the queue took it is
// DELIVERY_UNKNOWN, reconciled from the remote queue and replayed by request
// id, never sent again; an unreachable machine is a known refusal.
func TestRemoteCodexSendQueuesOnTheWorkerMachine(t *testing.T) {
	f := newRemoteConversationFixture(t)
	installRemoteCodex(t)
	codexHome := filepath.Join(f.home, "codex-work")
	session := f.remoteSession("rx-1", agentruntime.AgentCodex, map[string]string{"CODEX_HOME": codexHome})
	rollout := filepath.Join(codexHome, "sessions", "2026", "10", "28", "rollout-2026-10-28T10-00-00-"+remoteNative+".jsonl")
	writeTranscript(t, rollout,
		`{"timestamp":"2026-10-28T10:00:00Z","type":"session_meta","payload":{"id":"`+remoteNative+`","timestamp":"2026-10-28T10:00:00Z"}}`,
		`{"timestamp":"2026-10-28T10:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t0","turn_attribution":{"turn_trigger":"user"}}}`,
		`{"timestamp":"2026-10-28T10:00:01Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t0","item":{"type":"UserMessage","id":"um0","content":[{"type":"text","text":"# Project: hiveryn\n\nKICKOFF"}]}}}`)
	caps := f.service.chatCapabilities(session)
	if !caps.Send || caps.InputRoute != "codex_thread_queue" || caps.Machine != "ka" {
		t.Fatalf("capabilities = %+v", caps)
	}

	sub, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "q-1", Text: "queued while busy"})
	if err != nil || sub.Status != domain.SubmissionAccepted || sub.Acceptance != "codex_thread_queue" {
		t.Fatalf("send = %+v, %v", sub, err)
	}
	b, err := os.ReadFile(filepath.Join(codexHome, "queue.json"))
	if err != nil || !strings.Contains(string(b), sub.ID) || !strings.Contains(string(b), `"home":"`+f.home+`"`) {
		t.Fatalf("remote queue = %s, %v", b, err)
	}

	// Lost response after the queue took it.
	if err := os.WriteFile(filepath.Join(codexHome, "lose-add"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lost, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "q-2", Text: "response lost"})
	expectRefused(t, lost, err, domain.DeliveryUnknown, domain.SubmissionUnknown)
	_ = os.Remove(filepath.Join(codexHome, "lose-add"))
	f.dispatch()
	if got := f.submission(session.ID, lost.ID); got.Status != domain.SubmissionAccepted || got.AcceptedAt == nil {
		t.Fatalf("reconciled = %+v", got)
	}
	replay, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "q-2", Text: "response lost"})
	if err != nil || replay.ID != lost.ID || replay.Status != domain.SubmissionAccepted {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	b, _ = os.ReadFile(filepath.Join(codexHome, "queue.json"))
	if n := strings.Count(string(b), lost.ID); n != 1 {
		t.Fatalf("lost-response input queued %d times", n)
	}

	// The record shows it ran as a queue turn: completed from the rollout.
	appendTranscript(t, rollout,
		`{"timestamp":"2026-10-28T10:01:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1","turn_attribution":{"turn_trigger":"queue"}}}`,
		`{"timestamp":"2026-10-28T10:01:00Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"um1","client_id":"`+sub.ID+`","content":[{"type":"text","text":"queued while busy"}]}}}`,
		`{"timestamp":"2026-10-28T10:01:01Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`)
	f.dispatch()
	if got := f.submission(session.ID, sub.ID); got.Status != domain.SubmissionCompleted || got.NativeTurnID != "t1" {
		t.Fatalf("completed = %+v", got)
	}

	// Unreachable before anything was sent: a definitive refusal.
	f.service.closeRemoteConversation(session.ID)
	f.ssh.SetDown(t, "ka", true)
	down, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "q-3", Text: "machine down"})
	expectRefused(t, down, err, domain.DeliveryRejected, domain.SubmissionRejected)
	if !strings.Contains(down.Error, "SSH connection to machine ka") {
		t.Fatalf("refusal = %+v", down)
	}
}
