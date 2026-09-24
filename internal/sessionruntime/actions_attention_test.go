package sessionruntime

import (
	"context"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	artcodex "github.com/hiveryn/agentruntime/adapter/codex"

	"github.com/hiveryn/daemon/internal/domain"
)

// detectingAdapter is the fake adapter with the real Codex attention
// detector, so the fixture launches without a provider CLI but recognizes
// Codex's actual prompts.
type detectingAdapter struct {
	*fakeAdapter
	detector *artcodex.Adapter
}

func (d detectingAdapter) InspectScreen(lines []string) *agentruntime.Attention {
	return d.detector.InspectScreen(lines)
}

func (d detectingAdapter) AttentionCoverage() string { return d.detector.AttentionCoverage() }

// codexTrustDialog is Codex's folder-trust dialog, drawn the way Codex draws
// it: word by word with cursor moves.
const codexTrustDialog = "\x1b[1;1H\x1b[J\x1b[2;1H  Folder\x1b[2;10Haccess\x1b[5;3HTrust\x1b[5;9Hthis\x1b[5;14Hfolder?\x1b[8;1H›\x1b[8;3H1.\x1b[8;6HTrust\x1b[8;12Hand\x1b[8;16Hcontinue\x1b[9;3H2.\x1b[9;6HQuit\x1b[11;3Henter\x1b[11;9Hcontinue\x1b[11;18H·\x1b[11;20Hesc\x1b[11;24Hquit"

func (f *actionFixture) detectAttention() {
	f.service.adapters[agentruntime.AgentCodex] = detectingAdapter{fakeAdapter: f.adapter, detector: artcodex.New(artcodex.DefaultOptions())}
}

// mainOutput writes output to the current main terminal as its agent would.
func (f *actionFixture) mainOutput(t *testing.T, output string) {
	t.Helper()
	spec := f.terminal.startSpecs[len(f.terminal.startSpecs)-1]
	if spec.Observer == nil {
		t.Fatal("the action main terminal has no observer")
	}
	spec.Observer.Output([]byte(output))
}

func waitResult(t *testing.T, f *actionFixture, sessionID, executionID string, timeout time.Duration) <-chan domain.ActionWaitResult {
	t.Helper()
	done := make(chan domain.ActionWaitResult, 1)
	go func() {
		res, err := f.service.WaitForActionResult(context.Background(), sessionID, executionID, timeout)
		if err != nil {
			t.Errorf("wait: %v", err)
		}
		done <- res
	}()
	// Let the wait take its initial reading first.
	time.Sleep(50 * time.Millisecond)
	return done
}

func receive(t *testing.T, ch <-chan domain.ActionWaitResult) domain.ActionWaitResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
		return domain.ActionWaitResult{}
	}
}

func TestActionResultReportsAndWakesOnTerminalAttention(t *testing.T) {
	f := newActionFixture(t)
	f.detectAttention()
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()

	pending := f.request(t, arch.ID, "demo", "go")
	if pending.Attention.State != domain.ActionAttentionUnavailable {
		t.Fatalf("pending attention = %+v, want unavailable", pending.Attention)
	}
	if err := f.approve(arch.ID, pending.ExecutionID, "codex"); err != nil {
		t.Fatal(err)
	}
	running := f.result(t, arch.ID, pending.ExecutionID)
	if running.Status != domain.ActionRunRunning || running.Attention.State != domain.ActionAttentionNoneDetected || running.Attention.Coverage == "" {
		t.Fatalf("running result = %+v, want none_detected with coverage", running)
	}

	// The trust prompt appearing wakes a waiter, well before its timeout.
	start := time.Now()
	done := waitResult(t, f, arch.ID, pending.ExecutionID, 10*time.Second)
	f.mainOutput(t, codexTrustDialog)
	res := receive(t, done)
	if !res.Changed || res.TimedOut || res.Result.Status != domain.ActionRunRunning || time.Since(start) > 3*time.Second {
		t.Fatalf("wait on attention = %+v after %v", res, time.Since(start))
	}
	got := res.Result.Attention
	if got.State != domain.ActionAttentionInputRequired || got.Reason != artcodex.AttentionFolderTrust || got.Source != domain.ActionAttentionSourceTerminal || got.Since == nil {
		t.Fatalf("attention = %+v, want terminal folder_trust", got)
	}
	// The Actions window sees it on the execution record, which stays running.
	run, err := f.service.GetActionRun(ctx, pending.ExecutionID)
	if err != nil || run.Status != domain.ActionRunRunning || run.Attention == nil || run.Attention.Reason != artcodex.AttentionFolderTrust {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// Waiting again on the unchanged condition does not return at once.
	start = time.Now()
	res, err = f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, 400*time.Millisecond)
	if err != nil || res.Changed || !res.TimedOut || time.Since(start) < 350*time.Millisecond {
		t.Fatalf("repeated wait = %+v, %v after %v", res, err, time.Since(start))
	}

	// The agent exits and is resumed: the new agent's attention starts over,
	// and the cleared attention wakes a waiter.
	done = waitResult(t, f, arch.ID, pending.ExecutionID, 10*time.Second)
	f.service.handleTerminalExit(terminalExit{SessionID: run.SessionID, TerminalID: "old"})
	res = receive(t, done)
	if !res.Changed || res.Result.Attention.State != domain.ActionAttentionNoneDetected {
		t.Fatalf("wait across resume = %+v, want cleared", res)
	}
}

func TestActionAttentionFollowsHookPromptsAndSessionEnd(t *testing.T) {
	f := newActionFixture(t)
	f.detectAttention()
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	launched := f.launch(t, "demo", "go")
	sessionID := launched.Session.ID

	f.service.handleReceiverEvent(agentruntime.Event{ID: sessionID, NativeID: "native", Agent: agentruntime.AgentCodex, Status: agentruntime.StatusAwaitingInput, Message: "permission requested", Tool: "Bash", At: time.Now().UTC()})
	run, _ := f.service.GetActionRun(ctx, launched.Run.ID)
	if run.Attention == nil || run.Attention.State != domain.ActionAttentionInputRequired || run.Attention.Source != domain.ActionAttentionSourceHook {
		t.Fatalf("attention after a hook prompt = %+v", run.Attention)
	}
	// Activity is reported separately and unchanged in meaning.
	session, _ := f.sessions.GetSession(ctx, sessionID)
	if session.CurrentRun.AgentStatus != domain.AgentStatusWaiting {
		t.Fatalf("agent status = %q", session.CurrentRun.AgentStatus)
	}

	f.service.handleReceiverEvent(agentruntime.Event{ID: sessionID, NativeID: "native", Agent: agentruntime.AgentCodex, Status: agentruntime.StatusWorking, At: time.Now().UTC()})
	run, _ = f.service.GetActionRun(ctx, launched.Run.ID)
	if run.Attention == nil || run.Attention.State != domain.ActionAttentionNoneDetected {
		t.Fatalf("attention after the prompt was answered = %+v", run.Attention)
	}

	// Ending the execution drops the monitor and its state.
	f.service.handleReceiverEvent(agentruntime.Event{ID: sessionID, NativeID: "native", Agent: agentruntime.AgentCodex, Status: agentruntime.StatusAwaitingInput, Message: "permission requested", At: time.Now().UTC()})
	if _, err := f.service.CancelActionRun(ctx, launched.Run.ID); err != nil {
		t.Fatal(err)
	}
	run, _ = f.service.GetActionRun(ctx, launched.Run.ID)
	if run.Attention != nil || f.service.attentionMonitor(sessionID) != nil {
		t.Fatalf("ended execution kept attention: %+v", run.Attention)
	}
}

// A session start is not work: a resumed agent reports it and then waits at
// its prompt, so activity stays unknown instead of claiming active.
func TestSessionStartDoesNotReportActivity(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	launched := f.launch(t, "demo", "go")
	f.service.handleReceiverEvent(agentruntime.Event{ID: launched.Session.ID, NativeID: "native", Agent: agentruntime.AgentCodex, Status: agentruntime.StatusStarting, At: time.Now().UTC()})
	session, _ := f.sessions.GetSession(context.Background(), launched.Session.ID)
	if session.CurrentRun.AgentStatus != "" {
		t.Fatalf("agent status after session start = %q, want unreported", session.CurrentRun.AgentStatus)
	}
}
