package sessionruntime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

// intentSpec describes one approvable tool call. Exec is the side effect, and
// it runs only after the intent is approved (or auto-approved) — never before.
// Keeping the write inside Exec is what makes approval unbypassable: the agent
// has no path to the write except through an intent the daemon resolved.
//
// Inputs is the optional approval-input schema. Exec receives only values
// validated against it (nil when there are none): the user's on manual
// approval, the defaults on automatic approval — and automatic approval never
// runs while a default is missing or invalid.
type intentSpec[R any] struct {
	SessionID string
	Type      domain.IntentType
	Summary   string
	Payload   map[string]any // desktop-renderable; also the dedup input
	Inputs    []domain.IntentInputField
	Origin    domain.IntentOrigin
	Exec      func(context.Context, domain.IntentInputValues) (R, error)
}

// awaitIntent is the generic blocking wait shared by every approvable tool.
//
// The agent's MCP call parks here — held open by an untimed http.DefaultClient
// — until the user answers or the tool's policy fires. No polling.
//
// Ownership note: the intent deliberately does NOT die with the caller's ctx.
// An agent-runtime tool-call timeout cancels ctx and retries, which is exactly
// the case idempotency must survive; if ctx death killed the intent, the retry
// would find nothing cached and duplicate the write. So a detached owner
// goroutine holds the timer, and a caller whose ctx dies merely detaches.
func awaitIntent[R any](ctx context.Context, s *Service, spec intentSpec[R]) (domain.IntentResolution[R], error) {
	var zero domain.IntentResolution[R]

	policy, err := policyFor(spec.Type)
	if err != nil {
		return zero, err
	}
	if err := checkIntentInputSchema(spec.Inputs); err != nil {
		return zero, fmt.Errorf("%s: %w", spec.Type, err)
	}
	// Defaults are resolved once: the schema is immutable for the intent's
	// life, so the verdict cannot change while it waits.
	defaults, unresolved := resolveIntentInputs(spec.Inputs, nil)

	// Type-erase once, here: the store holds heterogeneous intents, so it
	// cannot be generic, and this closure is what lets it stay type-agnostic.
	erased := func(ctx context.Context, inputs domain.IntentInputValues) (any, error) {
		v, err := spec.Exec(ctx, inputs)
		if err != nil {
			return nil, err
		}
		return v, nil
	}

	in := domain.Intent{
		ID:               uuid.NewString(),
		Type:             spec.Type,
		Summary:          spec.Summary,
		Payload:          spec.Payload,
		Inputs:           spec.Inputs,
		UnresolvedInputs: unresolved,
		Origin:           spec.Origin,
		WaitSeconds:      int(s.intentWaitWindow().Seconds()),
		Policy:           policy,
		CreatedAt:        time.Now().UTC(),
	}

	// auto-allow never enters the store: nothing to approve, nothing to dedup
	// against. Emit one resolved event so the action still shows up in the log.
	// Unless its inputs cannot be resolved from defaults — then it must not
	// run, and it becomes an ordinary pending intent for the user to complete.
	if policy == domain.IntentPolicyAutoAllow && len(unresolved) == 0 {
		v, execErr := spec.Exec(ctx, defaults)
		res := intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeApproved, Result: v, Inputs: defaults}
		if execErr != nil {
			res = intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
		}
		if pubErr := s.publishIntentResolved(ctx, in, res); pubErr != nil {
			s.logger.Error("publish intent resolved for auto-allow", "intent_id", in.ID, "error", pubErr)
		}
		return typedResolution[R](res)
	}

	// The schema is part of the request, so it is part of "the same call
	// again"; submitted values are not — they arrive at resolution.
	var dedupInput any = spec.Payload
	if len(spec.Inputs) > 0 {
		dedupInput = map[string]any{"payload": spec.Payload, "inputs": spec.Inputs}
	}
	key, err := intentDedupKey(spec.SessionID, spec.Type, dedupInput)
	if err != nil {
		return zero, err
	}

	id, ch, replayed, disposition := s.intents.Begin(key, in, erased, defaults)
	switch disposition {
	case intentReplayed:
		// A retry inside the idempotency window: hand back the original
		// outcome rather than blocking on a dead intent or minting a duplicate.
		return typedResolution[R](replayed)
	case intentCreated:
		if err := s.publishIntentRequired(ctx, in); err != nil {
			// Roll back through Finish rather than a bare delete, so any
			// waiter that attached in the meantime also learns.
			s.intents.Finish(id, intentResult{
				Outcome: domain.IntentOutcomeError,
				Err:     err,
				Reason:  err.Error(),
			})
			return zero, fmt.Errorf("publish intent required: %w", err)
		}
		go s.runIntentPolicy(context.WithoutCancel(ctx), in, policy)
	case intentAttached:
		// An identical call is already in flight; just wait on it.
	}

	select {
	case <-ctx.Done():
		s.intents.Detach(id, ch)
		return zero, ctx.Err()
	case res := <-ch:
		return typedResolution[R](res)
	}
}

// typedResolution performs the single type assertion in the whole intent
// system. A mismatch is a programmer error (the spec's R disagrees with what
// Exec returned) and is surfaced loudly rather than silently zeroed.
func typedResolution[R any](res intentResult) (domain.IntentResolution[R], error) {
	out := domain.IntentResolution[R]{
		IntentID: res.IntentID,
		Outcome:  res.Outcome,
		Reason:   res.Reason,
	}
	if !res.Outcome.Approved() {
		return out, nil
	}
	out.Inputs = res.Inputs
	v, ok := res.Result.(R)
	if !ok {
		var want R
		return domain.IntentResolution[R]{}, fmt.Errorf(
			"intent %s resolved with result type %T, want %T", res.IntentID, res.Result, want)
	}
	out.Result = v
	return out, nil
}

// runIntentPolicy owns the wait window for one intent. It runs detached from
// the requesting agent's ctx on purpose (see awaitIntent).
func (s *Service) runIntentPolicy(ctx context.Context, intent domain.Intent, policy domain.IntentPolicy) {
	intentID := intent.ID
	if len(intent.UnresolvedInputs) > 0 && policy != domain.IntentPolicyWaitThenDeny {
		// Approving automatically would run with missing or invalid input, and
		// guessing a value is not ours to do. The intent stays pending for the
		// user; the desktop shows why from intent.unresolved_inputs, and an
		// agent retry attaches to it through dedup.
		s.logger.Warn("intent needs user input; automatic approval withheld",
			"intent_id", intentID, "intent_type", intent.Type, "policy", policy,
			"unresolved_inputs", intent.UnresolvedInputs)
		return
	}
	window := s.intentWaitWindow()
	if window <= 0 {
		// A non-positive window means "wait indefinitely" — the intent stays
		// pending until the user answers or the session tears down. Config
		// normalization coerces this back to the default, so only a
		// hand-built config reaches here.
		return
	}

	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}

	pending, won := s.intents.Claim(intentID)
	if !won {
		// The desktop claimed it first and owns Finish plus the resolved event.
		// This Claim CAS is the race resolver for the whole system.
		return
	}

	res := s.resolveByPolicy(ctx, pending, policy)
	s.intents.Finish(intentID, res)
	if err := s.publishIntentResolved(ctx, pending.intent, res); err != nil {
		s.logger.Error("publish intent resolved after policy fire",
			"intent_id", intentID, "policy", policy, "error", err)
	}
}

// resolveByPolicy applies the tool's expiry behavior. "No desktop connected" is
// not a special case: it is indistinguishable from "the user didn't click", and
// both land here.
func (s *Service) resolveByPolicy(ctx context.Context, pending *pendingIntent, policy domain.IntentPolicy) intentResult {
	switch policy {
	case domain.IntentPolicyWaitThenAllow:
		if len(pending.intent.UnresolvedInputs) > 0 {
			// runIntentPolicy never gets here with unresolved inputs; this is
			// the last line of defense against running with them.
			return intentResult{Outcome: domain.IntentOutcomeError, Reason: "intent inputs are unresolved; automatic approval refused"}
		}
		v, err := pending.exec(ctx, pending.defaults)
		if err != nil {
			return intentResult{Outcome: domain.IntentOutcomeError, Err: err, Reason: err.Error()}
		}
		return intentResult{Outcome: domain.IntentOutcomeAutoApproved, Result: v, Inputs: pending.defaults}
	case domain.IntentPolicyWaitThenDeny:
		return intentResult{
			Outcome: domain.IntentOutcomeAutoDenied,
			Reason:  fmt.Sprintf("no user response within %s", s.intentWaitWindow()),
		}
	default:
		return intentResult{
			Outcome: domain.IntentOutcomeError,
			Reason:  fmt.Sprintf("intent policy %q has no expiry behavior", policy),
		}
	}
}

// ApproveIntent resolves an intent as approved and performs its side effect
// with the user's validated inputs. The exec runs outside the store mutex — a
// daemon lock is never held across the agent's block or across I/O.
//
// Inputs are validated BEFORE the claim: invalid input returns a
// ValidationError and leaves the intent pending and unclaimed, so the user can
// correct it and approve again (and the expiry policy still applies). The
// schema is immutable, so validating against a pre-claim read is sound; if the
// policy or another client claims in between, the claim below loses and this
// reports not-found like any other already-resolved intent.
func (s *Service) ApproveIntent(ctx context.Context, sessionID, intentID string, submitted domain.IntentInputValues) (domain.Intent, error) {
	intent, ok := s.intents.GetForSession(sessionID, intentID)
	if !ok {
		return domain.Intent{}, &domain.NotFoundError{Resource: "intent", ID: intentID}
	}
	inputs, issues := resolveIntentInputs(intent.Inputs, submitted)
	if len(issues) > 0 {
		return domain.Intent{}, intentInputsError(issues)
	}

	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return domain.Intent{}, &domain.NotFoundError{Resource: "intent", ID: intentID}
	}

	v, execErr := pending.exec(ctx, inputs)
	res := intentResult{Outcome: domain.IntentOutcomeApproved, Result: v, Inputs: inputs}
	if execErr != nil {
		res = intentResult{Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
	}
	s.intents.Finish(intentID, res)

	if err := s.publishIntentResolved(ctx, pending.intent, res); err != nil {
		s.logger.Error("publish intent resolved after approve",
			"intent_id", intentID, "error", err)
	}
	if execErr != nil {
		// The user clicked approve and it failed. Surface it to them too —
		// telling the agent while silently returning 200 to the desktop would
		// hide a real failure.
		return pending.intent, fmt.Errorf("approve intent %s: %w", intentID, execErr)
	}
	return pending.intent, nil
}

// DenyIntent resolves an intent as denied. The side effect never runs.
func (s *Service) DenyIntent(ctx context.Context, sessionID, intentID, reason string) error {
	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return &domain.NotFoundError{Resource: "intent", ID: intentID}
	}

	res := intentResult{Outcome: domain.IntentOutcomeDeniedByUser, Reason: reason}
	s.intents.Finish(intentID, res)
	return s.publishIntentResolved(ctx, pending.intent, res)
}

// failPendingIntents resolves every intent still open on a session that is
// ending. Without it an intent would outlive its session and fire its side
// effect into a dead session when the policy expires — a gap that could not
// exist when there was one approval per session and it *was* the conclusion.
func (s *Service) failPendingIntents(ctx context.Context, sessionID, reason string) {
	for _, id := range s.intents.PendingForSession(sessionID) {
		pending, ok := s.intents.Claim(id)
		if !ok {
			continue
		}
		res := intentResult{Outcome: domain.IntentOutcomeError, Reason: reason}
		s.intents.Finish(id, res)
		if err := s.publishIntentResolved(ctx, pending.intent, res); err != nil {
			s.logger.Error("publish intent resolved on session teardown",
				"session_id", sessionID, "intent_id", id, "error", err)
		}
	}
}

// SetArchitectPublisher wires the architect-scoped event hub. It is a setter
// rather than a New() parameter because the hub is constructed after the
// service during startup.
func (s *Service) SetArchitectPublisher(publish func(string, domain.ArchitectEvent)) {
	s.publishArchitect = publish
}

// emitArchitectEvent invalidates the desktop's workspace view. It lives on the
// service (not the API handler) because a generic approve handler cannot know
// that a given intent touched a ticket — only the tool's Exec knows that.
// emitArchitectEvent stamps and fans out one architect event. ticketID is empty
// for reasons that are not about a ticket; sessionID is empty for reasons that
// are not about a session.
func (s *Service) emitArchitectEvent(architectKey string, reason domain.ArchitectEventReason, ticketID, sessionID string) {
	if s.publishArchitect == nil || architectKey == "" {
		return
	}
	s.publishArchitect(architectKey, domain.ArchitectEvent{
		Type:         domain.ArchitectEventType,
		ArchitectKey: architectKey,
		Reason:       reason,
		TicketID:     ticketID,
		SessionID:    sessionID,
		At:           time.Now().UTC(),
	})
}

// RequestCreateWorkTicket routes an agent's createWorkTicket through approval.
// The ticket is written only if the intent resolves approved, and the write
// happens here in the daemon — the agent has no unapproved path to it.
func (s *Service) RequestCreateWorkTicket(
	ctx context.Context, sessionID string, params domain.CreateTicketParams,
) (domain.IntentResolution[domain.Ticket], error) {
	var zero domain.IntentResolution[domain.Ticket]

	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return zero, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}

	// The architect key comes from the stored session, never from the request:
	// an agent cannot create tickets in an architect it wasn't spawned for.
	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return zero, err
	}

	// Validate before the popup, mirroring RequestConclusion: bad input goes
	// straight back to the agent and the user is never shown a doomed dialog.
	params.Title = strings.TrimSpace(params.Title)
	if params.Title == "" {
		return zero, &domain.ValidationError{Field: "title", Message: "is required"}
	}
	params.Repo = strings.TrimSpace(params.Repo)
	if params.Repo == "" {
		return zero, &domain.ValidationError{Field: "repo", Message: "is required"}
	}
	if _, ok := architect.Repos[params.Repo]; !ok {
		return zero, &domain.ValidationError{
			Field:   "repo",
			Message: fmt.Sprintf("repo key %q is not configured for architect %s", params.Repo, session.ArchitectKey),
		}
	}
	seen := map[string]struct{}{params.Repo: {}}
	for i, raw := range params.AdditionalRepos {
		key := strings.TrimSpace(raw)
		if key == "" {
			return zero, &domain.ValidationError{Field: "additional_repos", Message: "repo keys cannot be blank"}
		}
		if _, exists := seen[key]; exists {
			return zero, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is duplicated or overlaps primary repo", key)}
		}
		if _, ok := architect.Repos[key]; !ok {
			return zero, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is not configured for architect %s", key, session.ArchitectKey)}
		}
		seen[key] = struct{}{}
		params.AdditionalRepos[i] = key
	}
	sort.Strings(params.AdditionalRepos)

	payload := map[string]any{
		"title":            params.Title,
		"repo":             params.Repo,
		"additional_repos": params.AdditionalRepos,
		"body":             params.Body,
		"references":       params.References,
	}

	return awaitIntent(ctx, s, intentSpec[domain.Ticket]{
		SessionID: sessionID,
		Type:      domain.IntentTypeCreateWorkTicket,
		Summary:   params.Title,
		Payload:   payload,
		Origin:    intentOrigin(session),
		Exec: func(ctx context.Context, _ domain.IntentInputValues) (domain.Ticket, error) {
			p := params
			// Stamped here, NOT before the dedup hash. A per-call timestamp in
			// the hashed payload would make every retry hash differently and
			// silently disable dedup — the one bug that would quietly reinstate
			// duplicate tickets.
			p.Now = time.Now().UTC()

			ticket, err := s.tickets.CreateTicket(ctx, architect.Path, p)
			if err != nil {
				return domain.Ticket{}, err
			}
			s.emitArchitectEvent(session.ArchitectKey, domain.ArchitectEventTicketCreated, ticket.ID, "")
			return ticket, nil
		},
	})
}
