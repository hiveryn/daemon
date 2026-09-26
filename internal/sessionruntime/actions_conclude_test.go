package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

type concludeCall struct {
	res domain.IntentResolution[domain.ActionRun]
	err error
}

// startConclude runs the action agent's blocking concludeSession in the
// background, as the agent's MCP call would.
func (f *actionFixture) startConclude(sessionID string, req domain.ConcludeActionRequest) <-chan concludeCall {
	done := make(chan concludeCall, 1)
	go func() {
		res, err := f.service.ConcludeAction(context.Background(), sessionID, req)
		done <- concludeCall{res: res, err: err}
	}()
	return done
}

// pendingConclusion waits for the session's conclusion intent to be shown, or
// for the call to return first (a validation error never shows one).
func (f *actionFixture) pendingConclusion(t *testing.T, sessionID string, done <-chan concludeCall) (string, *concludeCall) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case call := <-done:
			return "", &call
		default:
		}
		for _, id := range f.service.intents.PendingForSession(sessionID) {
			if in, ok := f.service.intents.GetForSession(sessionID, id); ok && in.Type == domain.IntentTypeConcludeSession {
				return id, nil
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no conclusion intent raised for session %s", sessionID)
	return "", nil
}

func awaitConclude(t *testing.T, done <-chan concludeCall) concludeCall {
	t.Helper()
	select {
	case call := <-done:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("concludeSession did not return")
		return concludeCall{}
	}
}

// conclude is the approved path: the user approves the proposed conclusion.
func (f *actionFixture) conclude(t *testing.T, sessionID string, req domain.ConcludeActionRequest) (domain.ActionRun, error) {
	t.Helper()
	done := f.startConclude(sessionID, req)
	id, returned := f.pendingConclusion(t, sessionID, done)
	if returned != nil {
		return domain.ActionRun{}, returned.err
	}
	if _, err := f.service.ApproveIntent(context.Background(), sessionID, id, nil); err != nil {
		return domain.ActionRun{}, err
	}
	call := awaitConclude(t, done)
	if call.err != nil {
		return domain.ActionRun{}, call.err
	}
	if call.res.Outcome != domain.IntentOutcomeApproved {
		t.Fatalf("conclusion outcome = %q (%s), want approved", call.res.Outcome, call.res.Reason)
	}
	return call.res.Result, nil
}

func (f *actionFixture) deliver(t *testing.T, run domain.ActionRun) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *actionFixture) requireRunning(t *testing.T, result domain.LaunchActionResult) {
	t.Helper()
	run, err := f.service.GetActionRun(context.Background(), result.Run.ID)
	if err != nil || run.Status != domain.ActionRunRunning || run.Summary != "" || run.EndedAt != nil {
		t.Fatalf("execution = %+v, %v; want still running without a result", run, err)
	}
	session, err := f.sessions.GetSession(context.Background(), result.Session.ID)
	if err != nil || session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		t.Fatalf("session = %+v, %v; want still running", session, err)
	}
}

func TestActionConclusionWaitsForApprovalAndShowsItsProposal(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")
	f.deliver(t, result.Run)

	done := f.startConclude(result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "  AMS 19.7% vs LDN 8.5%  "})
	id, returned := f.pendingConclusion(t, result.Session.ID, done)
	if returned != nil {
		t.Fatalf("conclude returned before approval: %+v", returned)
	}
	f.requireRunning(t, result)
	session, _ := f.sessions.GetSession(ctx, result.Session.ID)
	if session.CurrentRun.AgentStatus != domain.AgentStatusWaiting {
		t.Fatalf("agent status = %q, want waiting while the conclusion is pending", session.CurrentRun.AgentStatus)
	}

	intent, _ := f.service.intents.GetForSession(result.Session.ID, id)
	if intent.Policy != domain.IntentPolicyWaitThenAllow || intent.Summary != "AMS 19.7% vs LDN 8.5%" {
		t.Fatalf("intent = %+v, want the conclusion policy and trimmed summary", intent)
	}
	if intent.Origin.SessionType != domain.SessionTypeAction || intent.Origin.SessionID != result.Session.ID || intent.Origin.ArchitectKey != "" {
		t.Fatalf("origin = %+v", intent.Origin)
	}
	want := map[string]any{"body": "AMS 19.7% vs LDN 8.5%", "outcome": "completed", "action": "demo", "execution_id": result.Run.ID, "output_dir": result.Run.OutputDir}
	for k, v := range want {
		if intent.Payload[k] != v {
			t.Fatalf("payload[%s] = %#v, want %#v", k, intent.Payload[k], v)
		}
	}
	events, err := f.service.ListSessionEvents(ctx, result.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	required := false
	for _, e := range events {
		if e.Type == sessionEventTypeIntent && e.Status == sessionEventStatusReqd && e.Raw["intent_id"] == id {
			required = true
		}
	}
	if !required {
		t.Fatalf("no intent/required event for %s: %+v", id, events)
	}

	if _, err := f.service.ApproveIntent(ctx, result.Session.ID, id, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	call := awaitConclude(t, done)
	if call.err != nil || call.res.Outcome != domain.IntentOutcomeApproved || call.res.Result.Status != domain.ActionRunCompleted || call.res.Result.Summary != "AMS 19.7% vs LDN 8.5%" {
		t.Fatalf("approved conclusion = %+v, %v", call.res, call.err)
	}
	if _, err := f.sessions.GetSession(ctx, result.Session.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("session survived approved conclusion: %v", err)
	}
}

func TestActionConclusionAutoApprovesAfterTheWaitWindow(t *testing.T) {
	f := newActionFixture(t)
	f.service.cfg.IntentWaitTimeout = 1
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")

	res, err := f.service.ConcludeAction(ctx, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "collector missing"})
	if err != nil || res.Outcome != domain.IntentOutcomeAutoApproved {
		t.Fatalf("conclusion = %+v, %v; want auto_approved", res, err)
	}
	if res.Result.Status != domain.ActionRunFailed || res.Result.Summary != "collector missing" {
		t.Fatalf("auto-approved execution = %+v", res.Result)
	}
	if _, err := f.sessions.GetSession(ctx, result.Session.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("session survived auto-approved conclusion: %v", err)
	}
}

func TestActionConclusionDenialKeepsExecutionRunningForFollowUp(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")
	f.deliver(t, result.Run)

	done := f.startConclude(result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "first pass"})
	id, _ := f.pendingConclusion(t, result.Session.ID, done)
	if err := f.service.DenyIntent(ctx, result.Session.ID, id, "also compare FRA"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	call := awaitConclude(t, done)
	if call.err != nil || call.res.Outcome != domain.IntentOutcomeDeniedByUser || call.res.Reason != "also compare FRA" {
		t.Fatalf("denied conclusion = %+v, %v", call.res, call.err)
	}
	f.requireRunning(t, result)

	// The agent follows up and proposes a new conclusion, which applies.
	run, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "AMS, LDN and FRA compared"})
	if err != nil || run.Status != domain.ActionRunCompleted || run.Summary != "AMS, LDN and FRA compared" {
		t.Fatalf("follow-up conclusion = %+v, %v", run, err)
	}
}

func TestActionConclusionRechecksDeliveryWhenApplied(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")
	f.deliver(t, result.Run)

	done := f.startConclude(result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "done"})
	id, _ := f.pendingConclusion(t, result.Session.ID, done)
	if err := os.Remove(filepath.Join(result.Run.OutputDir, "summary.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ApproveIntent(ctx, result.Session.ID, id, nil); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("approve err = %v, want the empty output refused", err)
	}
	call := awaitConclude(t, done)
	if call.err != nil || call.res.Outcome != domain.IntentOutcomeError || !strings.Contains(call.res.Reason, "is empty") {
		t.Fatalf("conclusion = %+v, %v; want an error outcome", call.res, call.err)
	}
	f.requireRunning(t, result)
}

func TestActionConclusionPendingWhenCancelledEndsTheRequest(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "demo", "go")

	done := f.startConclude(result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "stuck"})
	id, _ := f.pendingConclusion(t, result.Session.ID, done)
	if _, err := f.service.CancelActionRun(ctx, result.Run.ID); err != nil {
		t.Fatal(err)
	}
	call := awaitConclude(t, done)
	if call.err != nil || call.res.Outcome != domain.IntentOutcomeError {
		t.Fatalf("conclusion after cancel = %+v, %v; want error outcome", call.res, call.err)
	}
	if _, err := f.service.ApproveIntent(ctx, result.Session.ID, id, nil); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("late approve err = %v, want not found", err)
	}
	run, _ := f.service.GetActionRun(ctx, result.Run.ID)
	if run.Status != domain.ActionRunFailed || run.Error != "cancelled by user" || run.Summary != "" {
		t.Fatalf("execution = %+v, want the cancel, not the proposed conclusion", run)
	}
}

// An architect waiting on its requested execution sees no final result while
// the agent's conclusion is pending approval, and wakes when it is applied.
func TestArchitectSeesNoFinalResultWhileActionConclusionIsPending(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	pending := f.request(t, arch.ID, "demo", "go")
	if err := f.approve(arch.ID, pending.ExecutionID); err != nil {
		t.Fatal(err)
	}
	run, _ := f.service.GetActionRun(ctx, pending.ExecutionID)
	f.deliver(t, run)

	done := f.startConclude(run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "delivered"})
	id, _ := f.pendingConclusion(t, run.SessionID, done)

	if got := f.result(t, arch.ID, pending.ExecutionID); got.Status != domain.ActionRunRunning || got.Summary != "" || got.EndedAt != nil {
		t.Fatalf("result while pending = %+v, want running", got)
	}
	waited, err := f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, 200*time.Millisecond)
	if err != nil || !waited.TimedOut || waited.Changed || waited.Result.Status != domain.ActionRunRunning {
		t.Fatalf("wait while pending = %+v, %v; want timed out still running", waited, err)
	}

	woke := make(chan domain.ActionWaitResult, 1)
	go func() {
		w, _ := f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, 5*time.Second)
		woke <- w
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := f.service.ApproveIntent(ctx, run.SessionID, id, nil); err != nil {
		t.Fatal(err)
	}
	awaitConclude(t, done)
	select {
	case w := <-woke:
		if !w.Changed || w.Result.Status != domain.ActionRunCompleted || w.Result.Summary != "delivered" {
			t.Fatalf("wait after approval = %+v", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not wake on the applied conclusion")
	}
}
