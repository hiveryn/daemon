package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

// intentSpec describes one approvable tool call. Exec is the side effect, and
// it runs only after the intent is approved (or auto-approved) — never before.
// Keeping the write inside Exec is what makes approval unbypassable: the agent
// has no path to the write except through an intent the daemon resolved.
type intentSpec[R any] struct {
	SessionID string
	Type      domain.IntentType
	Summary   string
	Payload   map[string]any // desktop-renderable; also the dedup input
	Origin    domain.IntentOrigin
	Exec      func(context.Context) (R, error)
}

// eraseExec type-erases a spec's Exec once: the store holds heterogeneous
// intents, so it cannot be generic, and this closure is what lets it stay
// type-agnostic.
func eraseExec[R any](exec func(context.Context) (R, error)) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		v, err := exec(ctx)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
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
		v, execErr := spec.Exec(ctx)
		res := intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeApproved, Result: v}
		if execErr != nil {
			res = intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
		}
		if pubErr := s.publishIntentResolved(ctx, in, res); pubErr != nil {
			s.logger.Error("publish intent resolved for auto-allow", "intent_id", in.ID, "error", pubErr)
		}
		return typedResolution[R](res)
	}

	key, err := intentDedupKey(spec.SessionID, spec.Type, spec.Payload)
	if err != nil {
		return zero, err
	}

	id, ch, replayed, disposition := s.intents.Begin(key, in, erased)
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
		v, err := pending.exec(ctx)
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

// ApproveIntent resolves an intent as approved and performs its side effect.
// The exec runs outside the store mutex — a daemon lock is never held across
// the agent's block or across I/O.
func (s *Service) ApproveIntent(ctx context.Context, sessionID, intentID string) (domain.Intent, error) {
	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return domain.Intent{}, s.unanswerableIntent(sessionID, intentID)
	}

	// From here on the operation is the daemon's, not the request's: the
	// user's approval was accepted, so the desktop giving up on the response
	// (its own timeout, a reload) must neither abort the side effect halfway
	// — killing an SSH teardown mid-command — nor suppress the outcome.
	s.publishIntentResolving(ctx, pending.intent, "user")
	execCtx, cancel := intentExecContext(ctx)
	v, execErr := pending.exec(execCtx)
	cancel()
	res := intentResult{Outcome: domain.IntentOutcomeApproved, Result: v}
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
// Claimed intents are skipped: they are already resolving.
func (s *Service) failPendingIntents(ctx context.Context, sessionID, reason string) {
	ctx, cancel := intentFinalizeContext(ctx)
	defer cancel()
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
