package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

// Deferred intents (policy manual). The request returns at once with a stable
// intent id and status pending_approval; only the user resolves it, and there
// is no timer — defaults only prefill the form. The pending intent (with its
// captured Exec) lives in the in-memory intent store exactly like a blocking
// one, which keeps session scoping, retry dedup and the single-winner claim.
// What the store cannot give is an outcome addressable by id after the intent
// is gone, so each deferred intent also has a durable DeferredIntent record:
//
//	pending_approval ──approve──▶ running ──▶ completed | failed
//	        │                        (approved_at set)
//	        ├──deny──▶ denied
//	        └──session ends / daemon restarts──▶ failed (approved_at unset)
//
// A restart cannot resume a captured Exec, so startup fails every record still
// open instead of leaving it falsely pending (reconcileDeferredIntents). The id
// is the one caller-visible identity from request to outcome; a future Action
// run keeps it rather than minting a run id of its own.

// deferredIntentRetention bounds how long a resolved record stays
// retrievable. Open records are never pruned; they cannot outlive a restart.
const deferredIntentRetention = 30 * 24 * time.Hour

const (
	deferredFailedOnRestartPending = "daemon restarted before this request was approved; it never ran"
	deferredFailedOnRestartRunning = "daemon restarted while this request was running; it may or may not have taken effect"
	deferredFailedOnSessionEnd     = "session ended before this request was approved; it never ran"
)

// SetDeferredIntentRepository wires durable storage for deferred intents. It is
// a setter, like SetEventArchiver, so the many test constructions of Service
// need no change; a deferred request without it fails loudly.
func (s *Service) SetDeferredIntentRepository(repo domain.DeferredIntentRepository) {
	s.deferred = repo
}

func (s *Service) deferredRepo() (domain.DeferredIntentRepository, error) {
	if s.deferred == nil {
		return nil, errors.New("deferred intent repository not configured")
	}
	return s.deferred, nil
}

// submitDeferredIntent raises a deferred intent and returns its record without
// waiting for the user. A retry of the same call (same session, tool, payload
// and schema) inside the idempotency window returns the original record —
// pending or resolved — instead of minting a second request.
func submitDeferredIntent[R any](ctx context.Context, s *Service, spec intentSpec[R]) (domain.DeferredIntent, error) {
	var zero domain.DeferredIntent

	policy, err := policyFor(spec.Type)
	if err != nil {
		return zero, err
	}
	if policy != domain.IntentPolicyManual {
		return zero, fmt.Errorf("%s: policy %s is not deferred; request it with awaitIntent", spec.Type, policy)
	}
	if err := checkIntentInputSchema(spec.Inputs); err != nil {
		return zero, fmt.Errorf("%s: %w", spec.Type, err)
	}
	repo, err := s.deferredRepo()
	if err != nil {
		return zero, err
	}
	key, err := intentDedupKey(spec.SessionID, spec.Type, intentDedupInput(spec.Payload, spec.Inputs))
	if err != nil {
		return zero, err
	}

	in := domain.Intent{
		ID:        uuid.NewString(),
		Type:      spec.Type,
		Summary:   spec.Summary,
		Payload:   spec.Payload,
		Inputs:    spec.Inputs,
		Origin:    spec.Origin,
		Policy:    policy,
		CreatedAt: time.Now().UTC(),
		// WaitSeconds stays 0: there is no countdown.
	}

	id, ready, disposition := s.intents.BeginDeferred(key, in, eraseExec(spec.Exec), spec.Hooks)
	switch disposition {
	case intentReplayed:
		return repo.GetDeferredIntent(ctx, id)
	case intentAttached:
		select {
		case <-ready:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		return repo.GetDeferredIntent(ctx, id)
	}

	if n, err := repo.PruneDeferredIntents(ctx, in.CreatedAt.Add(-deferredIntentRetention)); err != nil {
		s.logger.Error("prune deferred intents", "error", err)
	} else if n > 0 {
		s.logger.Info("pruned deferred intents", "count", n)
	}

	record := domain.DeferredIntent{
		ID:        in.ID,
		Type:      in.Type,
		Summary:   in.Summary,
		Payload:   in.Payload,
		Origin:    in.Origin,
		Status:    domain.DeferredIntentPendingApproval,
		CreatedAt: in.CreatedAt,
	}
	if err := repo.CreateDeferredIntent(ctx, record); err != nil {
		// No record exists, so there is nothing for a retry to replay.
		s.intents.Discard(id)
		return zero, fmt.Errorf("persist deferred intent: %w", err)
	}
	if spec.Hooks.Created != nil {
		if err := spec.Hooks.Created(ctx, in); err != nil {
			// Withdrawn before anyone saw it. Discard rather than Finish, so a
			// retry mints a fresh request instead of replaying this failure.
			if failErr := s.failDeferredRecord(ctx, in, domain.DeferredIntentPendingApproval, "request could not be recorded: "+err.Error()); failErr != nil {
				s.logger.Error("fail withdrawn deferred intent", "intent_id", id, "error", failErr)
			}
			s.intents.Discard(id)
			return zero, err
		}
	}
	if err := s.publishIntentRequired(ctx, in); err != nil {
		// The user can never see it, so it must not read as pending.
		reason := "approval request could not be shown: " + err.Error()
		if failErr := s.failDeferredRecord(ctx, in, domain.DeferredIntentPendingApproval, reason); failErr != nil {
			s.logger.Error("fail unpublished deferred intent", "intent_id", id, "error", failErr)
		}
		spec.Hooks.abandoned(ctx, in, reason)
		s.intents.Finish(id, intentResult{Outcome: domain.IntentOutcomeError, Err: err, Reason: err.Error(), Status: domain.DeferredIntentFailed})
		return zero, fmt.Errorf("publish intent required: %w", err)
	}
	if !s.intents.MarkReady(id) {
		// Resolved before its records existed: the session ended in between,
		// and its teardown could not fail a record that was not written yet.
		// Either record may already have been failed by that teardown.
		spec.Hooks.abandoned(ctx, in, deferredFailedOnSessionEnd)
		if err := s.failDeferredRecord(ctx, in, domain.DeferredIntentPendingApproval, deferredFailedOnSessionEnd); err != nil && !errors.As(err, new(*domain.ConflictError)) {
			return zero, fmt.Errorf("fail deferred intent resolved during submission: %w", err)
		}
		return repo.GetDeferredIntent(ctx, id)
	}
	return record, nil
}

// approveDeferred runs a claimed deferred intent. Approval is recorded
// (running, approved_at) before the operation starts, and its outcome
// afterwards, so a lookup never conflates "approved" with "succeeded". The
// operation runs detached from the approving request: the desktop walking away
// mid-run must not abort it and leave the record running.
func (s *Service) approveDeferred(ctx context.Context, pending *pendingIntent, inputs domain.IntentInputValues) error {
	repo, err := s.deferredRepo()
	if err != nil {
		s.intents.Release(pending.intent.ID)
		return err
	}
	intentID := pending.intent.ID
	approvedAt := time.Now().UTC()
	running := deferredRecordOf(pending.intent)
	running.Status = domain.DeferredIntentRunning
	running.Inputs = inputs
	running.ApprovedAt = &approvedAt
	if err := repo.TransitionDeferredIntent(ctx, domain.DeferredIntentPendingApproval, running); err != nil {
		// Nothing ran; give the claim back so the user can try again.
		s.intents.Release(intentID)
		return fmt.Errorf("record approval of intent %s: %w", intentID, err)
	}

	execCtx := context.WithoutCancel(ctx)
	v, execErr := pending.exec(execCtx, inputs)

	endedAt := time.Now().UTC()
	final := running
	final.EndedAt = &endedAt
	res := intentResult{Outcome: domain.IntentOutcomeApproved, Result: v, Inputs: inputs, Status: domain.DeferredIntentCompleted}
	if execErr != nil {
		final.Status = domain.DeferredIntentFailed
		final.Error = execErr.Error()
		res = intentResult{Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error(), Status: domain.DeferredIntentFailed}
	} else {
		final.Status = domain.DeferredIntentCompleted
		final.Result = v
	}
	if err := repo.TransitionDeferredIntent(execCtx, domain.DeferredIntentRunning, final); err != nil {
		// The record stays running until the next startup fails it as
		// interrupted; say so loudly rather than pretend it was recorded.
		s.logger.Error("record deferred intent outcome", "intent_id", intentID, "status", final.Status, "error", err)
	}
	s.intents.Finish(intentID, res)
	if err := s.publishIntentResolved(execCtx, pending.intent, res); err != nil {
		s.logger.Error("publish intent resolved after deferred approve", "intent_id", intentID, "error", err)
	}
	if execErr != nil {
		return fmt.Errorf("approve intent %s: %w", intentID, execErr)
	}
	return nil
}

// denyDeferred records a denial. It needs no inputs and the operation never
// runs. If the denial cannot be recorded the claim is released, so the record
// never says pending while the intent is gone.
func (s *Service) denyDeferred(ctx context.Context, pending *pendingIntent, reason string) error {
	repo, err := s.deferredRepo()
	if err != nil {
		s.intents.Release(pending.intent.ID)
		return err
	}
	endedAt := time.Now().UTC()
	denied := deferredRecordOf(pending.intent)
	denied.Status = domain.DeferredIntentDenied
	denied.Reason = reason
	denied.EndedAt = &endedAt
	if err := repo.TransitionDeferredIntent(ctx, domain.DeferredIntentPendingApproval, denied); err != nil {
		s.intents.Release(pending.intent.ID)
		return fmt.Errorf("record denial of intent %s: %w", pending.intent.ID, err)
	}
	if pending.hooks.Denied != nil {
		pending.hooks.Denied(ctx, pending.intent, reason)
	}
	res := intentResult{Outcome: domain.IntentOutcomeDeniedByUser, Reason: reason, Status: domain.DeferredIntentDenied}
	s.intents.Finish(pending.intent.ID, res)
	return s.publishIntentResolved(ctx, pending.intent, res)
}

func (h deferredHooks) abandoned(ctx context.Context, in domain.Intent, reason string) {
	if h.Abandoned != nil {
		h.Abandoned(ctx, in, reason)
	}
}

// failDeferredRecord moves a record from `from` to failed with reason.
func (s *Service) failDeferredRecord(ctx context.Context, in domain.Intent, from domain.DeferredIntentStatus, reason string) error {
	repo, err := s.deferredRepo()
	if err != nil {
		return err
	}
	endedAt := time.Now().UTC()
	failed := deferredRecordOf(in)
	failed.Status = domain.DeferredIntentFailed
	failed.Error = reason
	failed.EndedAt = &endedAt
	return repo.TransitionDeferredIntent(ctx, from, failed)
}

func deferredRecordOf(in domain.Intent) domain.DeferredIntent {
	return domain.DeferredIntent{
		ID:        in.ID,
		Type:      in.Type,
		Summary:   in.Summary,
		Payload:   in.Payload,
		Origin:    in.Origin,
		CreatedAt: in.CreatedAt,
	}
}

// GetDeferredIntent returns a deferred intent's pending or resolved outcome by
// its id, scoped to the session that raised it. Another session's id reads as
// not found: the caller has no business learning it exists.
func (s *Service) GetDeferredIntent(ctx context.Context, sessionID, intentID string) (domain.DeferredIntent, error) {
	repo, err := s.deferredRepo()
	if err != nil {
		return domain.DeferredIntent{}, err
	}
	record, err := repo.GetDeferredIntent(ctx, intentID)
	if err != nil {
		return domain.DeferredIntent{}, err
	}
	if record.Origin.SessionID != sessionID {
		return domain.DeferredIntent{}, &domain.NotFoundError{Resource: "intent", ID: intentID}
	}
	return record, nil
}

// reconcileDeferredIntents runs at startup, before any request can raise a new
// intent. The in-memory store is empty, so every record still open lost its
// operation with the previous process: fail it with the reason, then prune
// resolved records past retention.
func (s *Service) reconcileDeferredIntents(ctx context.Context) error {
	if s.deferred == nil {
		return nil
	}
	now := time.Now().UTC()
	n, err := s.deferred.FailOpenDeferredIntents(ctx, deferredFailedOnRestartPending, deferredFailedOnRestartRunning, now)
	if err != nil {
		return err
	}
	if n > 0 {
		s.logger.Warn("failed deferred intents interrupted by daemon restart", "count", n)
	}
	if _, err := s.deferred.PruneDeferredIntents(ctx, now.Add(-deferredIntentRetention)); err != nil {
		return err
	}
	return nil
}
