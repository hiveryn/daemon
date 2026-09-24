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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// Fixture intent types. No production tool requests inputs yet (Actions will),
// so these are registered only for this package's tests, before any test runs.
const (
	// testDeferred is an input-bearing deferred request, the shape Actions
	// will use.
	testDeferred domain.IntentType = "testDeferred"
	// testBlocking is an ordinary blocking tool without inputs.
	testBlocking domain.IntentType = "testBlocking"
)

var testInputFixtureTypes = map[domain.IntentType]domain.IntentPolicy{
	testDeferred: domain.IntentPolicyManual,
	testBlocking: domain.IntentPolicyWaitThenAllow,
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

// deferredFixture raises fixture intents against a real SQLite store and
// records every exec.
type deferredFixture struct {
	service *Service
	repo    *fakeSessionRepository
	dbPath  string

	mu      sync.Mutex
	execs   []domain.IntentInputValues
	execErr error
	onExec  func(context.Context)
}

func newDeferredFixture(t *testing.T, waitSeconds int) *deferredFixture {
	t.Helper()
	service, repo, _ := newCreateTicketService(t)
	service.cfg.IntentWaitTimeout = waitSeconds
	f := &deferredFixture{service: service, repo: repo, dbPath: filepath.Join(t.TempDir(), "hiveryn.db")}
	f.attachStore(t, service)
	return f
}

func (f *deferredFixture) attachStore(t *testing.T, service *Service) *store.DeferredIntentStore {
	t.Helper()
	db, err := store.Open(context.Background(), f.dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := store.NewDeferredIntentStore(db)
	service.SetDeferredIntentRepository(repo)
	return repo
}

func (f *deferredFixture) spec(typ domain.IntentType, fields []domain.IntentInputField) intentSpec[string] {
	return intentSpec[string]{
		SessionID: "session-1",
		Type:      typ,
		Summary:   "run an action",
		Payload:   map[string]any{"name": "triage"},
		Inputs:    fields,
		Origin:    domain.IntentOrigin{SessionID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect},
		Exec: func(ctx context.Context, in domain.IntentInputValues) (string, error) {
			f.mu.Lock()
			f.execs = append(f.execs, in)
			execErr, onExec := f.execErr, f.onExec
			f.mu.Unlock()
			if onExec != nil {
				onExec(ctx)
			}
			if execErr != nil {
				return "", execErr
			}
			variant, _ := in["variant"].(string)
			return "ran with " + variant, nil
		},
	}
}

func (f *deferredFixture) submit(t *testing.T, fields []domain.IntentInputField) domain.DeferredIntent {
	t.Helper()
	record, err := submitDeferredIntent(context.Background(), f.service, f.spec(testDeferred, fields))
	if err != nil {
		t.Fatalf("submit deferred intent: %v", err)
	}
	return record
}

func (f *deferredFixture) lookup(t *testing.T, id string) domain.DeferredIntent {
	t.Helper()
	record, err := f.service.GetDeferredIntent(context.Background(), "session-1", id)
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	return record
}

func (f *deferredFixture) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs)
}

func (f *deferredFixture) eventsFor(id, status string) []domain.AppendSessionEventParams {
	var out []domain.AppendSessionEventParams
	for _, e := range f.repo.events() {
		if e.Type == sessionEventTypeIntent && e.Status == status && e.Raw["intent_id"] == id {
			out = append(out, e)
		}
	}
	return out
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
			name:   "defaults are never applied",
			fields: variantInputs("codex"),
			issues: []string{"variant: is required"},
		},
		{
			name:      "submitted values are taken as submitted",
			fields:    variantInputs("codex"),
			submitted: domain.IntentInputValues{"variant": "claude-opus", "note": "hi", "notify": true},
			want:      domain.IntentInputValues{"variant": "claude-opus", "note": "hi", "notify": true},
		},
		{
			name:      "blank optional text and absent optional boolean are omitted",
			fields:    variantInputs("codex"),
			submitted: domain.IntentInputValues{"variant": "codex", "note": "   "},
			want:      domain.IntentInputValues{"variant": "codex"},
		},
		{
			name:      "a stale default does not matter once a value is submitted",
			fields:    variantInputs("gone-variant"),
			submitted: domain.IntentInputValues{"variant": "codex"},
			want:      domain.IntentInputValues{"variant": "codex"},
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
			submitted: domain.IntentInputValues{"variant": "codex", "note": "a\nb"},
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

// A deferred request returns at once with a stable id and pending_approval,
// and nothing resolves it on a timer — not even when every field has a valid
// default.
func TestDeferredSubmitReturnsImmediatelyAndNeverAutoResolves(t *testing.T) {
	f := newDeferredFixture(t, 1)

	started := time.Now()
	record := f.submit(t, variantInputs("codex"))
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("submit took %s; a deferred request must not block", elapsed)
	}
	if record.ID == "" || record.Status != domain.DeferredIntentPendingApproval || record.ApprovedAt != nil || record.EndedAt != nil {
		t.Fatalf("submit record = %+v, want a pending_approval record with an id", record)
	}

	required := f.eventsFor(record.ID, sessionEventStatusReqd)
	if len(required) != 1 {
		t.Fatalf("required events = %d, want 1", len(required))
	}
	raw := required[0].Raw
	if raw["policy"] != string(domain.IntentPolicyManual) || raw["wait_seconds"] != 0 || raw["inputs"] == nil {
		t.Fatalf("required event raw = %#v, want policy manual, wait_seconds 0 and the schema", raw)
	}
	if _, ok := raw["unresolved_inputs"]; ok {
		t.Fatal("unresolved_inputs no longer exists")
	}

	// Well past the configured wait window.
	time.Sleep(1500 * time.Millisecond)
	if f.execCount() != 0 {
		t.Fatal("a deferred intent executed without explicit approval")
	}
	if got := f.lookup(t, record.ID); got.Status != domain.DeferredIntentPendingApproval {
		t.Fatalf("after the wait window status = %s, want pending_approval", got.Status)
	}
	if len(f.eventsFor(record.ID, sessionEventStatusResolvd)) != 0 {
		t.Fatal("a deferred intent was resolved without the user")
	}
}

// Invalid or missing values leave the request pending and correctable, the
// defaults never fill in, and a corrected approval executes exactly once with
// the submitted values. The lookup then reports approval and success
// separately.
func TestDeferredApproveIsCorrectableAndExplicit(t *testing.T) {
	f := newDeferredFixture(t, 20)
	record := f.submit(t, variantInputs("codex"))
	ctx := context.Background()

	var vErr *domain.ValidationError
	if _, err := f.service.ApproveIntent(ctx, "session-1", record.ID, nil); !errors.As(err, &vErr) || !strings.Contains(vErr.Message, "variant is required") {
		t.Fatalf("approve without values err = %v; the default must not stand in", err)
	}
	if _, err := f.service.ApproveIntent(ctx, "session-1", record.ID, domain.IntentInputValues{"variant": "nope"}); !errors.As(err, &vErr) || vErr.Field != "inputs" {
		t.Fatalf("invalid approve err = %v, want a ValidationError", err)
	}
	if f.execCount() != 0 || f.lookup(t, record.ID).Status != domain.DeferredIntentPendingApproval {
		t.Fatal("invalid input must leave the request pending without executing")
	}

	if _, err := f.service.ApproveIntent(ctx, "session-1", record.ID, domain.IntentInputValues{"variant": "claude-opus", "note": "go", "notify": true}); err != nil {
		t.Fatalf("corrected approve: %v", err)
	}
	got := f.lookup(t, record.ID)
	if got.Status != domain.DeferredIntentCompleted || got.Result != "ran with claude-opus" || got.ApprovedAt == nil || got.EndedAt == nil {
		t.Fatalf("record = %+v, want completed with its result and both timestamps", got)
	}
	if got.Inputs["variant"] != "claude-opus" || got.Inputs["note"] != "go" || got.Inputs["notify"] != true {
		t.Fatalf("record inputs = %v, want the approved values", got.Inputs)
	}
	if f.execCount() != 1 {
		t.Fatalf("execs = %d, want exactly one", f.execCount())
	}
	resolved := f.eventsFor(record.ID, sessionEventStatusResolvd)
	if len(resolved) != 1 || resolved[0].Raw["status"] != string(domain.DeferredIntentCompleted) || resolved[0].Raw["outcome"] != string(domain.IntentOutcomeApproved) {
		t.Fatalf("resolved events = %#v, want one approved/completed", resolved)
	}

	if _, err := f.service.ApproveIntent(ctx, "session-1", record.ID, domain.IntentInputValues{"variant": "codex"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("late approve err = %v, want NotFound", err)
	}
	if f.execCount() != 1 {
		t.Fatal("a late approval executed again")
	}
}

// Denial needs no field completion, keeps the reason, and never executes.
func TestDeferredDenyKeepsReason(t *testing.T) {
	f := newDeferredFixture(t, 20)
	record := f.submit(t, variantInputs(nil))

	if err := f.service.DenyIntent(context.Background(), "session-1", record.ID, "not today"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	got := f.lookup(t, record.ID)
	if got.Status != domain.DeferredIntentDenied || got.Reason != "not today" || got.ApprovedAt != nil || got.EndedAt == nil {
		t.Fatalf("record = %+v, want denied with the reason", got)
	}
	if _, err := f.service.ApproveIntent(context.Background(), "session-1", record.ID, domain.IntentInputValues{"variant": "codex"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("approve after deny err = %v, want NotFound", err)
	}
	if f.execCount() != 0 {
		t.Fatal("a denied request executed")
	}
}

// An approved request whose operation fails is failed with the error and the
// approval time: accepted, but not successful.
func TestDeferredExecutionFailureIsNotSuccess(t *testing.T) {
	f := newDeferredFixture(t, 20)
	f.execErr = errors.New("boom")
	record := f.submit(t, variantInputs(nil))

	_, err := f.service.ApproveIntent(context.Background(), "session-1", record.ID, domain.IntentInputValues{"variant": "codex"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("approve err = %v, want the execution failure surfaced", err)
	}
	got := f.lookup(t, record.ID)
	if got.Status != domain.DeferredIntentFailed || got.Error != "boom" || got.ApprovedAt == nil || got.Result != nil {
		t.Fatalf("record = %+v, want failed after approval with the error", got)
	}
}

// The operation is independent of the approving request: the approver going
// away mid-run neither cancels it nor leaves the record running.
func TestDeferredExecutionOutlivesApprovingRequest(t *testing.T) {
	f := newDeferredFixture(t, 20)
	ctx, cancel := context.WithCancel(context.Background())
	var execCtxErr error
	f.onExec = func(execCtx context.Context) {
		cancel()
		execCtxErr = execCtx.Err()
	}
	record := f.submit(t, variantInputs(nil))

	if _, err := f.service.ApproveIntent(ctx, "session-1", record.ID, domain.IntentInputValues{"variant": "codex"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if execCtxErr != nil {
		t.Fatalf("exec ctx was cancelled with the approving request: %v", execCtxErr)
	}
	if got := f.lookup(t, record.ID); got.Status != domain.DeferredIntentCompleted {
		t.Fatalf("status = %s, want completed", got.Status)
	}
}

// Concurrent approvals and a concurrent denial race through one claim: exactly
// one resolves it, the operation runs at most once, and the record agrees.
func TestDeferredConcurrentResolutionIsSingleWinner(t *testing.T) {
	f := newDeferredFixture(t, 20)
	record := f.submit(t, variantInputs(nil))

	var wg sync.WaitGroup
	errs := make([]error, 9)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i == 0 {
				errs[i] = f.service.DenyIntent(context.Background(), "session-1", record.ID, "")
				return
			}
			_, errs[i] = f.service.ApproveIntent(context.Background(), "session-1", record.ID, domain.IntentInputValues{"variant": "codex"})
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
		} else if !errors.As(err, new(*domain.NotFoundError)) {
			t.Fatalf("unexpected resolution error: %v", err)
		}
	}
	got := f.lookup(t, record.ID)
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly one", winners)
	}
	switch got.Status {
	case domain.DeferredIntentCompleted:
		if f.execCount() != 1 || errs[0] == nil {
			t.Fatalf("completed with execs=%d deny err=%v", f.execCount(), errs[0])
		}
	case domain.DeferredIntentDenied:
		if f.execCount() != 0 || errs[0] != nil {
			t.Fatalf("denied with execs=%d deny err=%v", f.execCount(), errs[0])
		}
	default:
		t.Fatalf("status = %s, want completed or denied", got.Status)
	}
}

// Retries of the same request, even concurrent ones, return the same id and
// never mint a second request; after resolution the retry returns the
// resolved outcome. A changed schema is a different request.
func TestDeferredRetryReturnsTheSameID(t *testing.T) {
	f := newDeferredFixture(t, 20)

	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = f.submit(t, variantInputs(nil)).ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("retries returned different ids: %v", ids)
		}
	}
	if pending := f.service.intents.PendingForSession("session-1"); len(pending) != 1 {
		t.Fatalf("pending = %v, want one request", pending)
	}
	if n := len(f.eventsFor(ids[0], sessionEventStatusReqd)); n != 1 {
		t.Fatalf("required events = %d, want one popup", n)
	}

	if _, err := f.service.ApproveIntent(context.Background(), "session-1", ids[0], domain.IntentInputValues{"variant": "codex"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	replayed := f.submit(t, variantInputs(nil))
	if replayed.ID != ids[0] || replayed.Status != domain.DeferredIntentCompleted {
		t.Fatalf("retry after resolution = %+v, want the original id completed", replayed)
	}
	if f.execCount() != 1 {
		t.Fatalf("execs = %d, want one", f.execCount())
	}

	if other := f.submit(t, variantInputs("codex")); other.ID == ids[0] || other.Status != domain.DeferredIntentPendingApproval {
		t.Fatalf("changed schema = %+v, want a new pending request", other)
	}
}

// A lookup is scoped to the session that raised the request.
func TestDeferredLookupIsSessionScoped(t *testing.T) {
	f := newDeferredFixture(t, 20)
	record := f.submit(t, variantInputs(nil))

	if _, err := f.service.GetDeferredIntent(context.Background(), "session-2", record.ID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("other session lookup err = %v, want NotFound", err)
	}
	if _, err := f.service.GetDeferredIntent(context.Background(), "session-1", "no-such-id"); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("unknown id err = %v, want NotFound", err)
	}
	if _, err := f.service.ApproveIntent(context.Background(), "session-2", record.ID, domain.IntentInputValues{"variant": "codex"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("other session approve err = %v, want NotFound", err)
	}
}

// Session teardown fails a still-pending request instead of leaving it
// falsely pending; it never ran.
func TestDeferredSessionTeardownFailsPending(t *testing.T) {
	f := newDeferredFixture(t, 20)
	record := f.submit(t, variantInputs(nil))

	f.service.failPendingIntents(context.Background(), "session-1", "session ended before this intent was resolved")

	got := f.lookup(t, record.ID)
	if got.Status != domain.DeferredIntentFailed || got.Error != deferredFailedOnSessionEnd || got.ApprovedAt != nil {
		t.Fatalf("record = %+v, want failed before approval", got)
	}
	resolved := f.eventsFor(record.ID, sessionEventStatusResolvd)
	if len(resolved) != 1 || resolved[0].Raw["status"] != string(domain.DeferredIntentFailed) {
		t.Fatalf("resolved events = %#v, want one failed", resolved)
	}
	if _, err := f.service.ApproveIntent(context.Background(), "session-1", record.ID, domain.IntentInputValues{"variant": "codex"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("approve after teardown err = %v, want NotFound", err)
	}
	if f.execCount() != 0 {
		t.Fatal("a torn-down request executed")
	}
}

// A restart cannot resume a captured operation. Startup fails every open
// record — pending ones as never run, running ones as interrupted — and
// converges the event log, so nothing stays falsely pending.
func TestDeferredRestartFailsOpenRecords(t *testing.T) {
	f := newDeferredFixture(t, 20)
	pending := f.submit(t, variantInputs(nil))

	// A second request caught mid-execution by the restart.
	approvedAt := time.Now().UTC()
	running := domain.DeferredIntent{
		ID: "running-1", Type: testDeferred, Summary: "mid-run", Status: domain.DeferredIntentRunning,
		Origin:    domain.IntentOrigin{SessionID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect},
		Inputs:    domain.IntentInputValues{"variant": "codex"},
		CreatedAt: approvedAt, ApprovedAt: &approvedAt,
	}
	if err := f.service.deferred.CreateDeferredIntent(context.Background(), running); err != nil {
		t.Fatal(err)
	}

	// The durable log the next process starts from.
	var logged []domain.SessionEvent
	for _, e := range f.repo.events() {
		logged = append(logged, domain.SessionEvent{SessionID: e.SessionID, Type: e.Type, Status: e.Status, Message: e.Message, Raw: e.Raw, At: e.At})
	}

	restarted, repo, _ := newCreateTicketService(t)
	repo.sessionEvents = map[string][]domain.SessionEvent{"session-1": logged}
	f.attachStore(t, restarted)
	if err := restarted.ReconcileIntents(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, err := restarted.GetDeferredIntent(context.Background(), "session-1", pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.DeferredIntentFailed || got.Error != deferredFailedOnRestartPending || got.ApprovedAt != nil || got.EndedAt == nil {
		t.Fatalf("pending after restart = %+v, want failed, never ran", got)
	}
	got, err = restarted.GetDeferredIntent(context.Background(), "session-1", running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.DeferredIntentFailed || got.Error != deferredFailedOnRestartRunning || got.ApprovedAt == nil {
		t.Fatalf("running after restart = %+v, want failed as interrupted", got)
	}

	var resolved []domain.AppendSessionEventParams
	for _, e := range repo.events() {
		if e.Status == sessionEventStatusResolvd && e.Raw["intent_id"] == pending.ID {
			resolved = append(resolved, e)
		}
	}
	if len(resolved) != 1 || resolved[0].Raw["status"] != string(domain.DeferredIntentFailed) {
		t.Fatalf("restart resolved events = %#v, want the popup cleared as failed", resolved)
	}
	if _, err := restarted.ApproveIntent(context.Background(), "session-1", pending.ID, domain.IntentInputValues{"variant": "codex"}); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("approve after restart err = %v, want NotFound", err)
	}
}

// Blocking and deferred paths are not interchangeable: a blocking wait never
// takes an input form (it would block the agent on it, or approve it on a
// timer), and each path refuses the other's policy.
func TestIntentPathsRefuseTheWrongShape(t *testing.T) {
	f := newDeferredFixture(t, 20)
	ctx := context.Background()

	if _, err := awaitIntent(ctx, f.service, f.spec(testBlocking, variantInputs("codex"))); err == nil || !strings.Contains(err.Error(), "must be deferred") {
		t.Fatalf("blocking with inputs err = %v", err)
	}
	if _, err := awaitIntent(ctx, f.service, f.spec(testDeferred, nil)); err == nil || !strings.Contains(err.Error(), "deferred") {
		t.Fatalf("blocking wait on a manual policy err = %v", err)
	}
	if _, err := submitDeferredIntent(ctx, f.service, f.spec(testBlocking, nil)); err == nil || !strings.Contains(err.Error(), "not deferred") {
		t.Fatalf("deferred submit of a blocking policy err = %v", err)
	}
	if _, err := submitDeferredIntent(ctx, f.service, f.spec(testDeferred, []domain.IntentInputField{{Name: "x", Label: "X", Type: "date"}})); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("malformed schema err = %v", err)
	}

	unwired, _, _ := newCreateTicketService(t)
	if _, err := submitDeferredIntent(ctx, unwired, f.spec(testDeferred, nil)); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("submit without a repository err = %v", err)
	}
	if len(f.repo.events()) != 0 || f.execCount() != 0 {
		t.Fatal("a refused request must publish and run nothing")
	}
}

// The desktop and lookup paths end to end through the real HTTP router: the
// schema arrives in the intent/required event, invalid values are a 400 that
// leaves the request answerable, and the stable id retrieves the pending and
// then the resolved outcome.
func TestDeferredIntentHTTPApproveAndLookup(t *testing.T) {
	f := newDeferredFixture(t, 20)
	handler := api.NewHandler(api.Dependencies{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: f.service,
	})
	record := f.submit(t, variantInputs("codex"))

	do := func(method, path, body string) (int, []byte) {
		var reader io.Reader
		if body != "" {
			reader = bytes.NewBufferString(body)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, reader))
		return rec.Code, rec.Body.Bytes()
	}
	lookup := func(session string) (int, domain.DeferredIntent) {
		code, body := do(http.MethodGet, "/api/sessions/"+session+"/intents/"+record.ID, "")
		var env struct {
			Data domain.DeferredIntent `json:"data"`
		}
		if code == http.StatusOK {
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode lookup: %v (%s)", err, body)
			}
		}
		return code, env.Data
	}

	var event struct {
		Raw struct {
			Inputs      []domain.IntentInputField `json:"inputs"`
			Policy      string                    `json:"policy"`
			WaitSeconds int                       `json:"wait_seconds"`
		} `json:"raw"`
	}
	encoded, err := json.Marshal(map[string]any{"raw": f.eventsFor(record.ID, sessionEventStatusReqd)[0].Raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Raw.Inputs) != 3 || event.Raw.Inputs[0].Default != "codex" || event.Raw.Policy != "manual" || event.Raw.WaitSeconds != 0 {
		t.Fatalf("event = %+v", event.Raw)
	}

	if code, got := lookup("session-1"); code != http.StatusOK || got.ID != record.ID || got.Status != domain.DeferredIntentPendingApproval {
		t.Fatalf("pending lookup = %d %+v", code, got)
	}
	approvePath := "/api/sessions/session-1/intents/" + record.ID + "/approve"
	if code, body := do(http.MethodPost, approvePath, `{"inputs":{"variant":"nope"}}`); code != http.StatusBadRequest || !strings.Contains(string(body), "variant") {
		t.Fatalf("invalid approve = %d %s, want 400 naming variant", code, body)
	}
	if code, _ := do(http.MethodPost, approvePath, ""); code != http.StatusBadRequest {
		t.Fatalf("empty approve of a required input = %d, want 400 (defaults never apply)", code)
	}
	if f.execCount() != 0 {
		t.Fatal("exec ran before a valid approval")
	}
	if code, body := do(http.MethodPost, approvePath, `{"inputs":{"variant":"codex","notify":true}}`); code != http.StatusOK {
		t.Fatalf("valid approve = %d %s", code, body)
	}
	code, got := lookup("session-1")
	if code != http.StatusOK || got.Status != domain.DeferredIntentCompleted || got.Result != "ran with codex" || got.Inputs["notify"] != true || got.ApprovedAt == nil {
		t.Fatalf("resolved lookup = %d %+v", code, got)
	}
	if code, _ := lookup("session-2"); code != http.StatusNotFound {
		t.Fatalf("other session lookup = %d, want 404", code)
	}
}

// Existing blocking tools without inputs keep their behavior: they block,
// approve with an empty body, and the policy still auto-approves on expiry.
func TestBlockingIntentWithoutInputsIsUnchanged(t *testing.T) {
	f := newDeferredFixture(t, 20)
	handler := api.NewHandler(api.Dependencies{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: f.service,
	})

	type outcome struct {
		res domain.IntentResolution[string]
		err error
	}
	raise := func() <-chan outcome {
		ch := make(chan outcome, 1)
		go func() {
			res, err := awaitIntent(context.Background(), f.service, f.spec(testBlocking, nil))
			ch <- outcome{res, err}
		}()
		return ch
	}
	pendingID := func() string {
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
	await := func(ch <-chan outcome, within time.Duration) outcome {
		select {
		case o := <-ch:
			return o
		case <-time.After(within):
			t.Fatalf("blocking intent did not resolve within %s", within)
			return outcome{}
		}
	}

	done := raise()
	id := pendingID()
	raw := f.eventsFor(id, sessionEventStatusReqd)[0].Raw
	if _, ok := raw["inputs"]; ok || raw["policy"] != string(domain.IntentPolicyWaitThenAllow) || raw["wait_seconds"] != 20 {
		t.Fatalf("blocking required raw = %#v", raw)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/intents/"+id+"/approve", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if o := await(done, 2*time.Second); o.err != nil || o.res.Outcome != domain.IntentOutcomeApproved || o.res.Inputs != nil {
		t.Fatalf("resolution = %+v err=%v", o.res, o.err)
	}
	// A blocking intent has no durable record to look up.
	lookup := httptest.NewRecorder()
	handler.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/api/sessions/session-1/intents/"+id, nil))
	if lookup.Code != http.StatusNotFound {
		t.Fatalf("blocking lookup = %d, want 404", lookup.Code)
	}

	expiring := newDeferredFixture(t, 1)
	res, err := awaitIntent(context.Background(), expiring.service, expiring.spec(testBlocking, nil))
	if err != nil || res.Outcome != domain.IntentOutcomeAutoApproved || expiring.execCount() != 1 {
		t.Fatalf("expiry resolution = %+v err=%v execs=%d, want auto_approved", res, err, expiring.execCount())
	}
}
