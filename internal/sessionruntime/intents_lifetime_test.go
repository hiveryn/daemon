package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// slowIntent raises one blocking intent whose Exec waits for release, standing
// in for an approved operation that spends seconds in SSH (a remote
// conclusion). It returns the intent id, the agent-side result channel and the
// context Exec ran with.
func slowIntent(t *testing.T, service *Service, payload map[string]any, exec func(context.Context) (string, error)) (string, chan domain.IntentResolution[string]) {
	t.Helper()
	results := make(chan domain.IntentResolution[string], 1)
	go func() {
		res, err := awaitIntent(context.Background(), service, intentSpec[string]{
			SessionID: "session-1",
			Type:      domain.IntentTypeCreateWorkTicket,
			Summary:   "slow",
			Payload:   payload,
			Origin:    domain.IntentOrigin{SessionID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect},
			Exec: func(ctx context.Context, _ domain.IntentInputValues) (string, error) {
				return exec(ctx)
			},
		})
		if err != nil {
			t.Errorf("awaitIntent: %v", err)
		}
		results <- res
	}()
	return waitPendingIntent(t, service), results
}

func waitPendingIntent(t *testing.T, service *Service) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := service.intents.PendingForSession("session-1"); len(ids) == 1 {
			return ids[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no pending intent was raised")
	return ""
}

func intentEventStatuses(repo *fakeSessionRepository, intentID string) []string {
	var out []string
	for _, e := range repo.events() {
		if e.Type == sessionEventTypeIntent && e.Raw["intent_id"] == intentID {
			out = append(out, e.Status)
		}
	}
	return out
}

func lastResolvedRaw(t *testing.T, repo *fakeSessionRepository, intentID string) map[string]any {
	t.Helper()
	var raw map[string]any
	for _, e := range repo.events() {
		if e.Type == sessionEventTypeIntent && e.Status == sessionEventStatusResolvd && e.Raw["intent_id"] == intentID {
			raw = e.Raw
		}
	}
	if raw == nil {
		t.Fatalf("intent %s has no resolved event", intentID)
	}
	return raw
}

// The live failure: the desktop's request bound cancelled the approving
// request mid-operation, which killed the SSH teardown and then the resolved
// event's own write, stranding the intent as "resolving". Once approval is
// accepted the operation and its outcome belong to the daemon.
func TestApprovedIntentOutlivesTheApprovingRequest(t *testing.T) {
	service, repo, _ := newCreateTicketService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var execCtxErr error
	id, results := slowIntent(t, service, map[string]any{"n": 1}, func(ctx context.Context) (string, error) {
		close(started)
		<-release
		execCtxErr = ctx.Err()
		if _, ok := ctx.Deadline(); !ok {
			t.Error("approved operation has no daemon-owned deadline")
		}
		return "done", nil
	})

	request, stopWaiting := context.WithCancel(context.Background())
	approved := make(chan error, 1)
	go func() {
		_, err := service.ApproveIntent(request, "session-1", id, nil)
		approved <- err
	}()
	<-started

	// The intent is claimed and visibly resolving; answering it again is a
	// conflict (its outcome is on the way), never a 404 read as "resolved".
	if got := intentEventStatuses(repo, id); len(got) != 2 || got[1] != sessionEventStatusResolving {
		t.Fatalf("intent events = %v, want required then resolving", got)
	}
	if _, err := service.ApproveIntent(context.Background(), "session-1", id, nil); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("second approve = %v, want a conflict while resolving", err)
	}
	if err := service.DenyIntent(context.Background(), "session-1", id, "too late"); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("deny while resolving = %v, want a conflict", err)
	}

	stopWaiting() // the desktop's request bound fires
	close(release)

	if err := <-approved; err != nil {
		t.Fatalf("approve: %v", err)
	}
	if execCtxErr != nil {
		t.Fatalf("approved operation was cancelled with the approving request: %v", execCtxErr)
	}
	if res := <-results; res.Outcome != domain.IntentOutcomeApproved || res.Result != "done" {
		t.Fatalf("agent got %+v, want approved/done", res)
	}
	if raw := lastResolvedRaw(t, repo, id); raw["outcome"] != string(domain.IntentOutcomeApproved) {
		t.Fatalf("resolved event outcome = %v, want approved", raw["outcome"])
	}
	// A different session still cannot learn the intent exists.
	if _, err := service.ApproveIntent(context.Background(), "session-2", id, nil); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("foreign approve = %v, want not found", err)
	}
}

// A denial whose requester gives up right after the claim is still recorded.
func TestDenialIsRecordedAfterRequesterLeaves(t *testing.T) {
	service, repo, _ := newCreateTicketService(t)
	id, results := slowIntent(t, service, map[string]any{"n": 2}, func(context.Context) (string, error) {
		t.Error("a denied intent ran")
		return "", nil
	})
	request, stopWaiting := context.WithCancel(context.Background())
	stopWaiting()
	if err := service.DenyIntent(request, "session-1", id, "no"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if res := <-results; res.Outcome != domain.IntentOutcomeDeniedByUser {
		t.Fatalf("agent got %+v, want denied", res)
	}
	if raw := lastResolvedRaw(t, repo, id); raw["outcome"] != string(domain.IntentOutcomeDeniedByUser) {
		t.Fatalf("resolved outcome = %v", raw["outcome"])
	}
}

// A failed approved operation is reported, durably, and is not replayed: the
// agent's retry is a fresh request the user approves again — the failed
// approval is never reused as authorization — while an approved outcome still
// replays so a retry after response loss never repeats the effect.
func TestFailedApprovalIsNotReplayedButSuccessIs(t *testing.T) {
	service, repo, _ := newCreateTicketService(t)
	payload := map[string]any{"n": 3}
	fail := errors.New("remote termination not confirmed on bk; session retained: ssh: killed")
	id, results := slowIntent(t, service, payload, func(context.Context) (string, error) { return "", fail })
	if _, err := service.ApproveIntent(context.Background(), "session-1", id, nil); !errors.Is(err, fail) {
		t.Fatalf("approve = %v, want the operation's own error", err)
	}
	if res := <-results; res.Outcome != domain.IntentOutcomeError || res.Reason != fail.Error() {
		t.Fatalf("agent got %+v, want error with the original reason", res)
	}
	if raw := lastResolvedRaw(t, repo, id); raw["outcome"] != string(domain.IntentOutcomeError) || raw["reason"] != fail.Error() {
		t.Fatalf("resolved event = %v", raw)
	}

	runs := 0
	retry, results := slowIntent(t, service, payload, func(context.Context) (string, error) { runs++; return "ok", nil })
	if retry == id {
		t.Fatal("the retry replayed the failure instead of asking again")
	}
	if _, err := service.ApproveIntent(context.Background(), "session-1", retry, nil); err != nil {
		t.Fatalf("approve retry: %v", err)
	}
	<-results

	// Lost response, same call again: replayed, not run again.
	res, err := awaitIntent(context.Background(), service, intentSpec[string]{
		SessionID: "session-1", Type: domain.IntentTypeCreateWorkTicket, Summary: "slow", Payload: payload,
		Origin: domain.IntentOrigin{SessionID: "session-1"},
		Exec:   func(context.Context, domain.IntentInputValues) (string, error) { runs++; return "again", nil },
	})
	if err != nil || res.Outcome != domain.IntentOutcomeApproved || res.Result != "ok" || runs != 1 {
		t.Fatalf("replay = %+v, %v (runs %d), want the original approval without a second run", res, err, runs)
	}
}

// A restart while an approved operation was running cannot say it never ran.
func TestRestartReasonDistinguishesInterruptedOperations(t *testing.T) {
	events := []domain.SessionEvent{
		{Type: sessionEventTypeIntent, Status: sessionEventStatusReqd, Raw: map[string]any{"intent_id": "a", "intent_type": "concludeSession"}},
		{Type: sessionEventTypeIntent, Status: sessionEventStatusReqd, Raw: map[string]any{"intent_id": "b", "intent_type": "concludeSession"}},
		{Type: sessionEventTypeIntent, Status: sessionEventStatusResolving, Raw: map[string]any{"intent_id": "b", "by": "user"}},
	}
	open := unresolvedIntents("s", events)
	if len(open) != 2 {
		t.Fatalf("open = %v, want both (resolving is still open)", open)
	}
	resolving := resolvingIntentIDs(events)
	if resolving["a"] || !resolving["b"] {
		t.Fatalf("resolving = %v, want only b", resolving)
	}
}

// Ending a session is detached from its requester and admitted once at a
// time, so a retry after a client timeout cannot race the first teardown.
func TestSessionTeardownIsDetachedAndExclusive(t *testing.T) {
	service := &Service{}
	request, stopWaiting := context.WithCancel(context.Background())
	ctx, done, err := service.beginTeardown(request, "s1")
	if err != nil {
		t.Fatal(err)
	}
	stopWaiting()
	if ctx.Err() != nil {
		t.Fatal("teardown context died with its requester")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > sessionTeardownTimeout {
		t.Fatalf("teardown has no bound (deadline %v, %v)", deadline, ok)
	}
	if _, _, err := service.beginTeardown(context.Background(), "s1"); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("concurrent teardown = %v, want a conflict", err)
	}
	if _, otherDone, err := service.beginTeardown(context.Background(), "s2"); err != nil {
		t.Fatalf("other session: %v", err)
	} else {
		otherDone()
	}
	done()
	if ctx.Err() == nil {
		t.Fatal("finished teardown left its context running")
	}
	if _, again, err := service.beginTeardown(context.Background(), "s1"); err != nil {
		t.Fatalf("teardown after the first finished: %v", err)
	} else {
		again()
	}
	if intentExecTimeout <= sessionTeardownTimeout || intentExecTimeout <= defaultRunLaunchTimeout {
		t.Fatal("an approved operation must outlast the teardown and launch bounds it contains, so they report their own errors")
	}
}
