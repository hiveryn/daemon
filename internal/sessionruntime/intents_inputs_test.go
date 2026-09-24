package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/domain"
)

// Fixture intent types. No production tool requests inputs yet (Actions will),
// so these are registered only for this package's tests, before any test runs.
const (
	testInputsWaitThenAllow domain.IntentType = "testInputsWaitThenAllow"
	testInputsWaitThenDeny  domain.IntentType = "testInputsWaitThenDeny"
	testInputsAutoAllow     domain.IntentType = "testInputsAutoAllow"
)

var testInputFixtureTypes = map[domain.IntentType]domain.IntentPolicy{
	testInputsWaitThenAllow: domain.IntentPolicyWaitThenAllow,
	testInputsWaitThenDeny:  domain.IntentPolicyWaitThenDeny,
	testInputsAutoAllow:     domain.IntentPolicyAutoAllow,
}

func TestMain(m *testing.M) {
	maps.Copy(intentPolicies, testInputFixtureTypes)
	os.Exit(m.Run())
}

func variantInputs(defaultVariant any) []domain.IntentInputField {
	return []domain.IntentInputField{
		{
			Name:     "variant",
			Label:    "Agent variant",
			Type:     domain.IntentInputChoice,
			Required: true,
			Default:  defaultVariant,
			Options:  []domain.IntentInputOption{{Value: "claude-opus", Label: "Claude Opus"}, {Value: "codex"}},
		},
		{Name: "note", Label: "Note", Type: domain.IntentInputText, MaxLength: 10},
		{Name: "notify", Label: "Notify", Type: domain.IntentInputBoolean, Default: false},
	}
}

// inputsFixture raises one fixture intent and records every exec.
type inputsFixture struct {
	service *Service
	repo    *fakeSessionRepository

	mu    sync.Mutex
	execs []domain.IntentInputValues
}

func newInputsFixture(t *testing.T, waitSeconds int) *inputsFixture {
	t.Helper()
	service, repo, _ := newCreateTicketService(t)
	service.cfg.IntentWaitTimeout = waitSeconds
	return &inputsFixture{service: service, repo: repo}
}

type inputsOutcome struct {
	res domain.IntentResolution[string]
	err error
}

func (f *inputsFixture) raise(typ domain.IntentType, fields []domain.IntentInputField) <-chan inputsOutcome {
	out := make(chan inputsOutcome, 1)
	go func() {
		res, err := awaitIntent(context.Background(), f.service, f.spec(typ, fields))
		out <- inputsOutcome{res, err}
	}()
	return out
}

func (f *inputsFixture) spec(typ domain.IntentType, fields []domain.IntentInputField) intentSpec[string] {
	return intentSpec[string]{
		SessionID: "session-1",
		Type:      typ,
		Summary:   "run an action",
		Payload:   map[string]any{"name": "triage"},
		Inputs:    fields,
		Origin:    domain.IntentOrigin{SessionID: "session-1", ArchitectKey: "hiveryn"},
		Exec: func(_ context.Context, in domain.IntentInputValues) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.execs = append(f.execs, in)
			variant, _ := in["variant"].(string)
			return "ran with " + variant, nil
		},
	}
}

func (f *inputsFixture) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs)
}

func (f *inputsFixture) pendingID(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := f.service.intents.PendingForSession("session-1"); len(ids) > 0 {
			return ids[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending intent was raised")
	return ""
}

func (f *inputsFixture) requiredEvent(t *testing.T) domain.AppendSessionEventParams {
	t.Helper()
	for _, e := range f.repo.events() {
		if e.Type == sessionEventTypeIntent && e.Status == sessionEventStatusReqd {
			return e
		}
	}
	t.Fatal("no intent/required event was published")
	return domain.AppendSessionEventParams{}
}

func awaitOutcome(t *testing.T, ch <-chan inputsOutcome, within time.Duration) inputsOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(within):
		t.Fatalf("intent did not resolve within %s", within)
		return inputsOutcome{}
	}
}

func TestCheckIntentInputSchemaRejectsMalformedSchemas(t *testing.T) {
	t.Parallel()

	choice := func(opts ...string) domain.IntentInputField {
		f := domain.IntentInputField{Name: "c", Label: "C", Type: domain.IntentInputChoice}
		for _, o := range opts {
			f.Options = append(f.Options, domain.IntentInputOption{Value: o})
		}
		return f
	}
	tooMany := make([]domain.IntentInputField, domain.MaxIntentInputFields+1)
	for i := range tooMany {
		tooMany[i] = domain.IntentInputField{Name: string(rune('a' + i)), Label: "x", Type: domain.IntentInputText}
	}

	cases := map[string][]domain.IntentInputField{
		"blank name":          {{Name: " ", Label: "x", Type: domain.IntentInputText}},
		"duplicate name":      {{Name: "a", Label: "x", Type: domain.IntentInputText}, {Name: "a", Label: "y", Type: domain.IntentInputText}},
		"missing label":       {{Name: "a", Type: domain.IntentInputText}},
		"unknown type":        {{Name: "a", Label: "x", Type: "date"}},
		"choice no options":   {choice()},
		"duplicate option":    {choice("x", "x")},
		"empty option value":  {choice("")},
		"options on text":     {{Name: "a", Label: "x", Type: domain.IntentInputText, Options: []domain.IntentInputOption{{Value: "v"}}}},
		"max_length on bool":  {{Name: "a", Label: "x", Type: domain.IntentInputBoolean, MaxLength: 3}},
		"negative max_length": {{Name: "a", Label: "x", Type: domain.IntentInputText, MaxLength: -1}},
		"too many fields":     tooMany,
	}
	for name, fields := range cases {
		if err := checkIntentInputSchema(fields); err == nil {
			t.Errorf("%s: expected a schema error", name)
		}
	}
	if err := checkIntentInputSchema(variantInputs("codex")); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}
	if err := checkIntentInputSchema(nil); err != nil {
		t.Fatalf("no inputs must be a valid schema: %v", err)
	}
}

func TestResolveIntentInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		fields    []domain.IntentInputField
		submitted domain.IntentInputValues
		want      domain.IntentInputValues
		issues    []string // "field: message-substring"
	}{
		{
			name:   "defaults alone",
			fields: variantInputs("codex"),
			want:   domain.IntentInputValues{"variant": "codex", "notify": false},
		},
		{
			name:      "submitted values win over defaults",
			fields:    variantInputs("codex"),
			submitted: domain.IntentInputValues{"variant": "claude-opus", "note": "hi", "notify": true},
			want:      domain.IntentInputValues{"variant": "claude-opus", "note": "hi", "notify": true},
		},
		{
			name:      "blank optional text is omitted",
			fields:    variantInputs("codex"),
			submitted: domain.IntentInputValues{"note": "   "},
			want:      domain.IntentInputValues{"variant": "codex", "notify": false},
		},
		{
			name:   "required choice without default",
			fields: variantInputs(nil),
			issues: []string{"variant: is required and has no default"},
		},
		{
			name:   "stale default is reported, never applied",
			fields: variantInputs("gone-variant"),
			issues: []string{`variant: default "gone-variant" is not one of the offered options`},
		},
		{
			name:      "blank required value",
			fields:    variantInputs(nil),
			submitted: domain.IntentInputValues{"variant": ""},
			issues:    []string{"variant: is required"},
		},
		{
			name:      "every problem is reported",
			fields:    variantInputs("codex"),
			submitted: domain.IntentInputValues{"variant": "nope", "note": "this is far too long", "notify": "yes", "extra": 1.0},
			issues: []string{
				"extra: is not an input",
				"note: is 20 characters, limit is 10",
				"notify: must be a boolean",
				`variant: "nope" is not one of the offered options`,
			},
		},
		{
			name:      "text is single-line",
			fields:    variantInputs("codex"),
			submitted: domain.IntentInputValues{"note": "a\nb"},
			issues:    []string{"note: must be a single line"},
		},
		{
			name:   "no schema, no values",
			fields: nil,
		},
		{
			name:      "no schema rejects values",
			submitted: domain.IntentInputValues{"variant": "codex"},
			issues:    []string{"variant: is not an input"},
		},
	}
	for _, tc := range cases {
		got, issues := resolveIntentInputs(tc.fields, tc.submitted)
		if len(issues) != len(tc.issues) {
			t.Errorf("%s: issues = %+v, want %v", tc.name, issues, tc.issues)
			continue
		}
		for i, want := range tc.issues {
			field, msg, _ := strings.Cut(want, ": ")
			if issues[i].Field != field || !strings.Contains(issues[i].Message, msg) {
				t.Errorf("%s: issue %d = %+v, want %q", tc.name, i, issues[i], want)
			}
		}
		if len(tc.issues) > 0 {
			if got != nil {
				t.Errorf("%s: values must be nil when there are issues, got %v", tc.name, got)
			}
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: values = %v, want %v", tc.name, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: values[%s] = %#v, want %#v", tc.name, k, got[k], v)
			}
		}
	}
}

// Invalid manual input must leave the intent pending and correctable: the
// exec does not run, the claim is not consumed, and a corrected submission
// then approves with exactly the validated values.
func TestApproveIntentInvalidInputStaysCorrectable(t *testing.T) {
	f := newInputsFixture(t, 20)
	done := f.raise(testInputsWaitThenAllow, variantInputs(nil))
	id := f.pendingID(t)

	raw := f.requiredEvent(t).Raw
	if _, ok := raw["inputs"]; !ok {
		t.Fatalf("intent/required event must carry the input schema, raw = %#v", raw)
	}
	if _, ok := raw["unresolved_inputs"]; !ok {
		t.Fatalf("a required input without default must be surfaced as unresolved, raw = %#v", raw)
	}

	_, err := f.service.ApproveIntent(context.Background(), "session-1", id, domain.IntentInputValues{"variant": "nope"})
	var vErr *domain.ValidationError
	if !errors.As(err, &vErr) || vErr.Field != "inputs" || !strings.Contains(vErr.Message, "variant") {
		t.Fatalf("invalid input error = %v, want a ValidationError naming the field", err)
	}
	_, err = f.service.ApproveIntent(context.Background(), "session-1", id, nil)
	if !errors.As(err, &vErr) || !strings.Contains(vErr.Message, "required") {
		t.Fatalf("missing input error = %v, want required", err)
	}
	if n := f.execCount(); n != 0 {
		t.Fatalf("exec ran %d times on invalid input", n)
	}
	if ids := f.service.intents.PendingForSession("session-1"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("intent must still be pending after invalid input, pending = %v", ids)
	}

	if _, err := f.service.ApproveIntent(context.Background(), "session-1", id,
		domain.IntentInputValues{"variant": "codex", "note": "go"}); err != nil {
		t.Fatalf("corrected approve: %v", err)
	}
	o := awaitOutcome(t, done, 2*time.Second)
	if o.err != nil || o.res.Outcome != domain.IntentOutcomeApproved || o.res.Result != "ran with codex" {
		t.Fatalf("resolution = %+v err=%v, want approved with codex", o.res, o.err)
	}
	want := domain.IntentInputValues{"variant": "codex", "note": "go", "notify": false}
	if len(o.res.Inputs) != len(want) || o.res.Inputs["variant"] != "codex" || o.res.Inputs["note"] != "go" || o.res.Inputs["notify"] != false {
		t.Fatalf("resolution inputs = %v, want %v", o.res.Inputs, want)
	}
	if f.execCount() != 1 {
		t.Fatalf("exec ran %d times, want exactly once", f.execCount())
	}
	resolved := f.repo.lastAppendedEvent(t)
	if resolved.Status != sessionEventStatusResolvd || resolved.Raw["inputs"] == nil {
		t.Fatalf("resolved event must record the approved inputs, got %#v", resolved)
	}

	// Already resolved: a late submission is not-found, never a second exec.
	_, err = f.service.ApproveIntent(context.Background(), "session-1", id, want)
	if !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("late approve err = %v, want NotFound", err)
	}
}

// wait-then-allow with valid defaults auto-approves with exactly the defaults.
func TestWaitThenAllowAutoApprovesWithValidatedDefaults(t *testing.T) {
	f := newInputsFixture(t, 1)
	done := f.raise(testInputsWaitThenAllow, variantInputs("claude-opus"))

	o := awaitOutcome(t, done, 3*time.Second)
	if o.err != nil || o.res.Outcome != domain.IntentOutcomeAutoApproved {
		t.Fatalf("resolution = %+v err=%v, want auto_approved", o.res, o.err)
	}
	if o.res.Result != "ran with claude-opus" || o.res.Inputs["variant"] != "claude-opus" {
		t.Fatalf("auto-approval must run with the default, got result=%q inputs=%v", o.res.Result, o.res.Inputs)
	}
	if _, ok := f.requiredEvent(t).Raw["unresolved_inputs"]; ok {
		t.Fatal("resolvable defaults must not be reported as unresolved")
	}
}

// A missing or invalid required default must never be executed automatically:
// the expiry passes, nothing runs, and the intent waits for the user.
func TestWaitThenAllowWithholdsAutoApprovalForUnresolvedInputs(t *testing.T) {
	for name, def := range map[string]any{"missing default": nil, "stale default": "gone-variant"} {
		t.Run(name, func(t *testing.T) {
			f := newInputsFixture(t, 1)
			done := f.raise(testInputsWaitThenAllow, variantInputs(def))
			id := f.pendingID(t)

			select {
			case o := <-done:
				t.Fatalf("intent resolved without user input: %+v err=%v", o.res, o.err)
			case <-time.After(1500 * time.Millisecond):
			}
			if f.execCount() != 0 {
				t.Fatal("automatic approval ran with unresolved inputs")
			}
			if ids := f.service.intents.PendingForSession("session-1"); len(ids) != 1 {
				t.Fatalf("intent must remain pending for the user, pending = %v", ids)
			}

			if _, err := f.service.ApproveIntent(context.Background(), "session-1", id,
				domain.IntentInputValues{"variant": "codex"}); err != nil {
				t.Fatalf("user completion: %v", err)
			}
			o := awaitOutcome(t, done, 2*time.Second)
			if o.res.Outcome != domain.IntentOutcomeApproved || o.res.Result != "ran with codex" {
				t.Fatalf("resolution = %+v, want approved with codex", o.res)
			}
		})
	}
}

// auto-allow runs immediately with valid defaults; with unresolved inputs it
// must not run and instead becomes a pending intent for the user.
func TestAutoAllowRequiresResolvableDefaults(t *testing.T) {
	f := newInputsFixture(t, 20)
	o := awaitOutcome(t, f.raise(testInputsAutoAllow, variantInputs("codex")), 2*time.Second)
	if o.res.Outcome != domain.IntentOutcomeApproved || o.res.Result != "ran with codex" {
		t.Fatalf("auto-allow resolution = %+v err=%v", o.res, o.err)
	}

	f = newInputsFixture(t, 20)
	done := f.raise(testInputsAutoAllow, variantInputs(nil))
	id := f.pendingID(t)
	if f.execCount() != 0 {
		t.Fatal("auto-allow ran with a missing required input")
	}
	if _, err := f.service.ApproveIntent(context.Background(), "session-1", id,
		domain.IntentInputValues{"variant": "claude-opus"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	o = awaitOutcome(t, done, 2*time.Second)
	if o.res.Outcome != domain.IntentOutcomeApproved || o.res.Result != "ran with claude-opus" {
		t.Fatalf("resolution = %+v, want approved with claude-opus", o.res)
	}
}

// Denial never requires input values, and wait-then-deny still auto-denies an
// intent whose inputs are unresolved.
func TestDenyNeedsNoInputs(t *testing.T) {
	f := newInputsFixture(t, 20)
	done := f.raise(testInputsWaitThenAllow, variantInputs(nil))
	id := f.pendingID(t)
	if err := f.service.DenyIntent(context.Background(), "session-1", id, "not now"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	o := awaitOutcome(t, done, 2*time.Second)
	if o.res.Outcome != domain.IntentOutcomeDeniedByUser || o.res.Reason != "not now" || o.res.Inputs != nil {
		t.Fatalf("resolution = %+v, want denied_by_user with the reason and no inputs", o.res)
	}

	f = newInputsFixture(t, 1)
	o = awaitOutcome(t, f.raise(testInputsWaitThenDeny, variantInputs(nil)), 3*time.Second)
	if o.res.Outcome != domain.IntentOutcomeAutoDenied {
		t.Fatalf("resolution = %+v, want auto_denied", o.res)
	}
	if f.execCount() != 0 {
		t.Fatal("a denied intent ran its exec")
	}
}

// The policy timer and a manual approve race through the same claim: whoever
// claims first resolves, and the loser never executes. Here the policy claims
// between the approve's validation and its claim.
func TestApproveLosesToPolicyClaimWithoutExecuting(t *testing.T) {
	f := newInputsFixture(t, 20)
	done := f.raise(testInputsWaitThenAllow, variantInputs("codex"))
	id := f.pendingID(t)

	pending, won := f.service.intents.Claim(id) // what runIntentPolicy does on expiry
	if !won {
		t.Fatal("policy claim failed")
	}
	_, err := f.service.ApproveIntent(context.Background(), "session-1", id, domain.IntentInputValues{"variant": "codex"})
	if !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("approve after policy claim err = %v, want NotFound", err)
	}
	res := f.service.resolveByPolicy(context.Background(), pending, domain.IntentPolicyWaitThenAllow)
	f.service.intents.Finish(id, res)

	o := awaitOutcome(t, done, 2*time.Second)
	if o.res.Outcome != domain.IntentOutcomeAutoApproved || f.execCount() != 1 {
		t.Fatalf("resolution = %+v execs=%d, want one auto-approval", o.res, f.execCount())
	}
}

// Concurrent manual approvals of one intent execute once.
func TestConcurrentApprovalsExecuteOnce(t *testing.T) {
	f := newInputsFixture(t, 20)
	done := f.raise(testInputsWaitThenAllow, variantInputs(nil))
	id := f.pendingID(t)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.service.ApproveIntent(context.Background(), "session-1", id, domain.IntentInputValues{"variant": "codex"})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.As(err, new(*domain.NotFoundError)) {
			t.Fatalf("unexpected approve error: %v", err)
		}
	}
	if ok != 1 || f.execCount() != 1 {
		t.Fatalf("successful approvals = %d, execs = %d; want exactly one of each", ok, f.execCount())
	}
	if o := awaitOutcome(t, done, 2*time.Second); o.res.Outcome != domain.IntentOutcomeApproved {
		t.Fatalf("resolution = %+v", o.res)
	}
}

// A retried call with the same payload and schema attaches to the pending
// intent and shares its resolution — values included — rather than minting a
// second intent; afterwards it replays.
func TestInputIntentRetryAttachesAndReplays(t *testing.T) {
	f := newInputsFixture(t, 20)
	first := f.raise(testInputsWaitThenAllow, variantInputs(nil))
	id := f.pendingID(t)
	second := f.raise(testInputsWaitThenAllow, variantInputs(nil))
	time.Sleep(50 * time.Millisecond)
	if ids := f.service.intents.PendingForSession("session-1"); len(ids) != 1 {
		t.Fatalf("retry minted a second intent: %v", ids)
	}

	if _, err := f.service.ApproveIntent(context.Background(), "session-1", id, domain.IntentInputValues{"variant": "codex"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	for i, ch := range []<-chan inputsOutcome{first, second} {
		o := awaitOutcome(t, ch, 2*time.Second)
		if o.res.IntentID != id || o.res.Inputs["variant"] != "codex" {
			t.Fatalf("caller %d resolution = %+v, want the shared intent with its inputs", i, o.res)
		}
	}
	o := awaitOutcome(t, f.raise(testInputsWaitThenAllow, variantInputs(nil)), time.Second)
	if o.res.IntentID != id || o.res.Inputs["variant"] != "codex" || f.execCount() != 1 {
		t.Fatalf("replay = %+v execs=%d, want the original outcome and one exec", o.res, f.execCount())
	}

	// A different schema is a different request.
	f.raise(testInputsWaitThenAllow, variantInputs("codex"))
	deadline := time.Now().Add(2 * time.Second)
	for len(f.service.intents.PendingForSession("session-1")) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ids := f.service.intents.PendingForSession("session-1"); len(ids) != 1 || ids[0] == id {
		t.Fatalf("a changed schema must raise a new intent, pending = %v", ids)
	}
}

// A malformed schema is a programmer error returned before any popup.
func TestAwaitIntentRejectsMalformedSchema(t *testing.T) {
	f := newInputsFixture(t, 20)
	o := awaitOutcome(t, f.raise(testInputsWaitThenAllow, []domain.IntentInputField{{Name: "x", Label: "X", Type: "date"}}), time.Second)
	if o.err == nil || !strings.Contains(o.err.Error(), "unsupported type") {
		t.Fatalf("err = %v, want schema error", o.err)
	}
	if len(f.repo.events()) != 0 {
		t.Fatal("a malformed schema must not publish an intent")
	}
}

// The desktop path, end to end through the real HTTP router: the schema
// arrives in the intent/required event, the desktop posts
// {"inputs": {...}} to the generic approve route, invalid values come back as
// a 400 that leaves the intent answerable, and valid values reach the exec.
func TestApproveIntentHTTPInputToExecution(t *testing.T) {
	f := newInputsFixture(t, 20)
	handler := api.NewHandler(api.Dependencies{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: f.service,
	})
	done := f.raise(testInputsWaitThenAllow, variantInputs(nil))
	id := f.pendingID(t)

	// Round-trip the event through JSON exactly as the SSE stream delivers it.
	var event struct {
		Raw struct {
			Inputs           []domain.IntentInputField `json:"inputs"`
			UnresolvedInputs []domain.IntentInputIssue `json:"unresolved_inputs"`
		} `json:"raw"`
	}
	encoded, err := json.Marshal(map[string]any{"raw": f.requiredEvent(t).Raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Raw.Inputs) != 3 || event.Raw.Inputs[0].Options[0].Label != "Claude Opus" {
		t.Fatalf("event schema = %+v", event.Raw.Inputs)
	}
	if len(event.Raw.UnresolvedInputs) != 1 || event.Raw.UnresolvedInputs[0].Field != "variant" {
		t.Fatalf("event unresolved = %+v", event.Raw.UnresolvedInputs)
	}

	approve := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/intents/"+id+"/approve", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, body := approve(`{"inputs":{"variant":"nope","notify":true}}`); code != http.StatusBadRequest || !strings.Contains(body, "variant") {
		t.Fatalf("invalid approve = %d %s, want 400 naming variant", code, body)
	}
	if code, _ := approve(``); code != http.StatusBadRequest {
		t.Fatalf("empty-body approve of a required input = %d, want 400", code)
	}
	if code, _ := approve(`{"values":{}}`); code != http.StatusBadRequest {
		t.Fatalf("unknown body field = %d, want 400", code)
	}
	if f.execCount() != 0 {
		t.Fatal("exec ran before valid input")
	}
	if code, body := approve(`{"inputs":{"variant":"codex","notify":true}}`); code != http.StatusOK {
		t.Fatalf("valid approve = %d %s", code, body)
	}
	o := awaitOutcome(t, done, 2*time.Second)
	if o.res.Outcome != domain.IntentOutcomeApproved || o.res.Inputs["variant"] != "codex" || o.res.Inputs["notify"] != true {
		t.Fatalf("resolution = %+v", o.res)
	}
}

// Existing no-input tools still approve with an empty body.
func TestApproveIntentHTTPWithoutInputs(t *testing.T) {
	f := newInputsFixture(t, 20)
	handler := api.NewHandler(api.Dependencies{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: f.service,
	})
	done := f.raise(testInputsWaitThenAllow, nil)
	id := f.pendingID(t)
	if _, ok := f.requiredEvent(t).Raw["inputs"]; ok {
		t.Fatal("an intent without inputs must not carry a schema")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/intents/"+id+"/approve", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	o := awaitOutcome(t, done, 2*time.Second)
	if o.res.Outcome != domain.IntentOutcomeApproved || o.res.Inputs != nil {
		t.Fatalf("resolution = %+v, want approved with no inputs", o.res)
	}
}
