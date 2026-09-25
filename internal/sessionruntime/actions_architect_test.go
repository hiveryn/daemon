package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// architect registers an architect whose hiveryn.yaml lists available and
// starts a running session for it.
func (f *actionFixture) architect(t *testing.T, key string, available ...string) domain.Session {
	t.Helper()
	if f.service.cfg.Architects == nil {
		f.service.cfg.Architects = map[string]config.ArchitectConfig{}
	}
	f.service.cfg.Architects[key] = config.ArchitectConfig{Name: key, Path: t.TempDir(), Repos: map[string]string{}, AvailableActions: available}
	ctx := context.Background()
	session, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{
		ArchitectKey: key,
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "architect-" + key,
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	if err != nil {
		t.Fatalf("create architect session: %v", err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "codex", Workdir: session.Workdir}); err != nil {
		t.Fatalf("create architect run: %v", err)
	}
	session, err = f.sessions.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// endArchitect ends an architect session the way every session end does
// (appendAndPublishSessionEnded fails its open intents), then removes it.
func (f *actionFixture) endArchitect(t *testing.T, session domain.Session) {
	t.Helper()
	ctx := context.Background()
	if err := f.service.appendAndPublishSessionEnded(ctx, session.ID, session.CurrentRun.ID, "done", "concluded", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.sessions.MarkRunCompleted(ctx, session.CurrentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sessions.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *actionFixture) request(t *testing.T, sessionID, name, prompt string) domain.ActionResult {
	t.Helper()
	result, err := f.service.RequestExecuteAction(context.Background(), sessionID, domain.ExecuteActionRequest{Name: name, Prompt: prompt})
	if err != nil {
		t.Fatalf("executeAction %s: %v", name, err)
	}
	return result
}

func (f *actionFixture) approve(sessionID, executionID, variant string) error {
	_, err := f.service.ApproveIntent(context.Background(), sessionID, executionID, domain.IntentInputValues{actionVariantInput: variant})
	return err
}

func (f *actionFixture) result(t *testing.T, sessionID, executionID string) domain.ActionResult {
	t.Helper()
	result, err := f.service.GetActionResult(context.Background(), sessionID, executionID)
	if err != nil {
		t.Fatalf("getActionResult %s: %v", executionID, err)
	}
	return result
}

func TestAvailableActionsListsConfiguredActionsAndReportsMissing(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "hidden")
	f.writeAction(t, "broken", "name: broken\n", testActionKickoff)
	arch := f.architect(t, "alpha", "missing", "demo", "broken")
	ctx := context.Background()

	list, err := f.service.AvailableActions(ctx, arch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Actions) != 3 {
		t.Fatalf("available = %+v, want exactly the three configured names", list.Actions)
	}
	missing, demo, broken := list.Actions[0], list.Actions[1], list.Actions[2]
	if missing.Name != "missing" || missing.Valid || len(missing.Problems) != 1 || !strings.Contains(missing.Problems[0].Message, "not found") {
		t.Fatalf("missing = %+v, want invalid with a not-found problem", missing)
	}
	if demo.Name != "demo" || !demo.Valid || demo.Description == "" || demo.Artifacts == "" {
		t.Fatalf("demo = %+v, want valid with description and artifacts", demo)
	}
	if broken.Valid || len(broken.Problems) == 0 {
		t.Fatalf("broken = %+v, want invalid with problems", broken)
	}

	none := f.architect(t, "beta")
	list, err = f.service.AvailableActions(ctx, none.ID)
	if err != nil || len(list.Actions) != 0 {
		t.Fatalf("omitted availableActions = %+v, %v; want none", list, err)
	}

	// Action agents can neither discover nor request Actions.
	launched := f.launch(t, "hidden", "go")
	if _, err := f.service.AvailableActions(ctx, launched.Session.ID); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("action session discovery err = %v, want validation error", err)
	}
	if _, err := f.service.RequestExecuteAction(ctx, launched.Session.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x"}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("action session executeAction err = %v, want validation error", err)
	}
}

func TestSuggestionsReachManualViewsButNotArchitects(t *testing.T) {
	f := newActionFixture(t)
	f.writeAction(t, "demo", "name: demo\ndescription: d\nartifacts: a\nsuggestions:\n  - Compare AMS and LDN\n", testActionKickoff)
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()

	def, err := f.service.GetAction(ctx, "demo")
	if err != nil || !def.Valid || len(def.Suggestions) != 1 || def.Suggestions[0] != "Compare AMS and LDN" {
		t.Fatalf("GetAction = %+v, %v; want the suggestion", def, err)
	}
	library, err := f.service.ListActions(ctx)
	if err != nil || len(library.Actions) != 1 || len(library.Actions[0].Suggestions) != 1 {
		t.Fatalf("ListActions = %+v, %v; want the suggestion", library, err)
	}
	available, err := f.service.AvailableActions(ctx, arch.ID)
	if err != nil || len(available.Actions) != 1 || !available.Actions[0].Valid || available.Actions[0].Suggestions != nil {
		t.Fatalf("AvailableActions = %+v, %v; want the valid definition without suggestions", available, err)
	}
}

func TestExecuteActionEnforcesAvailabilityAndValidity(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "other")
	f.writeAction(t, "broken", "name: broken\n", testActionKickoff)
	arch := f.architect(t, "alpha", "demo", "broken", "gone")
	ctx := context.Background()

	for _, tc := range []struct{ name, prompt, want string }{
		{"other", "x", "not available"},
		{"gone", "x", "not found"},
		{"broken", "x", "invalid definition"},
		{"demo", "  ", "prompt"},
		{"", "x", "name"},
	} {
		_, err := f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: tc.name, Prompt: tc.prompt})
		if !errors.As(err, new(*domain.ValidationError)) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("executeAction(%q, %q) err = %v, want validation error mentioning %q", tc.name, tc.prompt, err, tc.want)
		}
	}
	runs, _ := f.service.ListActionRuns(ctx, "", 0)
	if len(runs) != 0 {
		t.Fatalf("rejected requests left records: %+v", runs)
	}

	// Busy: a running execution (here a manual one) is an explicit error.
	manual := f.launch(t, "demo", "manual")
	_, err := f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x"})
	if !errors.As(err, new(*domain.ConflictError)) || !strings.Contains(err.Error(), manual.Run.ID) {
		t.Fatalf("busy executeAction err = %v, want conflict naming %s", err, manual.Run.ID)
	}
}

func TestExecuteActionReturnsPendingThenApprovalLaunchesUnderSameID(t *testing.T) {
	f := newActionFixture(t)
	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "claude"}
	repoPath := f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	events := f.service.SubscribeActionEvents()
	defer events.Close()

	pending := f.request(t, arch.ID, "demo", "compare AMS and LDN")
	if pending.Status != domain.ActionRunPendingApproval || pending.ExecutionID == "" || pending.StartedAt != nil || pending.ElapsedSeconds != nil || pending.OutputDir != "" || pending.Activity.Available {
		t.Fatalf("pending = %+v, want pending_approval with nothing started", pending)
	}
	if f.adapter.launchRequest.Workdir != "" {
		t.Fatal("an agent launched before approval")
	}
	if ev := <-events.C(); ev.ExecutionID != pending.ExecutionID || ev.Status != domain.ActionRunPendingApproval {
		t.Fatalf("event = %+v, want pending_approval", ev)
	}

	// The request is shown with a required variant choice and no default.
	in, ok := f.service.intents.Get(pending.ExecutionID)
	if !ok || in.Type != domain.IntentTypeExecuteAction || in.Policy != domain.IntentPolicyManual || in.WaitSeconds != 0 {
		t.Fatalf("intent = %+v, %v; want deferred executeAction under the execution id", in, ok)
	}
	if len(in.Inputs) != 1 || in.Inputs[0].Name != actionVariantInput || !in.Inputs[0].Required || in.Inputs[0].Default != nil || len(in.Inputs[0].Options) != 2 {
		t.Fatalf("inputs = %+v, want one required variant choice over both variants", in.Inputs)
	}
	if in.Payload["action"] != "demo" || in.Payload["prompt"] != "compare AMS and LDN" {
		t.Fatalf("payload = %+v", in.Payload)
	}

	// An invalid choice is correctable: the request stays pending.
	if err := f.approve(arch.ID, pending.ExecutionID, "nope"); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("invalid variant err = %v, want validation error", err)
	}
	if err := f.approve(arch.ID, pending.ExecutionID, ""); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("missing variant err = %v, want validation error", err)
	}
	if got := f.result(t, arch.ID, pending.ExecutionID); got.Status != domain.ActionRunPendingApproval {
		t.Fatalf("after invalid approvals = %+v, want still pending", got)
	}

	if err := f.approve(arch.ID, pending.ExecutionID, "codex"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	running := f.result(t, arch.ID, pending.ExecutionID)
	if running.Status != domain.ActionRunRunning || running.ProfileName != "codex" || running.StartedAt == nil || running.ElapsedSeconds == nil || running.OutputDir == "" {
		t.Fatalf("after approve = %+v, want running with start and output", running)
	}
	run, _ := f.service.GetActionRun(context.Background(), pending.ExecutionID)
	if run.Trigger != domain.ActionRunTriggerArchitect || run.ArchitectKey != "alpha" || run.RequesterSessionID != arch.ID || run.SessionID == "" {
		t.Fatalf("run = %+v", run)
	}
	if f.adapter.launchRequest.Workdir != repoPath || !strings.Contains(f.adapter.launchRequest.Prompt, "compare AMS and LDN") {
		t.Fatalf("agent launch = %+v", f.adapter.launchRequest)
	}
	// The generic approval completed with the launch; the Action did not.
	record, err := f.service.GetDeferredIntent(context.Background(), arch.ID, pending.ExecutionID)
	if err != nil || record.Status != domain.DeferredIntentCompleted {
		t.Fatalf("deferred record = %+v, %v", record, err)
	}
	if running.Status != domain.ActionRunRunning {
		t.Fatal("approval callback completed the Action")
	}

	// Activity is reported only once the agent has reported it.
	if running.Activity.Available {
		t.Fatalf("activity = %+v before any agent status", running.Activity)
	}
	session, _ := f.sessions.GetSession(context.Background(), run.SessionID)
	if err := f.sessions.UpdateRunAgentStatus(context.Background(), session.CurrentRun.ID, domain.AgentStatusActive); err != nil {
		t.Fatal(err)
	}
	if got := f.result(t, arch.ID, pending.ExecutionID); !got.Activity.Available || got.Activity.Status != domain.AgentStatusActive {
		t.Fatalf("activity = %+v, want active", got.Activity)
	}

	// Execution is independent of the requesting architect session.
	f.endArchitect(t, arch)
	if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conclude(t, run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "delivered"}); err != nil {
		t.Fatal(err)
	}

	// A later session of the same architect still reads it; another cannot.
	later := f.architect(t, "alpha", "demo")
	done := f.result(t, later.ID, pending.ExecutionID)
	if done.Status != domain.ActionRunCompleted || done.Summary != "delivered" || done.OutputDir != run.OutputDir || done.EndedAt == nil || done.Activity.Available {
		t.Fatalf("completed = %+v", done)
	}
	stranger := f.architect(t, "beta", "demo")
	if _, err := f.service.GetActionResult(context.Background(), stranger.ID, pending.ExecutionID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("other architect read err = %v, want not found", err)
	}
	manual := f.launch(t, "demo", "manual")
	if _, err := f.service.GetActionResult(context.Background(), later.ID, manual.Run.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("manual execution read err = %v, want not found", err)
	}
}

func TestExecuteActionDenialKeepsReasonAndRetryReplays(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()

	first := f.request(t, arch.ID, "demo", "go")
	again := f.request(t, arch.ID, "demo", "go")
	if again.ExecutionID != first.ExecutionID {
		t.Fatalf("retry minted %s, want the original %s", again.ExecutionID, first.ExecutionID)
	}
	if err := f.service.DenyIntent(ctx, arch.ID, first.ExecutionID, "not now"); err != nil {
		t.Fatal(err)
	}
	denied := f.result(t, arch.ID, first.ExecutionID)
	if denied.Status != domain.ActionRunDenied || denied.Reason != "not now" || denied.StartedAt != nil || denied.EndedAt == nil {
		t.Fatalf("denied = %+v", denied)
	}
	if replay := f.request(t, arch.ID, "demo", "go"); replay.ExecutionID != first.ExecutionID || replay.Status != domain.ActionRunDenied {
		t.Fatalf("replay = %+v, want the denied original", replay)
	}
	if err := f.approve(arch.ID, first.ExecutionID, "codex"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("approve after deny err = %v", err)
	}
	// Denial is history in the Actions window too.
	runs, _ := f.service.ListActionRuns(ctx, "demo", 0)
	if len(runs) != 1 || runs[0].Status != domain.ActionRunDenied {
		t.Fatalf("history = %+v", runs)
	}
}

func TestExecuteActionReplayWindowIsTenMinutes(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	now := time.Now().UTC()
	f.service.intents.now = func() time.Time { return now }

	first := f.request(t, arch.ID, "demo", "go")
	// A pending request never expires: it is deduplicated, not replayed.
	now = now.Add(time.Hour)
	if again := f.request(t, arch.ID, "demo", "go"); again.ExecutionID != first.ExecutionID {
		t.Fatalf("pending retry after 1h minted %s, want %s", again.ExecutionID, first.ExecutionID)
	}
	if err := f.service.DenyIntent(ctx, arch.ID, first.ExecutionID, "not now"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(10 * time.Minute)
	if replay := f.request(t, arch.ID, "demo", "go"); replay.ExecutionID != first.ExecutionID || replay.Status != domain.ActionRunDenied {
		t.Fatalf("inside window = %+v, want the denied original", replay)
	}

	now = now.Add(time.Second)
	second := f.request(t, arch.ID, "demo", "go")
	if second.ExecutionID == first.ExecutionID || second.Status != domain.ActionRunPendingApproval {
		t.Fatalf("past window = %+v, want a new pending request", second)
	}
	if err := f.approve(arch.ID, second.ExecutionID, "codex"); err != nil {
		t.Fatalf("approve new request: %v", err)
	}
	if got := f.result(t, arch.ID, second.ExecutionID); got.Status != domain.ActionRunRunning {
		t.Fatalf("new request = %+v, want running", got)
	}

	// Once the running execution's replay expires, the single-run rule still holds.
	now = now.Add(11 * time.Minute)
	if _, err := f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "go"}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("request while running err = %v, want conflict", err)
	}
	// Expiry forgets the replay, never the history.
	runs, _ := f.service.ListActionRuns(ctx, "demo", 0)
	if len(runs) != 2 {
		t.Fatalf("history = %+v, want both executions", runs)
	}
	if denied := f.result(t, arch.ID, first.ExecutionID); denied.Status != domain.ActionRunDenied {
		t.Fatalf("original = %+v, want still denied", denied)
	}
}

func TestExecuteActionApprovalRechecksBusyAndAvailability(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()

	// Another launch wins while the request is pending.
	pending := f.request(t, arch.ID, "demo", "go")
	manual := f.launch(t, "demo", "manual wins")
	err := f.approve(arch.ID, pending.ExecutionID, "codex")
	if !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("approve while busy err = %v, want conflict", err)
	}
	failed := f.result(t, arch.ID, pending.ExecutionID)
	if failed.Status != domain.ActionRunFailed || !strings.Contains(failed.Error, "could not start") || !strings.Contains(failed.Error, manual.Run.ID) || failed.StartedAt != nil {
		t.Fatalf("busy approval = %+v, want failed without starting", failed)
	}
	if still, _ := f.service.GetActionRun(ctx, manual.Run.ID); still.Status != domain.ActionRunRunning {
		t.Fatalf("winning execution = %+v", still)
	}
	if _, err := f.service.CancelActionRun(ctx, manual.Run.ID); err != nil {
		t.Fatal(err)
	}

	// Removed from availableActions while pending.
	second := f.request(t, arch.ID, "demo", "second")
	f.service.cfg.Architects["alpha"] = config.ArchitectConfig{Name: "alpha", Path: t.TempDir(), Repos: map[string]string{}}
	if err := f.approve(arch.ID, second.ExecutionID, "codex"); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("approve after removal err = %v", err)
	}
	if got := f.result(t, arch.ID, second.ExecutionID); got.Status != domain.ActionRunFailed || !strings.Contains(got.Error, "not available") {
		t.Fatalf("removed approval = %+v", got)
	}
}

// Concurrent approvals of one request launch it exactly once, and requests
// for the same action raced through approval run one at a time.
func TestExecuteActionConcurrentApprovalLaunchesOnce(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	a := f.request(t, arch.ID, "demo", "a")
	b := f.request(t, arch.ID, "demo", "b")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for _, id := range []string{a.ExecutionID, a.ExecutionID, a.ExecutionID, b.ExecutionID, b.ExecutionID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- f.approve(arch.ID, id, "codex")
		}(id)
	}
	wg.Wait()
	close(errs)

	ra, rb := f.result(t, arch.ID, a.ExecutionID), f.result(t, arch.ID, b.ExecutionID)
	statuses := []domain.ActionRunStatus{ra.Status, rb.Status}
	running, failed := 0, 0
	for _, st := range statuses {
		switch st {
		case domain.ActionRunRunning:
			running++
		case domain.ActionRunFailed:
			failed++
		}
	}
	if running != 1 || failed != 1 {
		t.Fatalf("statuses = %v, want exactly one running and one failed as busy", statuses)
	}
	sessions, _ := f.sessions.ListSessions(context.Background())
	actionSessions := 0
	for _, s := range sessions {
		if s.SessionType == domain.SessionTypeAction {
			actionSessions++
		}
	}
	if actionSessions != 1 {
		t.Fatalf("%d action sessions, want exactly one launch", actionSessions)
	}
}

func TestExecuteActionTeardownAndRestartFailPendingRequests(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "live")
	ctx := context.Background()

	arch := f.architect(t, "alpha", "demo", "live")
	abandoned := f.request(t, arch.ID, "demo", "abandoned")
	f.endArchitect(t, arch)
	later := f.architect(t, "alpha", "demo", "live")
	if got := f.result(t, later.ID, abandoned.ExecutionID); got.Status != domain.ActionRunFailed || got.StartedAt != nil || !strings.Contains(got.Error, "session ended") {
		t.Fatalf("abandoned = %+v, want failed as never run", got)
	}

	pending := f.request(t, later.ID, "demo", "pending at restart")
	launched := f.request(t, later.ID, "live", "running at restart")
	if err := f.approve(later.ID, launched.ExecutionID, "codex"); err != nil {
		t.Fatal(err)
	}
	liveRun, _ := f.service.GetActionRun(ctx, launched.ExecutionID)
	liveSession, _ := f.sessions.GetSession(ctx, liveRun.SessionID)
	if err := f.sessions.UpdateRunAgentStatus(ctx, liveSession.CurrentRun.ID, domain.AgentStatusActive); err != nil {
		t.Fatal(err)
	}

	f.open(t)
	if err := f.service.RestoreRunningSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileActionRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReconcileIntents(ctx); err != nil {
		t.Fatal(err)
	}
	f.service.cfg.Architects = map[string]config.ArchitectConfig{"alpha": {Name: "alpha", AvailableActions: []string{"demo", "live"}}}
	fresh := f.architect(t, "alpha", "demo", "live")
	if got := f.result(t, fresh.ID, pending.ExecutionID); got.Status != domain.ActionRunFailed || !strings.Contains(got.Error, "restarted") {
		t.Fatalf("pending across restart = %+v", got)
	}
	// The restored running execution keeps running: neither the generic
	// intent reconciliation nor the request bookkeeping overwrites it.
	if got := f.result(t, fresh.ID, launched.ExecutionID); got.Status != domain.ActionRunRunning {
		t.Fatalf("restored execution = %+v, want running", got)
	} else if got.Activity.Available {
		// The killed agent's last "active" says nothing about its resumed
		// replacement, which has reported nothing yet.
		t.Fatalf("restored activity = %+v, want unavailable until the resumed agent reports", got.Activity)
	}
	if err := os.WriteFile(filepath.Join(liveRun.OutputDir, "summary.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conclude(t, liveRun.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForActionResult(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	pending := f.request(t, arch.ID, "demo", "go")

	for _, bad := range []time.Duration{0, -time.Second, 31 * time.Second} {
		if _, err := f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, bad); !errors.As(err, new(*domain.ValidationError)) {
			t.Errorf("timeout %v err = %v, want validation error", bad, err)
		}
	}

	// No change: returns the unchanged state at the timeout, not at once.
	start := time.Now()
	res, err := f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, 300*time.Millisecond)
	if err != nil || !res.TimedOut || res.Changed || res.Result.Status != domain.ActionRunPendingApproval || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("timeout wait = %+v, %v after %v", res, err, time.Since(start))
	}

	// A status change ends the wait early.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = f.approve(arch.ID, pending.ExecutionID, "codex")
	}()
	start = time.Now()
	res, err = f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, 10*time.Second)
	if err != nil || !res.Changed || res.TimedOut || res.Result.Status != domain.ActionRunRunning || time.Since(start) > 5*time.Second {
		t.Fatalf("changed wait = %+v, %v", res, err)
	}

	// Cancelling the wait does not touch the execution.
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := f.service.WaitForActionResult(cctx, arch.ID, pending.ExecutionID, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait err = %v", err)
	}
	if got := f.result(t, arch.ID, pending.ExecutionID); got.Status != domain.ActionRunRunning {
		t.Fatalf("after cancelled wait = %+v, want still running", got)
	}

	// A final execution returns at once.
	run, _ := f.service.GetActionRun(ctx, pending.ExecutionID)
	if _, err := f.service.CancelActionRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	res, err = f.service.WaitForActionResult(ctx, arch.ID, pending.ExecutionID, 10*time.Second)
	if err != nil || res.Changed || res.TimedOut || res.Result.Status != domain.ActionRunFailed || time.Since(start) > time.Second {
		t.Fatalf("final wait = %+v, %v", res, err)
	}

	// Scoped like getActionResult.
	stranger := f.architect(t, "beta", "demo")
	if _, err := f.service.WaitForActionResult(ctx, stranger.ID, pending.ExecutionID, time.Second); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("stranger wait err = %v", err)
	}
}
