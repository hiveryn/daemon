package sessionruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// notices returns the project's action-conclusion messages, newest first.
func (f *actionFixture) notices(t *testing.T, architectKey string) []domain.AgentMessage {
	t.Helper()
	listed, err := f.service.ListAgentMessages(context.Background(), architectKey)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	out := []domain.AgentMessage{}
	for _, m := range listed {
		if m.Kind == domain.AgentMessageActionConcluded {
			out = append(out, m)
		}
	}
	return out
}

// requestAndStart raises an approved request from sessionID and returns the
// running execution.
func (f *actionFixture) requestAndStart(t *testing.T, sessionID, name string) domain.ActionRun {
	t.Helper()
	pending := f.request(t, sessionID, name, "go")
	if err := f.approve(sessionID, pending.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pending.wait(t); err != nil {
		t.Fatal(err)
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
	got := f.notices(t, "alpha")
	if len(got) != 1 {
		t.Fatalf("notices = %+v, want exactly one", got)
	}
	notice := got[0]
	wantBody := "Action demo execution " + run.ID + " has concluded. Outcome: completed. Use getActionResult with this execution id to read its summary; its artifact package (1 file) is copied to your machine on that first read, and output_dir is set once artifacts.state is ready."
	if notice.State != domain.AgentMessagePending || notice.Recipient != "architect:alpha" || notice.ArchitectKey != "alpha" || notice.CommittedAt == nil ||
		notice.DedupeKey != "action_concluded:"+run.ID+":completed" || notice.Body != wantBody {
		t.Fatalf("notice = %+v\nwant body %q", notice, wantBody)
	}
	if notice.Payload["execution_id"] != run.ID || notice.Payload["action"] != "demo" || notice.Payload["outcome"] != "completed" || notice.Payload["package"] != true {
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

func TestWorkerIsNotifiedInItsOwnSessionForAnArtifactFreeFailedAction(t *testing.T) {
	f := newActionFixture(t)
	f.writeArtifactFreeAction(t, "lookup")
	f.architect(t, "alpha", "lookup")
	w := f.worker(t, "alpha", "ticket-1")
	ctx := context.Background()
	run := f.requestAndStart(t, w.ID, "lookup")
	if run.OutputDir != "" || run.ArtifactContract != "" {
		t.Fatalf("artifact-free run = %+v", run)
	}
	concluded, err := f.conclude(t, run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "site down"})
	if err != nil || concluded.Status != domain.ActionRunFailed {
		t.Fatalf("conclusion = %+v, %v", concluded, err)
	}
	got := f.notices(t, "alpha")
	if len(got) != 1 {
		t.Fatalf("notices = %+v", got)
	}
	notice := got[0]
	wantBody := "Action lookup execution " + run.ID + " has concluded. Outcome: failed. Use getActionResult with this execution id to read its summary. No artifact package was published."
	if notice.State != domain.AgentMessagePending || notice.Recipient != "session:"+w.ID || notice.Body != wantBody || notice.Payload["package"] != false || notice.Payload["requester_session_id"] != w.ID {
		t.Fatalf("notice = %+v\nwant body %q", notice, wantBody)
	}
	if result := f.result(t, w.ID, run.ID); result.Status != domain.ActionRunFailed || result.Artifacts.State != domain.ActionArtifactsNone {
		t.Fatalf("result = %+v", result)
	}
	// The worker's session is the target while it runs; the fixture's run
	// (no profile snapshot, an adapter without message delivery) is a
	// retained condition, not a failure. Once the session is gone the notice
	// fails: never redirected.
	if err := f.service.dispatchAgentMessages(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, notice.ID); m.State != domain.AgentMessagePending || m.Condition == "" {
		t.Fatalf("addressed to the worker = %+v", m)
	}
	f.endArchitect(t, w)
	if err := f.service.dispatchAgentMessages(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.messages.GetAgentMessage(ctx, notice.ID); m.State != domain.AgentMessageFailed || !strings.Contains(m.LastError, "recipient session "+w.ID+" no longer exists") {
		t.Fatalf("after the worker ended = %+v", m)
	}
}

func TestManualActionLaunchNotifiesNobody(t *testing.T) {
	f := newActionFixture(t)
	f.writeArtifactFreeAction(t, "lookup")
	result := f.launch(t, "lookup", "go")
	if _, err := f.conclude(t, result.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "found"}); err != nil {
		t.Fatal(err)
	}
	open, err := f.messages.ListOpenAgentMessages(context.Background())
	if err != nil || len(open) != 0 {
		t.Fatalf("messages after a manual conclusion = %+v, %v", open, err)
	}
	if got := f.notices(t, ""); len(got) != 0 {
		t.Fatalf("unscoped notices = %+v", got)
	}
}

func TestRepeatedActionConclusionAttemptsConvergeOnOneNotice(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	run := domain.ActionRun{ID: "exec-1", Action: "demo", Trigger: domain.ActionRunTriggerArchitect, ArchitectKey: "alpha", RequesterSessionID: "arch-1"}
	first, err := f.service.beginActionConcludedMessage(ctx, run, domain.ActionRunCompleted, nil)
	if err != nil || first.State != domain.AgentMessagePendingCommit || !strings.HasSuffix(first.Body, "No artifact package was published.") {
		t.Fatalf("first = %+v %v", first, err)
	}
	// The next attempt has a package: same row, text refreshed.
	pkg := &domain.ActionArtifactRecord{ExecutionID: "exec-1", Kind: domain.ActionArtifactPackage, Files: 3}
	again, err := f.service.beginActionConcludedMessage(ctx, run, domain.ActionRunCompleted, pkg)
	if err != nil || again.ID != first.ID || again.State != domain.AgentMessagePendingCommit || !strings.Contains(again.Body, "(3 files)") || again.Payload["package"] != true {
		t.Fatalf("again = %+v %v", again, err)
	}
	if stored, _ := f.messages.GetAgentMessage(ctx, first.ID); stored.Body != again.Body {
		t.Fatalf("stored body %q, want refreshed", stored.Body)
	}
	// The other outcome is another announcement.
	other, err := f.service.beginActionConcludedMessage(ctx, run, domain.ActionRunFailed, nil)
	if err != nil || other.ID == first.ID || other.DedupeKey != "action_concluded:exec-1:failed" {
		t.Fatalf("other = %+v %v", other, err)
	}
	// A manual launch begins nothing; a worker request targets its session.
	if m, err := f.service.beginActionConcludedMessage(ctx, domain.ActionRun{ID: "manual", Trigger: domain.ActionRunTriggerManual}, domain.ActionRunCompleted, nil); err != nil || m.ID != "" {
		t.Fatalf("manual = %+v %v", m, err)
	}
	worker := domain.ActionRun{ID: "exec-2", Action: "demo", Trigger: domain.ActionRunTriggerWorker, ArchitectKey: "alpha", RequesterSessionID: "w-1"}
	if m, err := f.service.beginActionConcludedMessage(ctx, worker, domain.ActionRunCompleted, nil); err != nil || m.Recipient != "session:w-1" {
		t.Fatalf("worker = %+v %v", m, err)
	}
}

func TestReconcileActionNoticeAgainstTheExecutionRecord(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		outcome   domain.ActionRunStatus
		finish    func(t *testing.T, f *actionFixture, id string)
		state     domain.AgentMessageState
		condition string
		lastError string
	}{
		{name: "still running", outcome: domain.ActionRunCompleted, finish: func(*testing.T, *actionFixture, string) {},
			state: domain.AgentMessagePendingCommit, condition: "conclusion not persisted; the execution is running"},
		{name: "published with this outcome", outcome: domain.ActionRunCompleted, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.PublishActionConclusion(context.Background(), id, domain.ActionRunCompleted, "done", nil, now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessagePending},
		{name: "published failed with this outcome", outcome: domain.ActionRunFailed, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.PublishActionConclusion(context.Background(), id, domain.ActionRunFailed, "broken", nil, now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessagePending},
		{name: "published with the other outcome", outcome: domain.ActionRunCompleted, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.PublishActionConclusion(context.Background(), id, domain.ActionRunFailed, "broken", nil, now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessageFailed, lastError: "superseded: the execution ended as failed without this conclusion"},
		{name: "cancelled by the daemon", outcome: domain.ActionRunFailed, finish: func(t *testing.T, f *actionFixture, id string) {
			if err := f.runs.FinishActionRun(context.Background(), id, domain.ActionRunFailed, "", "cancelled by user", now); err != nil {
				t.Fatal(err)
			}
		}, state: domain.AgentMessageFailed, lastError: "superseded: the execution ended as failed without this conclusion (cancelled by user)"},
		// The record is never created for this case (see below).
		{name: "execution gone", outcome: domain.ActionRunCompleted, finish: func(*testing.T, *actionFixture, string) {},
			state: domain.AgentMessageFailed, lastError: "no longer exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			ctx := context.Background()
			id := "exec-" + strings.ReplaceAll(tc.name, " ", "-")
			run := domain.ActionRun{ID: id, Action: "demo", Trigger: domain.ActionRunTriggerArchitect, Status: domain.ActionRunRunning, ArchitectKey: "alpha",
				RequesterSessionID: "arch-1", CreatedAt: now, StartedAt: &now}
			if tc.name != "execution gone" {
				if err := f.runs.CreateActionRun(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			m, err := f.service.beginActionConcludedMessage(ctx, run, tc.outcome, nil)
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
			if got.State != tc.state || !strings.Contains(got.Condition, tc.condition) || !strings.Contains(got.LastError, tc.lastError) {
				t.Fatalf("reconciled = state %q condition %q last error %q; want %q containing %q / %q", got.State, got.Condition, got.LastError, tc.state, tc.condition, tc.lastError)
			}
			if tc.state == domain.AgentMessagePending && got.CommittedAt == nil {
				t.Fatal("committed_at not set")
			}
		})
	}
}

// A daemon restart between the notice and the publish: the execution is
// reconciled first (interrupted → failed), then the outbox settles the notice
// as superseded; a notice whose publish did land is promoted.
func TestRestartSettlesActionNoticesAfterTheExecutions(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "other")
	arch := f.architect(t, "alpha", "demo", "other")
	ctx := context.Background()
	interrupted := f.requestAndStart(t, arch.ID, "demo")
	landed := f.requestAndStart(t, arch.ID, "other")

	// Both notices were begun; the daemon died before committing either. The
	// first execution's publish never happened and its session is lost; the
	// second's publish landed.
	pendingInterrupted, err := f.service.beginActionConcludedMessage(ctx, interrupted, domain.ActionRunCompleted, nil)
	if err != nil {
		t.Fatal(err)
	}
	pendingLanded, err := f.service.beginActionConcludedMessage(ctx, landed, domain.ActionRunFailed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runs.PublishActionConclusion(ctx, landed.ID, domain.ActionRunFailed, "partial", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.sessions.DeleteSession(ctx, interrupted.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.sessions.DeleteSession(ctx, landed.SessionID); err != nil {
		t.Fatal(err)
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
	if got.State != domain.AgentMessageFailed || !strings.Contains(got.LastError, "superseded: the execution ended as failed without this conclusion (interrupted") {
		t.Fatalf("interrupted notice = %+v", got)
	}
	got, _ = f.messages.GetAgentMessage(ctx, pendingLanded.ID)
	if got.State != domain.AgentMessagePending || got.CommittedAt == nil {
		t.Fatalf("landed notice = %+v, want committed and open", got)
	}
	// Only one notice per execution, and the result the notice points at is
	// readable by the project's next architect session.
	if notices := f.notices(t, "alpha"); len(notices) != 2 {
		t.Fatalf("notices = %+v", notices)
	}
	next := f.architect(t, "alpha", "demo", "other")
	if result := f.result(t, next.ID, landed.ID); result.Status != domain.ActionRunFailed || result.Summary != "partial" {
		t.Fatalf("result after restart = %+v", result)
	}
}
