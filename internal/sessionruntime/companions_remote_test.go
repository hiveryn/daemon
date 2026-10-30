package sessionruntime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/agentruntime"
	artcodex "github.com/hiveryn/agentruntime/adapter/codex"
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
