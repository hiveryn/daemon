package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/domain"
)

// notices returns the project's action-ended messages, newest first.
func (f *actionFixture) notices(t *testing.T, architectKey string) []domain.AgentMessage {
	t.Helper()
	listed, err := f.service.ListAgentMessages(context.Background(), architectKey)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	out := []domain.AgentMessage{}
	for _, m := range listed {
		if m.Kind == domain.AgentMessageActionEnded {
			out = append(out, m)
		}
	}
	return out
}

// onlyNotice is the project's single action-ended notice, which must be
// deliverable (pending) and about executionID.
func (f *actionFixture) onlyNotice(t *testing.T, architectKey, executionID string) domain.AgentMessage {
	t.Helper()
	got := f.notices(t, architectKey)
	if len(got) != 1 {
		t.Fatalf("notices = %+v, want exactly one", got)
	}
	notice := got[0]
	if notice.State != domain.AgentMessagePending || notice.CommittedAt == nil || notice.DedupeKey != "action_ended:"+executionID || notice.Payload["execution_id"] != executionID {
		t.Fatalf("notice = %+v, want a deliverable notice for %s", notice, executionID)
	}
	return notice
}

// requestAndStart raises an approved request from sessionID and returns the
// running execution.
func (f *actionFixture) requestAndStart(t *testing.T, sessionID, name string) domain.ActionRun {
	t.Helper()
	pending := f.request(t, sessionID, name, "go")
	if err := f.approve(sessionID, pending.ExecutionID); err != nil {
		t.Fatal(err)
	}
	res, err := pending.wait(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.IntentOutcomeApproved || res.Notification == nil {
		t.Fatalf("approved response = %+v, want a notification statement", res)
	}
	run, err := f.service.GetActionRun(context.Background(), pending.ExecutionID)
	if err != nil || run.Status != domain.ActionRunRunning {
		t.Fatalf("execution = %+v, %v; want running", run, err)
	}
	return run
}

func (f *actionFixture) writeArtifactFreeAction(t *testing.T, name string) {
	t.Helper()
	f.writeAction(t, name, "name: "+name+"\ndescription: Looks things up.\nsuggestions: [Look up one thing]\n", "Handle this request:\n\n{{prompt}}\n")
}

// deliverable swaps in a codex adapter that can receive messages (push,
// ungated), as the real one does, and returns it.
func (f *actionFixture) deliverable(t *testing.T) *fakeMessageAdapter {
	t.Helper()
	adapter := &fakeMessageAdapter{kind: agentruntime.AgentCodex, mode: agentruntime.MessageDeliveryPush}
	f.service.adapters[agentruntime.AgentCodex] = adapter
	return adapter
}

func TestArchitectIsNotifiedOnceItsActionConclusionIsPublished(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	run := f.requestAndStart(t, arch.ID, "demo")
	f.deliver(t, run)
	if _, err := f.service.DeliverActionArtifacts(ctx, run.SessionID, run.OutputDir); err != nil {
		t.Fatal(err)
	}
	// Preparation and a pending conclusion announce nothing.
	done := f.startConclude(run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "compared"})
	id, _ := f.pendingConclusion(t, run.SessionID, done)
	if got := f.notices(t, "alpha"); len(got) != 0 {
		t.Fatalf("notice before approval: %+v", got)
	}
	// Denied: the agent keeps running and nobody is told.
	if err := f.service.DenyIntent(ctx, run.SessionID, id, "also compare FRA"); err != nil {
		t.Fatal(err)
	}
	if call := awaitConclude(t, done); call.res.Outcome != domain.IntentOutcomeDeniedByUser {
		t.Fatalf("denied conclusion = %+v", call.res)
	}
	if got := f.notices(t, "alpha"); len(got) != 0 {
		t.Fatalf("notice after denial: %+v", got)
	}

	concluded, err := f.conclude(t, run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "compared, FRA too"})
	if err != nil || concluded.Status != domain.ActionRunCompleted {
		t.Fatalf("conclusion = %+v, %v", concluded, err)
	}
	notice := f.onlyNotice(t, "alpha", run.ID)
	wantBody := "Action demo execution " + run.ID + " has ended. Outcome: completed. Use getActionResult with this execution id to read its summary; its artifact package (1 file) is copied to your machine on that first read, and output_dir is set once artifacts.state is ready."
	if notice.Recipient != "architect:alpha" || notice.ArchitectKey != "alpha" || notice.Body != wantBody {
		t.Fatalf("notice = %+v\nwant body %q", notice, wantBody)
	}
	if notice.Payload["action"] != "demo" || notice.Payload["outcome"] != "completed" || notice.Payload["package"] != true || notice.Payload["error"] != "" {
		t.Fatalf("payload = %+v", notice.Payload)
	}
	// What the notice points at is already served: the final status and,
	// on the same machine, the ready package.
	result := f.result(t, arch.ID, run.ID)
	if result.Status != domain.ActionRunCompleted || result.Summary != "compared, FRA too" || result.Artifacts.State != domain.ActionArtifactsReady || result.OutputDir == "" {
		t.Fatalf("result = %+v", result)
	}

	// The architect's session is replaced: the notice waits for the next one
	// rather than failing or going anywhere else.
	f.endArchitect(t, arch)
	if err := f.service.dispatchAgentMessages(ctx); err != nil {
		t.Fatal(err)
	}
	retained, _ := f.messages.GetAgentMessage(ctx, notice.ID)
	if retained.State != domain.AgentMessagePending || !strings.Contains(retained.Condition, "no running architect session for alpha") {
		t.Fatalf("after replacement = %+v", retained)
	}
}

// A ticket worker requests an artifact-free Action; the agent's failed
// conclusion is announced in the worker's own session and delivered there
// through the provider route.
func TestWorkerIsNotifiedInItsOwnSessionForAnArtifactFreeFailedAction(t *testing.T) {
	f := newActionFixture(t)
	f.writeArtifactFreeAction(t, "lookup")
	f.architect(t, "alpha", "lookup")
	w := f.worker(t, "alpha", "ticket-1")
	adapter := f.deliverable(t)
	ctx := context.Background()
	run := f.requestAndStart(t, w.ID, "lookup")
	if run.OutputDir != "" || run.ArtifactContract != "" {
		t.Fatalf("artifact-free run = %+v", run)
	}
	concluded, err := f.conclude(t, run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "site down"})
	if err != nil || concluded.Status != domain.ActionRunFailed {
		t.Fatalf("conclusion = %+v, %v", concluded, err)
	}
	notice := f.onlyNotice(t, "alpha", run.ID)
	wantBody := "Action lookup execution " + run.ID + " has ended. Outcome: failed. Use getActionResult with this execution id to read its summary. No artifact package was published."
	if notice.Recipient != "session:"+w.ID || notice.Body != wantBody || notice.Payload["package"] != false || notice.Payload["requester_session_id"] != w.ID {
		t.Fatalf("notice = %+v\nwant body %q", notice, wantBody)
	}
	if result := f.result(t, w.ID, run.ID); result.Status != domain.ActionRunFailed || result.Artifacts.State != domain.ActionArtifactsNone {
		t.Fatalf("result = %+v", result)
	}

	// The worker's session receives it: handed to the provider with the text
	// the agent reads.
	if err := f.sessions.UpdateRunNativeID(ctx, w.CurrentRun.ID, "native-w"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.dispatchAgentMessages(ctx); err != nil {
		t.Fatal(err)
	}
	handed, _ := f.messages.GetAgentMessage(ctx, notice.ID)
	if handed.State != domain.AgentMessageHanded || handed.TargetSessionID != w.ID {
		t.Fatalf("after dispatch = %+v, want handed to the worker", handed)
	}
	delivered := adapter.deliveries()
	if len(delivered) != 1 || delivered[0].NativeSessionID != "native-w" || delivered[0].Text != "Hiveryn message "+notice.ID+": "+wantBody {
		t.Fatalf("delivered = %+v", delivered)
	}
}

// A worker's notice is never redirected: once its session is gone the notice
// fails, and the result stays readable by the project.
func TestWorkerNoticeFailsOnceItsSessionIsGone(t *testing.T) {
	f := newActionFixture(t)
	f.writeArtifactFreeAction(t, "lookup")
	arch := f.architect(t, "alpha", "lookup")
	w := f.worker(t, "alpha", "ticket-1")
	ctx := context.Background()
	run := f.requestAndStart(t, w.ID, "lookup")
	if _, err := f.conclude(t, run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "found"}); err != nil {
		t.Fatal(err)
	}
	notice := f.onlyNotice(t, "alpha", run.ID)
	// The fixture's default adapter has no message route: a retained
	// condition while the session runs, visible as such.
	if err := f.service.dispatchAgentMessages(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, notice.ID); m.State != domain.AgentMessagePending || !strings.Contains(m.Condition, "cannot receive messages") {
		t.Fatalf("addressed to the worker = %+v", m)
	}
	f.endArchitect(t, w)
	if err := f.service.dispatchAgentMessages(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, notice.ID); m.State != domain.AgentMessageFailed || !strings.Contains(m.LastError, "recipient session "+w.ID+" no longer exists") {
		t.Fatalf("after the worker ended = %+v", m)
	}
	if result := f.result(t, arch.ID, run.ID); result.Status != domain.ActionRunCompleted || result.Summary != "found" {
		t.Fatalf("result for the project = %+v", result)
	}
}

func TestManualActionLaunchNotifiesNobody(t *testing.T) {
	f := newActionFixture(t)
	f.writeArtifactFreeAction(t, "lookup")
	f.writeValidAction(t, "demo")
	ctx := context.Background()
	result := f.launch(t, "lookup", "go")
	if _, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "found"}); err != nil {
		t.Fatal(err)
	}
	cancelled := f.launch(t, "demo", "go")
	if _, err := f.service.CancelActionRun(ctx, cancelled.Run.ID); err != nil {
		t.Fatal(err)
	}
	all, err := f.messages.ListAgentMessages(ctx, "", 0)
	if err != nil || len(all) != 0 {
		t.Fatalf("messages after manual executions = %+v, %v", all, err)
	}
}

func TestCancelledActionNotifiesItsRequester(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	run := f.requestAndStart(t, arch.ID, "demo")

	cancelled, err := f.service.CancelActionRun(ctx, run.ID)
	if err != nil || cancelled.Status != domain.ActionRunFailed {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	notice := f.onlyNotice(t, "alpha", run.ID)
	wantBody := "Action demo execution " + run.ID + " has ended. Outcome: failed (cancelled by user). Use getActionResult with this execution id to read its record. No artifact package was published."
	if notice.Recipient != "architect:alpha" || notice.Body != wantBody || notice.Payload["outcome"] != "failed" || notice.Payload["error"] != "cancelled by user" {
		t.Fatalf("notice = %+v\nwant body %q", notice, wantBody)
	}
	if result := f.result(t, arch.ID, run.ID); result.Status != domain.ActionRunFailed || result.Error != "cancelled by user" || result.Artifacts.State != domain.ActionArtifactsNone {
		t.Fatalf("result = %+v", result)
	}
}

// The approved request starts the execution and the launch then fails: the
// call reports the error, and the requester is still told that the
// execution ended, once.
func TestLaunchFailureAfterStartNotifiesItsRequester(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	// The execution starts (single-run rule claimed) and the agent's PTY
	// cannot be launched.
	pending := f.request(t, arch.ID, "demo", "go")
	f.terminal.startErr = errors.New("pty launch failed")
	if err := f.approve(arch.ID, pending.ExecutionID); err == nil || !strings.Contains(err.Error(), "pty launch failed") {
		t.Fatalf("approve err = %v, want the launch failure", err)
	}
	res, err := pending.wait(t)
	if err != nil || res.Outcome != domain.IntentOutcomeError || res.Result.Status != domain.ActionRunFailed || res.Result.StartedAt == nil || !strings.Contains(res.Result.Error, "launch failed (launch agent)") || res.Notification != nil {
		t.Fatalf("response = %+v, %v; want error with the launch failure and no notification promise", res, err)
	}
	notice := f.onlyNotice(t, "alpha", pending.ExecutionID)
	if !strings.Contains(notice.Body, "Outcome: failed (launch failed (launch agent): ") || notice.Payload["error"] != res.Result.Error {
		t.Fatalf("notice = %+v", notice)
	}
	// A request that never started — its variant is gone by the time it is
	// approved — is reported by the call alone.
	f.terminal.startErr = nil
	never := f.request(t, arch.ID, "demo", "again")
	delete(f.service.cfg.Variants, "codex")
	if err := f.approve(arch.ID, never.ExecutionID); err == nil {
		t.Fatal("approve of a request that cannot start succeeded")
	}
	res, err = never.wait(t)
	if err != nil || res.Outcome != domain.IntentOutcomeError || res.Result.StartedAt != nil || !strings.Contains(res.Result.Error, "could not start") {
		t.Fatalf("never-started response = %+v, %v", res, err)
	}
	f.onlyNotice(t, "alpha", pending.ExecutionID)
}

// The agent's process exits and cannot be resumed: the execution fails and
// the requester is told.
func TestResumeFailureNotifiesItsRequester(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	run := f.requestAndStart(t, arch.ID, "demo")
	session, err := f.sessions.GetSession(ctx, run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.failActionSession(session, "agent exited and could not be resumed: boom")
	notice := f.onlyNotice(t, "alpha", run.ID)
	if !strings.Contains(notice.Body, "Outcome: failed (agent exited and could not be resumed: boom)") {
		t.Fatalf("notice = %+v", notice)
	}
	if result := f.result(t, arch.ID, run.ID); result.Status != domain.ActionRunFailed || !strings.Contains(result.Error, "could not be resumed") {
		t.Fatalf("result = %+v", result)
	}
}

// A conclusion begun its notice and then lost to the user's cancel: the one
// row says what happened, and the late conclusion changes nothing.
func TestConclusionLosingToCancelLeavesOneAccurateNotice(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	run := f.requestAndStart(t, arch.ID, "demo")

	proposed := run
	proposed.Status, proposed.Summary = domain.ActionRunCompleted, "compared"
	begun, err := f.service.beginActionEndedMessage(ctx, proposed, nil)
	if err != nil || begun.State != domain.AgentMessagePendingCommit || !strings.Contains(begun.Body, "Outcome: completed") {
		t.Fatalf("begun = %+v, %v", begun, err)
	}
	if _, err := f.service.CancelActionRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	notice := f.onlyNotice(t, "alpha", run.ID)
	if notice.ID != begun.ID || !strings.Contains(notice.Body, "Outcome: failed (cancelled by user)") || notice.Payload["outcome"] != "failed" {
		t.Fatalf("notice after the cancel won = %+v", notice)
	}
	// The conclusion's write loses, and settling again is a no-op.
	if err := f.runs.PublishActionConclusion(ctx, run.ID, domain.ActionRunCompleted, "compared", nil, time.Now().UTC()); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("late publish err = %v, want conflict", err)
	}
	f.service.settleActionEndedMessage(ctx, begun, run.ID)
	f.service.finishActionRunFailed(run.ID, "a late daemon-side failure")
	again := f.onlyNotice(t, "alpha", run.ID)
	if again.Body != notice.Body || again.UpdatedAt != notice.UpdatedAt {
		t.Fatalf("notice changed after the race was settled: %+v", again)
	}
}

func TestRepeatedBeginsConvergeOnOneNoticePerExecution(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	run := domain.ActionRun{ID: "exec-1", Action: "demo", Trigger: domain.ActionRunTriggerArchitect, ArchitectKey: "alpha", RequesterSessionID: "arch-1", Status: domain.ActionRunCompleted}
	first, err := f.service.beginActionEndedMessage(ctx, run, nil)
	if err != nil || first.State != domain.AgentMessagePendingCommit || !strings.HasSuffix(first.Body, "No artifact package was published.") {
		t.Fatalf("first = %+v %v", first, err)
	}
	// Another begin for the same execution — another outcome, a package —
	// is the same row, untouched: the settle renders the final text.
	pkg := &domain.ActionArtifactRecord{ExecutionID: "exec-1", Kind: domain.ActionArtifactPackage, Files: 3}
	again, err := f.service.beginActionEndedMessage(ctx, failedRun(run, "cancelled by user"), pkg)
	if err != nil || again.ID != first.ID || again.Body != first.Body {
		t.Fatalf("again = %+v %v", again, err)
	}
	// A manual launch begins nothing; a worker request targets its session.
	if m, err := f.service.beginActionEndedMessage(ctx, domain.ActionRun{ID: "manual", Trigger: domain.ActionRunTriggerManual}, nil); err != nil || m.ID != "" {
		t.Fatalf("manual = %+v %v", m, err)
	}
	worker := domain.ActionRun{ID: "exec-2", Action: "demo", Trigger: domain.ActionRunTriggerWorker, ArchitectKey: "alpha", RequesterSessionID: "w-1", Status: domain.ActionRunFailed, Error: "x"}
	if m, err := f.service.beginActionEndedMessage(ctx, worker, nil); err != nil || m.Recipient != "session:w-1" || m.Payload["error"] != "x" {
		t.Fatalf("worker = %+v %v", m, err)
	}
}

func TestReconcileActionNoticeAgainstTheExecutionRecord(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		started   bool
		finish    func(t *testing.T, f *actionFixture, id string)
		state     domain.AgentMessageState
		body      string
		condition string
		lastError string
	}{
		{name: "still running", started: true, finish: func(*testing.T, *actionFixture, string) {},
			state: domain.AgentMessagePendingCommit, condition: "the execution is still running"},
		{name: "published completed", started: true, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.PublishActionConclusion(context.Background(), id, domain.ActionRunCompleted, "done", nil, now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessagePending, body: "Outcome: completed. Use getActionResult with this execution id to read its summary. No artifact package was published."},
		{name: "published failed by the agent", started: true, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.PublishActionConclusion(context.Background(), id, domain.ActionRunFailed, "broken", nil, now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessagePending, body: "Outcome: failed. Use getActionResult with this execution id to read its summary."},
		{name: "cancelled by the daemon", started: true, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.FinishActionRun(context.Background(), id, domain.ActionRunFailed, "", "cancelled by user", now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessagePending, body: "Outcome: failed (cancelled by user). Use getActionResult with this execution id to read its record."},
		{name: "never ran", started: false, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.EndPendingActionRun(context.Background(), id, domain.ActionRunFailed, "", "could not start", now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessageFailed, lastError: "the request never ran (failed)"},
		{name: "denied", started: false, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.EndPendingActionRun(context.Background(), id, domain.ActionRunDenied, "no", "", now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessageFailed, lastError: "the request was denied; it never ran"},
		// The record is never created for this case (see below).
		{name: "execution gone", started: true, finish: func(*testing.T, *actionFixture, string) {},
			state: domain.AgentMessageFailed, lastError: "no longer exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			ctx := context.Background()
			id := "exec-" + strings.ReplaceAll(tc.name, " ", "-")
			run := domain.ActionRun{ID: id, Action: "demo", Trigger: domain.ActionRunTriggerArchitect, Status: domain.ActionRunPendingApproval, ArchitectKey: "alpha",
				RequesterSessionID: "arch-1", CreatedAt: now}
			if tc.started {
				run.Status, run.StartedAt = domain.ActionRunRunning, &now
			}
			if tc.name != "execution gone" {
				if err := f.runs.CreateActionRun(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			m, err := f.service.beginActionEndedMessage(ctx, failedRun(run, "provisional"), nil)
			if err != nil {
				t.Fatal(err)
			}
			tc.finish(t, f, id)
			// A young row is left alone by the periodic pass, settled at startup.
			if err := f.service.reconcileAgentMessages(ctx, false); err != nil {
				t.Fatal(err)
			}
			if got, _ := f.messages.GetAgentMessage(ctx, m.ID); got.State != domain.AgentMessagePendingCommit {
				t.Fatalf("periodic pass touched a young row: %+v", got)
			}
			if err := f.service.reconcileAgentMessages(ctx, true); err != nil {
				t.Fatal(err)
			}
			got, _ := f.messages.GetAgentMessage(ctx, m.ID)
			if got.State != tc.state || !strings.Contains(got.Condition, tc.condition) || !strings.Contains(got.LastError, tc.lastError) || !strings.Contains(got.Body, tc.body) {
				t.Fatalf("reconciled = state %q condition %q last error %q body %q; want %q containing %q / %q / %q", got.State, got.Condition, got.LastError, got.Body, tc.state, tc.condition, tc.lastError, tc.body)
			}
			if tc.state == domain.AgentMessagePending {
				wantOutcome := "failed"
				if strings.Contains(tc.body, "Outcome: completed") {
					wantOutcome = "completed"
				}
				if got.CommittedAt == nil || strings.Contains(got.Body, "provisional") || got.Payload["outcome"] != wantOutcome {
					t.Fatalf("committed row = %+v, want the final text", got)
				}
			}
		})
	}
}

// A daemon restart: an execution whose session is lost is failed as
// interrupted and its requester told — with or without a notice begun before
// the crash — and a notice whose publish landed is promoted with the final
// text. The results the notices point at are readable by the project's next
// architect session.
func TestRestartNotifiesInterruptedAndLandedExecutions(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "other")
	f.writeValidAction(t, "third")
	arch := f.architect(t, "alpha", "demo", "other", "third")
	ctx := context.Background()
	interrupted := f.requestAndStart(t, arch.ID, "demo")
	landed := f.requestAndStart(t, arch.ID, "other")
	silent := f.requestAndStart(t, arch.ID, "third")

	// Two notices were begun; the daemon died before settling either. The
	// first execution's publish never happened and its session is lost; the
	// second's publish landed. The third had nothing begun and lost its
	// session too.
	proposed := interrupted
	proposed.Status, proposed.Summary = domain.ActionRunCompleted, "almost"
	pendingInterrupted, err := f.service.beginActionEndedMessage(ctx, proposed, nil)
	if err != nil {
		t.Fatal(err)
	}
	proposed = landed
	proposed.Status, proposed.Summary = domain.ActionRunFailed, "partial"
	pendingLanded, err := f.service.beginActionEndedMessage(ctx, proposed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runs.PublishActionConclusion(ctx, landed.ID, domain.ActionRunFailed, "partial", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{interrupted.SessionID, landed.SessionID, silent.SessionID} {
		if err := f.sessions.DeleteSession(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	f.open(t)
	if err := f.service.RestoreRunningSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileActionRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.StartMessageDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.service.stopMessageDelivery()

	got, _ := f.messages.GetAgentMessage(ctx, pendingInterrupted.ID)
	if got.State != domain.AgentMessagePending || !strings.Contains(got.Body, "Outcome: failed (interrupted: its agent session no longer exists)") || strings.Contains(got.Body, "almost") {
		t.Fatalf("interrupted notice = %+v", got)
	}
	got, _ = f.messages.GetAgentMessage(ctx, pendingLanded.ID)
	if got.State != domain.AgentMessagePending || got.CommittedAt == nil || !strings.Contains(got.Body, "Outcome: failed. Use getActionResult with this execution id to read its summary") {
		t.Fatalf("landed notice = %+v", got)
	}
	notices := f.notices(t, "alpha")
	if len(notices) != 3 {
		t.Fatalf("notices = %+v, want one per execution", notices)
	}
	var silentNotice *domain.AgentMessage
	for i := range notices {
		if notices[i].Payload["execution_id"] == silent.ID {
			silentNotice = &notices[i]
		}
	}
	if silentNotice == nil || silentNotice.State != domain.AgentMessagePending || !strings.Contains(silentNotice.Body, "interrupted: its agent session no longer exists") {
		t.Fatalf("silent execution's notice = %+v", silentNotice)
	}
	next := f.architect(t, "alpha", "demo", "other", "third")
	if result := f.result(t, next.ID, landed.ID); result.Status != domain.ActionRunFailed || result.Summary != "partial" {
		t.Fatalf("landed result after restart = %+v", result)
	}
	if result := f.result(t, next.ID, silent.ID); result.Status != domain.ActionRunFailed || !strings.Contains(result.Error, "interrupted") {
		t.Fatalf("silent result after restart = %+v", result)
	}
}

// The response to an approved request says whether the notice can reach the
// requester; a denied request promises nothing.
func TestExecuteActionResponseSaysWhetherTheNoticeCanReachTheRequester(t *testing.T) {
	f := newActionFixture(t)
	f.writeArtifactFreeAction(t, "lookup")
	arch := f.architect(t, "alpha", "lookup")
	local := f.worker(t, "alpha", "ticket-1")
	remote := f.remoteWorker(t, "alpha", "ticket-2", "bk")
	ctx := context.Background()

	// Without a message route for the agent, nothing is promised.
	pending := f.request(t, arch.ID, "lookup", "one")
	if err := f.approve(arch.ID, pending.ExecutionID); err != nil {
		t.Fatal(err)
	}
	res, err := pending.wait(t)
	if err != nil || res.Notification == nil || res.Notification.Available || res.Notification.Reason != "agent codex cannot receive messages" {
		t.Fatalf("response without a route = %+v, %v", res, err)
	}
	if _, err := f.service.CancelActionRun(ctx, pending.ExecutionID); err != nil {
		t.Fatal(err)
	}

	f.deliverable(t)
	for _, tc := range []struct {
		name    string
		session domain.Session
		want    domain.ActionNotification
	}{
		{"architect", arch, domain.ActionNotification{Available: true}},
		{"local worker", local, domain.ActionNotification{Available: true}},
		{"remote worker", remote, domain.ActionNotification{Reason: "this session runs on machine bk, and message delivery to remote sessions is not available"}},
	} {
		pending := f.request(t, tc.session.ID, "lookup", tc.name)
		if err := f.approve(tc.session.ID, pending.ExecutionID); err != nil {
			t.Fatal(err)
		}
		res, err := pending.wait(t)
		if err != nil || res.Outcome != domain.IntentOutcomeApproved || res.Notification == nil || *res.Notification != tc.want {
			t.Fatalf("%s response = %+v, %v; want notification %+v", tc.name, res, err, tc.want)
		}
		// The remote worker's notice, once the execution ends, is retained
		// with the same limitation visible, and its result stays readable.
		if _, err := f.service.CancelActionRun(ctx, pending.ExecutionID); err != nil {
			t.Fatal(err)
		}
		if tc.name == "remote worker" {
			if err := f.service.dispatchAgentMessages(ctx); err != nil {
				t.Fatal(err)
			}
			notice := f.onlyNoticeFor(t, "alpha", pending.ExecutionID)
			if !strings.Contains(notice.Condition, "message delivery to remote sessions is not available") {
				t.Fatalf("remote worker's notice = %+v", notice)
			}
			if result := f.result(t, remote.ID, pending.ExecutionID); result.Status != domain.ActionRunFailed {
				t.Fatalf("remote worker's result = %+v", result)
			}
		}
	}

	// A denied request is not an execution: no promise, no notice.
	denied := f.request(t, arch.ID, "lookup", "denied")
	if err := f.service.DenyIntent(ctx, arch.ID, denied.ExecutionID, "not now"); err != nil {
		t.Fatal(err)
	}
	res, err = denied.wait(t)
	if err != nil || res.Outcome != domain.IntentOutcomeDeniedByUser || res.Notification != nil {
		t.Fatalf("denied response = %+v, %v", res, err)
	}
	for _, n := range f.notices(t, "alpha") {
		if n.Payload["execution_id"] == denied.ExecutionID {
			t.Fatalf("denied request produced a notice: %+v", n)
		}
	}
}

// onlyNoticeFor is the project's single notice about executionID.
func (f *actionFixture) onlyNoticeFor(t *testing.T, architectKey, executionID string) domain.AgentMessage {
	t.Helper()
	var found []domain.AgentMessage
	for _, n := range f.notices(t, architectKey) {
		if n.Payload["execution_id"] == executionID {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Fatalf("notices for %s = %+v, want exactly one", executionID, found)
	}
	return found[0]
}
