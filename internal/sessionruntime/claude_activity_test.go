package sessionruntime

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/domain"
)

// A retried waiter (released with 503 by a restarting receiver, or cut off by
// a lost tunnel) re-posts the envelope of the hook that started it. Its
// arrival is a connection coming up, not the agent stopping: newer working
// evidence stands.
func TestRetriedClaudeWaiterDoesNotReopenAStaleIdleGate(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("a-stale", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}

	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	stop := f.armWaiter(session.ID, "Stop", "")
	// The user types in the terminal: a new turn runs.
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusWorking})
	// The waiter is released to retry and re-arms with its old Stop envelope.
	f.service.messages.mailbox.Interrupt(session.ID)
	if status, _ := waiterResponse(t, stop); status != http.StatusServiceUnavailable {
		t.Fatalf("interrupted waiter status %d", status)
	}
	retry := f.armWaiter(session.ID, "Stop", "")

	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "stale", Text: "must not join the running turn"})
	expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected)
	m := f.enqueue("notice")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "agent busy")
	if _, armed := f.service.messages.mailbox.Armed(session.ID); !armed {
		t.Fatal("the retried waiter was handed something while the agent works")
	}

	// The running turn's genuine Stop reopens the gate: the next send is
	// handed to the armed waiter.
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusIdle})
	f.dispatch()
	f.expect(m.ID, domain.AgentMessageHanded, "handed to the Stop waiter")
	if status, _ := waiterResponse(t, retry); status != http.StatusOK {
		t.Fatalf("retried waiter status %d", status)
	}
}

// waitProbes waits for background native-record idle probes to finish.
func (f *messageFixture) waitProbes() {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.service.messages.mu.Lock()
		n := len(f.service.messages.probing)
		f.service.messages.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("idle probe still running")
}

// A daemon that restarted while a worker kept running holds no hook evidence:
// the retried waiter re-arms, and only the native record can say the agent
// stopped. A turn without an end is busy; an unreadable record unavailable;
// a recorded turn end recovers sending without a worker restart.
func TestUnknownClaudeActivityIsEstablishedOnlyFromTheNativeRecord(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	session := f.terminalSession("a-restart", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	claude := f.history[agentruntime.AgentClaude]
	at := f.now
	claude.record(
		agentruntime.HistoryItem{ID: "u1", TurnID: "p1", Role: "user", NativeOrigin: "human", Text: "first", At: at},
		agentruntime.HistoryItem{ID: "a1:0", TurnID: "p1", Role: "assistant", Status: "completed", Text: "done", At: at},
		agentruntime.HistoryItem{ID: "s1", TurnID: "p1", Role: "system", Status: "completed", TurnEnd: true, At: at},
		// Typed while the daemon was down: still running.
		agentruntime.HistoryItem{ID: "u2", TurnID: "p2", Role: "user", NativeOrigin: "human", Text: "second", At: at},
	)
	retry := f.armWaiter(session.ID, "Stop", "")
	f.waitProbes()
	if _, known, _ := f.service.messageActivity(session.ID); known {
		t.Fatal("a running turn established activity")
	}
	sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "running", Text: "x"})
	refused := expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected)
	if !strings.Contains(refused.Error, "no completed turn after the latest input") {
		t.Fatalf("refusal = %s", refused.Error)
	}
	m := f.enqueue("notice")
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "agent activity unknown")

	claude.mu.Lock()
	claude.err = errors.New("record unreachable")
	claude.mu.Unlock()
	sub, err = f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "unreadable", Text: "x"})
	refused = expectRefused(t, sub, err, domain.DeliverySendUnavailable, domain.SubmissionRejected)
	if !strings.Contains(refused.Error, "record unreachable") {
		t.Fatalf("refusal = %s", refused.Error)
	}
	claude.mu.Lock()
	claude.err = nil
	claude.mu.Unlock()
	if _, armed := f.service.messages.mailbox.Armed(session.ID); !armed {
		t.Fatal("a refused send took the waiter")
	}

	// The turn ended while its Stop hook was lost: the record says so.
	claude.record(
		agentruntime.HistoryItem{ID: "a2:0", TurnID: "p2", Role: "assistant", Status: "completed", Text: "done", At: at},
		agentruntime.HistoryItem{ID: "s2", TurnID: "p2", Role: "system", Status: "completed", TurnEnd: true, At: at},
	)
	result := make(chan domain.Submission, 1)
	go func() {
		sub, err := f.service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "idle", Text: "now"})
		if err != nil {
			t.Errorf("send: %v", err)
		}
		result <- sub
	}()
	status, body := waiterResponse(t, retry)
	if status != http.StatusOK || !strings.Contains(body, "now") {
		t.Fatalf("waiter = %d %s", status, body)
	}
	id := domain.SubmissionIdentity(session.ID, "idle")
	claude.record(agentruntime.HistoryItem{ID: "u3", TurnID: "p3", Role: "user", NativeOrigin: "task_notification", Text: claudeInputHeader + id + " — x:\n\nnow", At: at.Add(time.Second)})
	if accepted := <-result; accepted.Status != domain.SubmissionRunning || accepted.Acceptance != "claude_native_record" {
		t.Fatalf("accepted = %+v", accepted)
	}
	// The notice was not handed into that turn as well.
	f.dispatch()
	f.expect(m.ID, domain.AgentMessagePending, "")
	if got := f.get(m.ID); got.Attempts != 0 {
		t.Fatalf("notice handed alongside the input: %+v", got)
	}
}

// A hook event that arrives while the record is read is newer than what the
// read saw: the record's idle verdict is dropped.
func TestHookEventDuringIdleProbeWins(t *testing.T) {
	f := newTerminalFixture(t)
	session := f.terminalSession("a-race", agentruntime.AgentClaude)
	f.repo.listedSessions = []domain.Session{session}
	claude := f.history[agentruntime.AgentClaude]
	claude.record(
		agentruntime.HistoryItem{ID: "u1", TurnID: "p1", Role: "user", NativeOrigin: "human", Text: "first", At: f.now},
		agentruntime.HistoryItem{ID: "s1", TurnID: "p1", Role: "system", Status: "completed", TurnEnd: true, At: f.now},
	)
	claude.onRead = func() {
		f.service.observeAgentStatusForMessages(agentruntime.Event{ID: session.ID, Status: agentruntime.StatusWorking})
	}
	f.armWaiter(session.ID, "Stop", "")
	f.waitProbes()
	if idle, known, _ := f.service.messageActivity(session.ID); idle || !known {
		t.Fatalf("gate = idle %v known %v; want the hook's busy", idle, known)
	}
	sub, err := f.service.SubmitMessage(context.Background(), session.ID, domain.SubmissionRequest{RequestID: "race", Text: "x"})
	expectRefused(t, sub, err, domain.DeliverySessionBusy, domain.SubmissionRejected)
}

// SessionStart is part of the ordered hook stream: a resumed or cleared
// conversation sits at its prompt, a startup runs its kickoff turn next.
func TestSessionStartHookSetsTheGate(t *testing.T) {
	f := newMessageFixture(t)
	for source, want := range map[string]bool{"resume": true, "clear": true, "startup": false} {
		id := "s-" + source
		f.service.observeAgentStatusForMessages(agentruntime.Event{ID: id, Status: agentruntime.StatusWorking})
		if source != "startup" {
			f.service.observeAgentStatusForMessages(agentruntime.Event{ID: id, Agent: agentruntime.AgentClaude, Status: agentruntime.StatusStarting, Metadata: map[string]string{"source": source}})
		} else {
			f.service.observeAgentStatusForMessages(agentruntime.Event{ID: id, Status: agentruntime.StatusIdle})
			f.service.observeAgentStatusForMessages(agentruntime.Event{ID: id, Agent: agentruntime.AgentClaude, Status: agentruntime.StatusStarting, Metadata: map[string]string{"source": source}})
		}
		if idle, known, _ := f.service.messageActivity(id); idle != want || !known {
			t.Fatalf("%s: idle %v known %v", source, idle, known)
		}
	}
	// A compaction says nothing about the prompt.
	f.service.observeAgentStatusForMessages(agentruntime.Event{ID: "s-compact", Agent: agentruntime.AgentClaude, Status: agentruntime.StatusStarting, Metadata: map[string]string{"source": "compact"}})
	if _, known, _ := f.service.messageActivity("s-compact"); known {
		t.Fatal("compact set the gate")
	}
}
