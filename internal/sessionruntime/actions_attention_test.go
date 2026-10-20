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

// nextEvent returns the next action event for executionID, or fails.
func nextEvent(t *testing.T, sub domain.ActionEventSubscription, executionID string) domain.ActionEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-sub.C():
			if !ok {
				t.Fatal("subscription closed")
			}
			if event.ExecutionID == executionID {
				return event
			}
		case <-deadline:
			t.Fatal("no action event")
		}
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
	if err := f.approve(arch.ID, pending.ExecutionID); err != nil {
		t.Fatal(err)
	}
	running := f.result(t, arch.ID, pending.ExecutionID)
	if running.Status != domain.ActionRunRunning || running.Attention.State != domain.ActionAttentionNoneDetected || running.Attention.Coverage == "" {
		t.Fatalf("running result = %+v, want none_detected with coverage", running)
	}

	// The trust prompt appearing is announced on the action stream (the
	// Actions window's) while the execution stays running, and the result
	// reports it.
	sub := f.service.SubscribeActionEvents()
	defer sub.Close()
	f.mainOutput(t, codexTrustDialog)
	if event := nextEvent(t, sub, pending.ExecutionID); event.Status != domain.ActionRunRunning {
		t.Fatalf("attention event = %+v, want running", event)
	}
	got := f.result(t, arch.ID, pending.ExecutionID).Attention
	if got.State != domain.ActionAttentionInputRequired || got.Reason != artcodex.AttentionFolderTrust || got.Source != domain.ActionAttentionSourceTerminal || got.Since == nil {
		t.Fatalf("attention = %+v, want terminal folder_trust", got)
	}
	run, err := f.service.GetActionRun(ctx, pending.ExecutionID)
	if err != nil || run.Status != domain.ActionRunRunning || run.Attention == nil || run.Attention.Reason != artcodex.AttentionFolderTrust {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// The agent exits and is resumed: the new agent's attention starts over,
	// and the cleared attention is announced too.
	f.service.handleTerminalExit(terminalExit{SessionID: run.SessionID, TerminalID: "old"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		nextEvent(t, sub, pending.ExecutionID)
		if f.result(t, arch.ID, pending.ExecutionID).Attention.State == domain.ActionAttentionNoneDetected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attention not cleared across resume")
		}
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
