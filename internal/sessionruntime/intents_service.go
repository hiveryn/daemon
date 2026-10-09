package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// intentSpec describes one approvable tool call. Exec is the side effect, and
// it runs only after the intent is approved (or auto-approved) — never before.
// Keeping the write inside Exec is what makes approval unbypassable: the agent
// has no path to the write except through an intent the daemon resolved.
//
// Inputs is the optional approval-input schema. A spec with inputs is always
// deferred (submitDeferredIntent, policy manual): the user completes the form
// and only the user approves it, so Exec receives exactly the values the user
// submitted, validated. Blocking tools (awaitIntent) take no inputs.
type intentSpec[R any] struct {
	SessionID string
	Type      domain.IntentType
	Summary   string
	Payload   map[string]any // desktop-renderable; also the dedup input
	Inputs    []domain.IntentInputField
	Origin    domain.IntentOrigin
	Exec      func(context.Context, domain.IntentInputValues) (R, error)
	// Hooks keep a tool's own record in step; see intentHooks.
	Hooks intentHooks
}

// intentHooks keep a tool's own durable record in step with an intent whose
// outcome that record is authoritative for (executeAction: the Action
// execution, whose id is the intent id). Every hook is optional, and each
// runs at most once per intent. They serve deferred and blocking intents
// alike; an approval's outcome reaches the record through the tool's Exec.
type intentHooks struct {
	// Created runs before the request is shown (for a deferred intent, once
	// its generic record exists). An error withdraws the request: nothing is
	// shown, and a retry is free to try again.
	Created func(ctx context.Context, in domain.Intent) error
	// Denied runs after the user's denial is recorded.
	Denied func(ctx context.Context, in domain.Intent, reason string)
	// Abandoned runs when the request ends without the user resolving it: its
	// session ended first, or it could not be shown. It never ran.
	Abandoned func(ctx context.Context, in domain.Intent, reason string)
}

// eraseExec type-erases a spec's Exec once: the store holds heterogeneous
// intents, so it cannot be generic, and this closure is what lets it stay
// type-agnostic.
func eraseExec[R any](exec func(context.Context, domain.IntentInputValues) (R, error)) func(context.Context, domain.IntentInputValues) (any, error) {
	return func(ctx context.Context, inputs domain.IntentInputValues) (any, error) {
		v, err := exec(ctx, inputs)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
}

// intentDedupInput is what "the same call again" hashes. The schema is part of
// the request; submitted values are not — they arrive at resolution.
func intentDedupInput(payload map[string]any, inputs []domain.IntentInputField) any {
	if len(inputs) == 0 {
		return payload
	}
	return map[string]any{"payload": payload, "inputs": inputs}
}

// awaitIntent is the generic blocking wait shared by every blocking tool.
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
	if policy == domain.IntentPolicyManual {
		return zero, fmt.Errorf("%s: policy %s is deferred; request it with submitDeferredIntent", spec.Type, policy)
	}
	if len(spec.Inputs) > 0 {
		// Blocking the agent on a form, or approving one on a timer, is exactly
		// what deferred approval replaced.
		return zero, fmt.Errorf("%s: an intent with approval inputs must be deferred (policy %s)", spec.Type, domain.IntentPolicyManual)
	}

	erased := eraseExec(spec.Exec)
	in := domain.Intent{
		ID:          uuid.NewString(),
		Type:        spec.Type,
		Summary:     spec.Summary,
		Payload:     spec.Payload,
		Origin:      spec.Origin,
		WaitSeconds: int(s.intentWaitWindow().Seconds()),
		Policy:      policy,
		CreatedAt:   time.Now().UTC(),
	}

	// auto-allow never enters the store: nothing to approve, nothing to dedup
	// against. Emit one resolved event so the action still shows up in the log.
	if policy == domain.IntentPolicyAutoAllow {
		v, execErr := spec.Exec(ctx, nil)
		res := intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeApproved, Result: v}
		if execErr != nil {
			res = intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
		}
		if pubErr := s.publishIntentResolved(ctx, in, res); pubErr != nil {
			s.logger.Error("publish intent resolved for auto-allow", "intent_id", in.ID, "error", pubErr)
		}
		return typedResolution[R](res)
	}

	key, err := intentDedupKey(spec.SessionID, spec.Type, intentDedupInput(spec.Payload, spec.Inputs))
	if err != nil {
		return zero, err
	}

	id, ch, replayed, disposition := s.intents.BeginWithHooks(key, in, erased, spec.Hooks)
	switch disposition {
	case intentReplayed:
		// A retry inside the idempotency window: hand back the original
		// outcome rather than blocking on a dead intent or minting a duplicate.
		return typedResolution[R](replayed)
	case intentCreated:
		if spec.Hooks.Created != nil {
			if err := spec.Hooks.Created(ctx, in); err != nil {
				// Withdrawn before anyone saw it: no replay, so a retry mints
				// a fresh request instead of replaying this failure.
				s.intents.Abort(id, intentResult{Outcome: domain.IntentOutcomeError, Err: err, Reason: err.Error()})
				return zero, err
			}
			if _, ok := s.intents.Get(id); !ok {
				// The session ended while the tool's record was being
				// written, and its teardown ran before the record existed.
				// Bring the record in line; the teardown's resolution is
				// already on ch, and nothing is shown.
				spec.Hooks.abandoned(ctx, in, intentFailedOnSessionEnd)
				break
			}
		}
		if err := s.publishIntentRequired(ctx, in); err != nil {
			spec.Hooks.abandoned(ctx, in, "approval request could not be shown: "+err.Error())
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

	if policy == domain.IntentPolicyWaitThenAllow {
		s.publishIntentResolving(ctx, pending.intent, "policy")
	}
	execCtx, cancel := intentExecContext(ctx)
	res := s.resolveByPolicy(execCtx, pending, policy)
	cancel()
	s.finishIntent(ctx, pending.intent, res, "policy fire")
}

// resolveByPolicy applies the tool's expiry behavior. "No desktop connected" is
// not a special case: it is indistinguishable from "the user didn't click", and
// both land here.
func (s *Service) resolveByPolicy(ctx context.Context, pending *pendingIntent, policy domain.IntentPolicy) intentResult {
	switch policy {
	case domain.IntentPolicyWaitThenAllow:
		if len(pending.intent.Inputs) > 0 {
			// awaitIntent refuses such a spec; this is the last line of defense
			// against approving a form nobody filled in.
			return intentResult{Outcome: domain.IntentOutcomeError, Reason: "an intent with approval inputs is never approved automatically"}
		}
		v, err := pending.exec(ctx, nil)
		if err != nil {
			return intentResult{Outcome: domain.IntentOutcomeError, Err: err, Reason: err.Error()}
		}
		return intentResult{Outcome: domain.IntentOutcomeAutoApproved, Result: v}
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
// correct it and approve again. The schema is immutable, so validating against
// a pre-claim read is sound; if the policy or another client claims in
// between, the claim below loses and this reports the intent as resolving.
func (s *Service) ApproveIntent(ctx context.Context, sessionID, intentID string, submitted domain.IntentInputValues) (domain.Intent, error) {
	intent, ok := s.intents.GetForSession(sessionID, intentID)
	if !ok {
		return domain.Intent{}, s.unanswerableIntent(sessionID, intentID)
	}
	inputs, issues := resolveIntentInputs(intent.Inputs, submitted)
	if len(issues) > 0 {
		return domain.Intent{}, intentInputsError(issues)
	}

	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return domain.Intent{}, s.unanswerableIntent(sessionID, intentID)
	}
	if pending.intent.Policy == domain.IntentPolicyManual {
		return pending.intent, s.approveDeferred(ctx, pending, inputs)
	}

	// From here on the operation is the daemon's, not the request's: the
	// user's approval was accepted, so the desktop giving up on the response
	// (its own timeout, a reload) must neither abort the side effect halfway
	// — killing an SSH teardown mid-command — nor suppress the outcome.
	s.publishIntentResolving(ctx, pending.intent, "user")
	execCtx, cancel := intentExecContext(ctx)
	v, execErr := pending.exec(execCtx, inputs)
	cancel()
	res := intentResult{Outcome: domain.IntentOutcomeApproved, Result: v, Inputs: inputs}
	if execErr != nil {
		res = intentResult{Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
	}
	s.finishIntent(ctx, pending.intent, res, "approve")
	if execErr != nil {
		// The user clicked approve and it failed. Surface it to them too —
		// telling the agent while silently returning 200 to the desktop would
		// hide a real failure.
		return pending.intent, fmt.Errorf("approve intent %s: %w", intentID, execErr)
	}
	return pending.intent, nil
}

// DenyIntent resolves an intent as denied. The side effect never runs.
//
// Once claimed, the denial is recorded on a daemon-owned context: a desktop that
// stops waiting must not leave the intent denied in memory but unrecorded.
func (s *Service) DenyIntent(ctx context.Context, sessionID, intentID, reason string) error {
	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return s.unanswerableIntent(sessionID, intentID)
	}
	ctx, cancel := intentFinalizeContext(ctx)
	defer cancel()
	if pending.intent.Policy == domain.IntentPolicyManual {
		return s.denyDeferred(ctx, pending, reason)
	}

	if pending.hooks.Denied != nil {
		pending.hooks.Denied(ctx, pending.intent, reason)
	}
	res := intentResult{Outcome: domain.IntentOutcomeDeniedByUser, Reason: reason}
	s.intents.Finish(intentID, res)
	return s.publishIntentResolved(ctx, pending.intent, res)
}

// unanswerableIntent is the error an answering client sees for an intent it
// could not claim. One still resolving is a conflict, not "not found": its
// outcome is on the way, and a client that read 404 as "already resolved"
// would drop the card of an operation that is still running — or may yet fail.
func (s *Service) unanswerableIntent(sessionID, intentID string) error {
	if s.intents.ResolvingForSession(sessionID, intentID) {
		return &domain.ConflictError{Resource: "intent", Field: "status", Message: fmt.Sprintf("intent %s is already resolving; its outcome is published when it finishes", intentID)}
	}
	return &domain.NotFoundError{Resource: "intent", ID: intentID}
}

// intentExecTimeout bounds an approved intent's side effect, which runs on a
// daemon-owned context once approval is accepted. It sits above the two-minute
// worker launch bound (launchTimeout), so an approved spawnTicketWorker reports
// the launch's own error, and leaves room for a remote conclusion: commit
// resolution and confirmed termination are several SSH round trips, each of
// which may meet a slow link. Exceeding it fails the operation honestly — a
// remote teardown cut off here is reported as not confirmed, never as done.
const intentExecTimeout = 3 * time.Minute

// intentFinalizeTimeout bounds recording an intent's outcome (its tool record,
// the in-memory resolution and the durable resolved event), on a context the
// approving request cannot cancel: losing the response must never suppress the
// outcome. These are local SQLite writes; the bound only stops a wedged store
// from holding a goroutine forever.
const intentFinalizeTimeout = 30 * time.Second

func intentExecContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), intentExecTimeout)
}

func intentFinalizeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), intentFinalizeTimeout)
}

// finishIntent resolves a claimed blocking intent: waiters and the replay
// cache learn res, then the durable resolved event is appended, on a
// finalization context of its own.
func (s *Service) finishIntent(ctx context.Context, in domain.Intent, res intentResult, via string) {
	ctx, cancel := intentFinalizeContext(ctx)
	defer cancel()
	s.intents.Finish(in.ID, res)
	err := s.publishIntentResolved(ctx, in, res)
	switch {
	case err == nil:
	case errors.As(err, new(*domain.NotFoundError)) && res.Outcome.Approved():
		// The approved operation ended and removed its own session (a
		// conclusion): its session_ended event, published before removal, is
		// the terminal record, and no log remains to append to.
		s.logger.Info("intent resolved after its session ended", "intent_id", in.ID, "via", via)
	default:
		s.logger.Error("publish intent resolved after "+via, "intent_id", in.ID, "outcome", res.Outcome, "error", err)
	}
}

// failPendingIntents resolves every intent still open on a session that is
// ending. Without it an intent would outlive its session and fire its side
// effect into a dead session when the policy expires — a gap that could not
// exist when there was one approval per session and it *was* the conclusion.
// A deferred intent's record is failed too, and a tool's own record through
// its Abandoned hook, so neither reads as pending.
// Claimed intents are skipped: they are already resolving, and a deferred
// one that is running finishes and records its own outcome.
func (s *Service) failPendingIntents(ctx context.Context, sessionID, reason string) {
	ctx, cancel := intentFinalizeContext(ctx)
	defer cancel()
	for _, id := range s.intents.PendingForSession(sessionID) {
		pending, ok := s.intents.Claim(id)
		if !ok {
			continue
		}
		res := intentResult{Outcome: domain.IntentOutcomeError, Reason: reason}
		if pending.intent.Policy == domain.IntentPolicyManual {
			res = intentResult{Outcome: domain.IntentOutcomeError, Reason: intentFailedOnSessionEnd, Status: domain.DeferredIntentFailed}
			if err := s.failDeferredRecord(ctx, pending.intent, domain.DeferredIntentPendingApproval, intentFailedOnSessionEnd); err != nil {
				s.logger.Error("fail deferred intent on session teardown",
					"session_id", sessionID, "intent_id", id, "error", err)
			}
		}
		pending.hooks.abandoned(ctx, pending.intent, intentFailedOnSessionEnd)
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
	cfg, err := config.WritableConfig(s.cfg, s.configSource)
	if err != nil {
		return zero, err
	}
	if _, err := cfg.ScopeMachine(architect, params.Repo, params.AdditionalRepos); err != nil {
		return zero, &domain.ValidationError{Field: "repos", Message: err.Error()}
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
			live, err := config.WritableConfig(s.cfg, s.configSource)
			if err != nil {
				return domain.Ticket{}, err
			}
			a, ok := live.Architects[session.ArchitectKey]
			if !ok {
				return domain.Ticket{}, fmt.Errorf("architect no longer configured")
			}
			if _, err := live.ScopeMachine(a, p.Repo, p.AdditionalRepos); err != nil {
				return domain.Ticket{}, &domain.ValidationError{Field: "repos", Message: err.Error()}
			}

			ticket, err := s.tickets.CreateTicket(ctx, a.Path, p)
			if err != nil {
				return domain.Ticket{}, err
			}
			s.emitArchitectEvent(session.ArchitectKey, domain.ArchitectEventTicketCreated, ticket.ID, "")
			return ticket, nil
		},
	})
}
