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

// actionRequest is an executeAction call blocked on its approval. The
// embedded result is the execution as it was when the request was shown.
type actionRequest struct {
	domain.ActionResult
	done chan actionRequestOutcome
}

type actionRequestOutcome struct {
	res domain.ExecuteActionResponse
	err error
}

// call runs RequestExecuteAction in the background; it blocks until the
// request resolves.
func (f *actionFixture) call(sessionID string, req domain.ExecuteActionRequest) chan actionRequestOutcome {
	done := make(chan actionRequestOutcome, 1)
	go func() {
		res, err := f.service.RequestExecuteAction(context.Background(), sessionID, req)
		done <- actionRequestOutcome{res, err}
	}()
	return done
}

// request raises a new codex request and returns once it is shown.
func (f *actionFixture) request(t *testing.T, sessionID, name, prompt string) actionRequest {
	t.Helper()
	return f.requestVariant(t, sessionID, name, prompt, "codex")
}

func (f *actionFixture) requestVariant(t *testing.T, sessionID, name, prompt, variant string) actionRequest {
	t.Helper()
	before := map[string]bool{}
	for _, id := range f.service.intents.PendingForSession(sessionID) {
		before[id] = true
	}
	done := f.call(sessionID, domain.ExecuteActionRequest{Name: name, Prompt: prompt, Variant: variant})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case out := <-done:
			t.Fatalf("executeAction %s resolved before it was shown: %+v, %v", name, out.res, out.err)
		default:
		}
		for _, id := range f.service.intents.PendingForSession(sessionID) {
			if before[id] {
				continue
			}
			if _, err := f.service.GetActionRun(context.Background(), id); err != nil {
				continue
			}
			if _, shown := f.service.intents.GetForSession(sessionID, id); !shown {
				continue
			}
			return actionRequest{ActionResult: f.result(t, sessionID, id), done: done}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("executeAction %s was never shown", name)
	return actionRequest{}
}

// wait returns the request's resolution.
func (r actionRequest) wait(t *testing.T) (domain.ExecuteActionResponse, error) {
	t.Helper()
	return receiveOutcome(t, r.done)
}

func receiveOutcome(t *testing.T, done chan actionRequestOutcome) (domain.ExecuteActionResponse, error) {
	t.Helper()
	select {
	case out := <-done:
		return out.res, out.err
	case <-time.After(10 * time.Second):
		t.Fatal("executeAction did not resolve")
		return domain.ExecuteActionResponse{}, nil
	}
}

// approve approves a request: the variant was chosen when it was made, so the
// approval carries no inputs.
func (f *actionFixture) approve(sessionID, executionID string) error {
	_, err := f.service.ApproveIntent(context.Background(), sessionID, executionID, nil)
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
	if _, err := f.service.RequestExecuteAction(ctx, launched.Session.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x", Variant: "codex"}); !errors.As(err, new(*domain.ValidationError)) {
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

	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "claude"}
	for _, tc := range []struct {
		name, prompt, variant string
		want                  []string
	}{
		{"other", "x", "codex", []string{"not available"}},
		{"gone", "x", "codex", []string{"not found"}},
		{"broken", "x", "codex", []string{"invalid definition"}},
		{"demo", "  ", "codex", []string{"prompt"}},
		{"", "x", "codex", []string{"name"}},
		// A missing variant is never defaulted: the error lists every
		// configured variant and tells the agent to ask the user.
		{"demo", "x", "", []string{"variant is required", "no default", "Ask the user", "claude (claude)", "codex (codex)"}},
		{"demo", "x", "  ", []string{"variant is required", "Ask the user"}},
		{"demo", "x", "nope", []string{`"nope" is not a configured agent variant`, "Ask the user", "claude (claude), codex (codex)"}},
	} {
		_, err := f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: tc.name, Prompt: tc.prompt, Variant: tc.variant})
		if !errors.As(err, new(*domain.ValidationError)) {
			t.Errorf("executeAction(%q, %q, %q) err = %v, want validation error", tc.name, tc.prompt, tc.variant, err)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("executeAction(%q, %q, %q) err = %v, want it to mention %q", tc.name, tc.prompt, tc.variant, err, want)
			}
		}
	}
	variants := f.service.cfg.Variants
	f.service.cfg.Variants = map[string]config.VariantConfig{}
	if _, err := f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x", Variant: "codex"}); !errors.As(err, new(*domain.ValidationError)) || !strings.Contains(err.Error(), "no agent variants are configured") {
		t.Errorf("no variants err = %v, want validation error saying none are configured", err)
	}
	f.service.cfg.Variants = variants
	runs, _ := f.service.ListActionRuns(ctx, "", 0)
	if len(runs) != 0 {
		t.Fatalf("rejected requests left records: %+v", runs)
	}

	// Busy: a running execution (here a manual one) is an explicit error.
	manual := f.launch(t, "demo", "manual")
	_, err := f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x", Variant: "codex"})
	if !errors.As(err, new(*domain.ConflictError)) || !strings.Contains(err.Error(), manual.Run.ID) {
		t.Fatalf("busy executeAction err = %v, want conflict naming %s", err, manual.Run.ID)
	}
}

func TestExecuteActionWaitsForApprovalThenLaunchesUnderSameID(t *testing.T) {
	f := newActionFixture(t)
	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "claude"}
	f.service.cfg.IntentWaitTimeout = 20
	repoPath := f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	events := f.service.SubscribeActionEvents()
	defer events.Close()

	pending := f.requestVariant(t, arch.ID, "demo", "compare AMS and LDN", "codex")
	if pending.Status != domain.ActionRunPendingApproval || pending.ExecutionID == "" || pending.ProfileName != "codex" || pending.StartedAt != nil || pending.ElapsedSeconds != nil || pending.OutputDir != "" || pending.Activity.Available {
		t.Fatalf("pending = %+v, want pending_approval for codex with nothing started", pending)
	}
	if f.adapter.launchRequest.Workdir != "" {
		t.Fatal("an agent launched before approval")
	}
	if ev := <-events.C(); ev.ExecutionID != pending.ExecutionID || ev.Status != domain.ActionRunPendingApproval {
		t.Fatalf("event = %+v, want pending_approval", ev)
	}

	// The request is shown like createWorkTicket: a countdown, no inputs, the
	// requested variant as information.
	in, ok := f.service.intents.Get(pending.ExecutionID)
	if !ok || in.Type != domain.IntentTypeExecuteAction || in.Policy != domain.IntentPolicyWaitThenAllow || in.WaitSeconds != 20 || len(in.Inputs) != 0 {
		t.Fatalf("intent = %+v, %v; want a wait-then-allow executeAction without inputs under the execution id", in, ok)
	}
	if in.Payload["action"] != "demo" || in.Payload["prompt"] != "compare AMS and LDN" || in.Payload["variant"] != "codex" {
		t.Fatalf("payload = %+v", in.Payload)
	}
	select {
	case out := <-pending.done:
		t.Fatalf("executeAction returned before approval: %+v, %v", out.res, out.err)
	default:
	}

	if err := f.approve(arch.ID, pending.ExecutionID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	res, err := pending.wait(t)
	if err != nil || res.Outcome != domain.IntentOutcomeApproved || res.Result.ExecutionID != pending.ExecutionID || res.Result.Status != domain.ActionRunRunning {
		t.Fatalf("resolution = %+v, %v; want approved and running under the execution id", res, err)
	}
	running := f.result(t, arch.ID, pending.ExecutionID)
	if running.Status != domain.ActionRunRunning || running.ProfileName != "codex" || running.StartedAt == nil || running.ElapsedSeconds == nil || running.OutputDir == "" {
		t.Fatalf("after approve = %+v, want running codex with start and output", running)
	}
	run, _ := f.service.GetActionRun(context.Background(), pending.ExecutionID)
	if run.Trigger != domain.ActionRunTriggerArchitect || run.ArchitectKey != "alpha" || run.RequesterSessionID != arch.ID || run.SessionID == "" || run.ProfileName != "codex" {
		t.Fatalf("run = %+v", run)
	}
	if f.adapter.launchRequest.Workdir != repoPath || !strings.Contains(f.adapter.launchRequest.Prompt, "compare AMS and LDN") {
		t.Fatalf("agent launch = %+v", f.adapter.launchRequest)
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
	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "claude"}
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	codexReq := domain.ExecuteActionRequest{Name: "demo", Prompt: "go", Variant: "codex"}

	first := f.request(t, arch.ID, "demo", "go")
	// An identical call while it is pending attaches to it.
	again := f.call(arch.ID, codexReq)
	// Another variant is another request, never the same execution.
	other := f.requestVariant(t, arch.ID, "demo", "go", "claude")
	if other.ExecutionID == first.ExecutionID {
		t.Fatal("a request for another variant reused the execution")
	}
	if err := f.service.DenyIntent(ctx, arch.ID, first.ExecutionID, "not now"); err != nil {
		t.Fatal(err)
	}
	for _, done := range []chan actionRequestOutcome{first.done, again} {
		res, err := receiveOutcome(t, done)
		if err != nil || res.Outcome != domain.IntentOutcomeDeniedByUser || res.Reason != "not now" || res.Result.ExecutionID != first.ExecutionID || res.Result.Status != domain.ActionRunDenied {
			t.Fatalf("denied resolution = %+v, %v; want denied_by_user with the denied execution", res, err)
		}
	}
	denied := f.result(t, arch.ID, first.ExecutionID)
	if denied.Status != domain.ActionRunDenied || denied.Reason != "not now" || denied.StartedAt != nil || denied.EndedAt == nil || denied.ProfileName != "codex" {
		t.Fatalf("denied = %+v", denied)
	}
	if f.adapter.launchRequest.Workdir != "" {
		t.Fatal("a denied request launched an agent")
	}
	replay, err := f.service.RequestExecuteAction(ctx, arch.ID, codexReq)
	if err != nil || replay.Outcome != domain.IntentOutcomeDeniedByUser || replay.Result.ExecutionID != first.ExecutionID || replay.Result.Status != domain.ActionRunDenied {
		t.Fatalf("replay = %+v, %v; want the denied original at once", replay, err)
	}
	if err := f.approve(arch.ID, first.ExecutionID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("approve after deny err = %v", err)
	}
	// The other variant's request is still pending and independent.
	if got := f.result(t, arch.ID, other.ExecutionID); got.Status != domain.ActionRunPendingApproval || got.ProfileName != "claude" {
		t.Fatalf("other variant = %+v, want still pending for claude", got)
	}
	if err := f.service.DenyIntent(ctx, arch.ID, other.ExecutionID, "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.wait(t); err != nil {
		t.Fatal(err)
	}
	// Denial is history in the Actions window too.
	runs, _ := f.service.ListActionRuns(ctx, "demo", 0)
	if len(runs) != 2 || runs[0].Status != domain.ActionRunDenied || runs[1].Status != domain.ActionRunDenied {
		t.Fatalf("history = %+v", runs)
	}
}

func TestExecuteActionAutoApprovesWhenTheWindowExpires(t *testing.T) {
	f := newActionFixture(t)
	f.service.cfg.IntentWaitTimeout = 1
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")

	pending := f.request(t, arch.ID, "demo", "go")
	res, err := pending.wait(t)
	if err != nil || res.Outcome != domain.IntentOutcomeAutoApproved || res.Result.ExecutionID != pending.ExecutionID || res.Result.Status != domain.ActionRunRunning || res.Result.ProfileName != "codex" {
		t.Fatalf("resolution = %+v, %v; want auto_approved and running", res, err)
	}
	run, _ := f.service.GetActionRun(context.Background(), pending.ExecutionID)
	if run.Status != domain.ActionRunRunning || run.SessionID == "" || run.ProfileName != "codex" {
		t.Fatalf("run = %+v, want launched with codex", run)
	}
}

func TestExecuteActionReplayWindowIsTenMinutes(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()
	now := time.Now().UTC()
	var mu sync.Mutex
	f.service.intents.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	req := domain.ExecuteActionRequest{Name: "demo", Prompt: "go", Variant: "codex"}

	first := f.request(t, arch.ID, "demo", "go")
	// A pending request never expires: it is deduplicated, not replayed.
	advance(time.Hour)
	again := f.call(arch.ID, req)
	if err := f.service.DenyIntent(ctx, arch.ID, first.ExecutionID, "not now"); err != nil {
		t.Fatal(err)
	}
	if res, err := receiveOutcome(t, again); err != nil || res.Result.ExecutionID != first.ExecutionID {
		t.Fatalf("pending retry after 1h = %+v, %v; want the original %s", res, err, first.ExecutionID)
	}
	if _, err := first.wait(t); err != nil {
		t.Fatal(err)
	}

	advance(10 * time.Minute)
	if replay, err := f.service.RequestExecuteAction(ctx, arch.ID, req); err != nil || replay.Result.ExecutionID != first.ExecutionID || replay.Result.Status != domain.ActionRunDenied {
		t.Fatalf("inside window = %+v, %v; want the denied original", replay, err)
	}

	advance(time.Second)
	second := f.request(t, arch.ID, "demo", "go")
	if second.ExecutionID == first.ExecutionID || second.Status != domain.ActionRunPendingApproval {
		t.Fatalf("past window = %+v, want a new pending request", second)
	}
	if err := f.approve(arch.ID, second.ExecutionID); err != nil {
		t.Fatalf("approve new request: %v", err)
	}
	if res, err := second.wait(t); err != nil || res.Result.Status != domain.ActionRunRunning {
		t.Fatalf("new request = %+v, %v; want running", res, err)
	}

	// Once the running execution's replay expires, the single-run rule still holds.
	advance(11 * time.Minute)
	if _, err := f.service.RequestExecuteAction(ctx, arch.ID, req); !errors.As(err, new(*domain.ConflictError)) {
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
	err := f.approve(arch.ID, pending.ExecutionID)
	if !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("approve while busy err = %v, want conflict", err)
	}
	res, err := pending.wait(t)
	if err != nil || res.Outcome != domain.IntentOutcomeError || !strings.Contains(res.Reason, manual.Run.ID) || res.Result.Status != domain.ActionRunFailed {
		t.Fatalf("busy resolution = %+v, %v; want error with the failed execution", res, err)
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
	if err := f.approve(arch.ID, second.ExecutionID); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("approve after removal err = %v", err)
	}
	if got := f.result(t, arch.ID, second.ExecutionID); got.Status != domain.ActionRunFailed || !strings.Contains(got.Error, "not available") {
		t.Fatalf("removed approval = %+v", got)
	}
	if _, err := second.wait(t); err != nil {
		t.Fatalf("removed approval resolution err = %v", err)
	}

	// A variant removed from the configuration while pending.
	f.service.cfg.Architects["alpha"] = config.ArchitectConfig{Name: "alpha", Path: t.TempDir(), Repos: map[string]string{}, AvailableActions: []string{"demo"}}
	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "claude"}
	third := f.requestVariant(t, arch.ID, "demo", "third", "claude")
	delete(f.service.cfg.Variants, "claude")
	if err := f.approve(arch.ID, third.ExecutionID); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("approve after variant removal err = %v", err)
	}
	if got := f.result(t, arch.ID, third.ExecutionID); got.Status != domain.ActionRunFailed || !strings.Contains(got.Error, "not a configured agent variant") || got.StartedAt != nil {
		t.Fatalf("removed variant approval = %+v", got)
	}
	if _, err := third.wait(t); err != nil {
		t.Fatal(err)
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
			errs <- f.approve(arch.ID, id)
		}(id)
	}
	wg.Wait()
	close(errs)
	for _, r := range []actionRequest{a, b} {
		if _, err := r.wait(t); err != nil {
			t.Fatalf("resolution err = %v", err)
		}
	}

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
	// The requester is gone, so how its call ended is moot; it must end.
	_, _ = abandoned.wait(t)
	later := f.architect(t, "alpha", "demo", "live")
	if got := f.result(t, later.ID, abandoned.ExecutionID); got.Status != domain.ActionRunFailed || got.StartedAt != nil || !strings.Contains(got.Error, "session ended") {
		t.Fatalf("abandoned = %+v, want failed as never run", got)
	}

	pending := f.request(t, later.ID, "demo", "pending at restart")
	launched := f.request(t, later.ID, "live", "running at restart")
	if err := f.approve(later.ID, launched.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err := launched.wait(t); err != nil {
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
		_ = f.approve(arch.ID, pending.ExecutionID)
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
