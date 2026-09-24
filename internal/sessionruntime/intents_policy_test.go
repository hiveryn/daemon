package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

var errBoom = errors.New("boom: could not write ticket")

// An unregistered tool must fail loudly. Defaulting to auto-allow would hand an
// agent an unapproved write the moment someone typos an intent type.
func TestPolicyForUnknownToolFailsFast(t *testing.T) {
	t.Parallel()

	_, err := policyFor(domain.IntentType("notARegisteredTool"))
	if err == nil {
		t.Fatal("expected an error for an unregistered tool, not a silent default")
	}
	if !strings.Contains(err.Error(), "notARegisteredTool") {
		t.Fatalf("error should name the offending tool, got %v", err)
	}
}

// Milestone-1 policy, asserted explicitly: concludeSession must keep today's
// exact auto-approve-on-timeout behavior.
func TestIntentPoliciesMilestoneOne(t *testing.T) {
	t.Parallel()

	want := map[domain.IntentType]domain.IntentPolicy{
		domain.IntentTypeConcludeSession:  domain.IntentPolicyWaitThenAllow,
		domain.IntentTypeCreateWorkTicket: domain.IntentPolicyWaitThenAllow,
		domain.IntentTypeExecuteAction:    domain.IntentPolicyManual,
	}
	for typ, wantPolicy := range want {
		got, err := policyFor(typ)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if got != wantPolicy {
			t.Errorf("%s policy = %q, want %q", typ, got, wantPolicy)
		}
	}
	// Test-only fixture types (intents_inputs_test.go) are registered by
	// TestMain and are not production tools.
	if got := len(intentPolicies) - len(testInputFixtureTypes); got != len(want) {
		t.Errorf("intentPolicies has %d production entries, want %d — add new tools to this test",
			got, len(want))
	}
}

// The auto-deny mirror branch must resolve WITHOUT running the side effect.
// Nothing uses it in milestone 1, so this is what keeps it honest.
func TestResolveByPolicyWaitThenDenyDoesNotExec(t *testing.T) {
	t.Parallel()

	service := &Service{}
	executed := false
	pending := &pendingIntent{
		intent: domain.Intent{ID: "i-1"},
		exec: func(context.Context, domain.IntentInputValues) (any, error) {
			executed = true
			return "should not happen", nil
		},
	}

	res := service.resolveByPolicy(context.Background(), pending, domain.IntentPolicyWaitThenDeny)

	if executed {
		t.Fatal("wait-then-deny must never run the side effect")
	}
	if res.Outcome != domain.IntentOutcomeAutoDenied {
		t.Fatalf("outcome = %q, want auto_denied", res.Outcome)
	}
	if res.Result != nil {
		t.Fatalf("a denied intent must carry no result, got %#v", res.Result)
	}
	if res.Reason == "" {
		t.Fatal("auto_denied must explain itself so the agent can tell the user")
	}
}

// wait-then-allow runs the side effect and reports auto_approved.
func TestResolveByPolicyWaitThenAllowExecs(t *testing.T) {
	t.Parallel()

	service := &Service{}
	pending := &pendingIntent{
		intent: domain.Intent{ID: "i-1"},
		exec:   func(context.Context, domain.IntentInputValues) (any, error) { return "did it", nil },
	}

	res := service.resolveByPolicy(context.Background(), pending, domain.IntentPolicyWaitThenAllow)

	if res.Outcome != domain.IntentOutcomeAutoApproved {
		t.Fatalf("outcome = %q, want auto_approved", res.Outcome)
	}
	if res.Result != "did it" {
		t.Fatalf("result = %#v, want the exec's return value", res.Result)
	}
}

// An exec failure surfaces as error (retryable), never as a denial.
func TestResolveByPolicyExecFailureIsErrorNotDenial(t *testing.T) {
	t.Parallel()

	service := &Service{}
	pending := &pendingIntent{
		intent: domain.Intent{ID: "i-1"},
		exec:   func(context.Context, domain.IntentInputValues) (any, error) { return nil, errBoom },
	}

	res := service.resolveByPolicy(context.Background(), pending, domain.IntentPolicyWaitThenAllow)

	if res.Outcome != domain.IntentOutcomeError {
		t.Fatalf("outcome = %q, want error", res.Outcome)
	}
	if !res.Outcome.Retryable() {
		t.Fatal("error must be retryable; a denial must not be")
	}
	if !strings.Contains(res.Reason, "boom") {
		t.Fatalf("reason should carry the full failure detail, got %q", res.Reason)
	}
}

func TestIntentOutcomeRetrySemantics(t *testing.T) {
	t.Parallel()

	// The whole point of the catalog: denials read "do not retry".
	for _, o := range []domain.IntentOutcome{domain.IntentOutcomeDeniedByUser, domain.IntentOutcomeAutoDenied} {
		if o.Retryable() {
			t.Errorf("%s must not be retryable", o)
		}
		if o.Approved() {
			t.Errorf("%s must not count as approved", o)
		}
	}
	for _, o := range []domain.IntentOutcome{domain.IntentOutcomeApproved, domain.IntentOutcomeAutoApproved} {
		if !o.Approved() {
			t.Errorf("%s must count as approved", o)
		}
		if o.Retryable() {
			t.Errorf("%s must not be retryable", o)
		}
	}
	if !domain.IntentOutcomeError.Retryable() {
		t.Error("error must be retryable")
	}
}
