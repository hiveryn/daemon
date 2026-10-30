package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

// Cooperative companion shutdown.
//
// The architect requests it with its own request_id (requestCompanionShutdown):
// the request is recorded bound to the exact architect and companion sessions,
// the companion is marked shutting down (no new assignments, pending action
// requests withdrawn) and the instruction carrying the request_id is queued
// to it. The companion completes it (completeCompanionShutdown) with that id
// and its handoff; only a recorded request for this very companion session
// and its owning architect session is accepted. After acknowledging, the
// daemon terminates the companion — over SSH for a remote one — and tells the
// architect the request id, the handoff and the actual outcome. A termination
// it cannot confirm is reported as such and stays stop_failed: visible,
// blocking the architect's conclusion, never retried or forced on its own.

// shutdownInstructionBody is the instruction queued to the companion.
func shutdownInstructionBody(requestID string) string {
	return fmt.Sprintf("Shutdown requested by the architect. request_id: %s\n\nPreserve useful knowledge and unfinished work in your repository, then call completeCompanionShutdown with this request_id and your handoff.", requestID)
}

// RequestCompanionShutdown is the architect's requestCompanionShutdown.
func (s *Service) RequestCompanionShutdown(ctx context.Context, architectSessionID string, req domain.CompanionShutdownRequest) (domain.CompanionShutdownResult, error) {
	requestID, err := canonicalRequestID(req.RequestID)
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}
	architect, err := s.architectOf(ctx, architectSessionID)
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}
	c, err := s.namedCompanion(ctx, architectSessionID, req.Companion)
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}
	accepted := domain.CompanionShutdownResult{RequestID: requestID, Status: domain.CompanionAccepted}
	repo, err := s.messageRepo()
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}

	existing, err := s.companionRepo.GetShutdown(ctx, requestID)
	switch {
	case err == nil:
		if existing.ArchitectSessionID != architectSessionID || existing.Companion != c.Name || existing.CompanionSessionID != c.SessionID {
			return domain.CompanionShutdownResult{}, &domain.ConflictError{Resource: "companion_shutdown", Field: "request_id", Message: "request_id " + requestID + " was already used for a different shutdown target; generate a new UUID for this companion"}
		}
		// A repeat of the recorded request: nothing is queued again. A
		// commit that failed earlier is completed now.
		if m, getErr := repo.GetAgentMessageByDedupeKey(ctx, shutdownDedupeKey(requestID)); getErr == nil {
			if commitErr := s.CommitAgentMessage(ctx, m.ID); commitErr != nil {
				return domain.CompanionShutdownResult{}, fmt.Errorf("the shutdown request is recorded but its instruction could not be queued: %w", commitErr)
			}
		}
		return accepted, nil
	case !errors.As(err, new(*domain.NotFoundError)):
		return domain.CompanionShutdownResult{}, err
	}

	if c.Status != domain.CompanionRunning {
		if other, getErr := s.companionRepo.GetShutdownBySession(ctx, c.SessionID); getErr == nil && c.SessionID != "" {
			return domain.CompanionShutdownResult{}, &domain.ConflictError{Resource: "companion_shutdown", Field: "request_id", Message: fmt.Sprintf("companion %s is already shutting down under request_id %s (%s); repeat that request_id instead of issuing a new one", c.Name, other.RequestID, other.Status)}
		}
		return domain.CompanionShutdownResult{}, &domain.ConflictError{Resource: "companion", Field: "status", Message: fmt.Sprintf("companion %s is not running (%s)%s; there is nothing to shut down", c.Name, c.Status, errorSuffix(c.Error))}
	}

	// The instruction is begun before the request is recorded and made
	// deliverable after, so a crash in between is settled against the
	// request record (checkShutdownRequestedCommit).
	notice, err := s.BeginAgentMessage(ctx, AgentMessageParams{
		Kind:         domain.AgentMessageCompanionShutdownRequested,
		Recipient:    domain.AgentMessageRecipientSession + c.SessionID,
		ArchitectKey: architect.ArchitectKey,
		DedupeKey:    shutdownDedupeKey(requestID),
		Body:         shutdownInstructionBody(requestID),
		Payload: map[string]any{
			"request_id":           requestID,
			"companion":            c.Name,
			"architect_session_id": architectSessionID,
			"companion_session_id": c.SessionID,
		},
	})
	if err != nil {
		return domain.CompanionShutdownResult{}, fmt.Errorf("queue the shutdown instruction: %w; the shutdown was NOT requested", err)
	}
	now := time.Now().UTC()
	if err := s.companionRepo.CreateShutdown(ctx, domain.CompanionShutdown{
		RequestID:          requestID,
		ArchitectKey:       architect.ArchitectKey,
		ArchitectSessionID: architectSessionID,
		CompanionSessionID: c.SessionID,
		Companion:          c.Name,
		Status:             domain.ShutdownRequested,
		RequestedAt:        now,
	}); err != nil {
		s.markMessageFailed(&notice, "the shutdown request was not recorded: "+err.Error())
		if updateErr := repo.UpdateAgentMessage(context.WithoutCancel(ctx), notice); updateErr != nil {
			s.logger.Error("withdraw shutdown instruction", "message_id", notice.ID, "error", updateErr)
		}
		return domain.CompanionShutdownResult{}, fmt.Errorf("record the shutdown request: %w; the shutdown was NOT requested", err)
	}
	if err := s.CommitAgentMessage(ctx, notice.ID); err != nil {
		return domain.CompanionShutdownResult{}, fmt.Errorf("the shutdown request is recorded but its instruction could not be queued (repeat the call with the same request_id): %w", err)
	}
	s.closeSessionActions(ctx, c.SessionID, actionReasonShutdown)
	s.emitCompanionChanged(c.ArchitectKey, c.SessionID)
	s.logger.Info("companion shutdown requested", "architect_session_id", architectSessionID, "companion", c.Name, "session_id", c.SessionID, "request_id", requestID)
	return accepted, nil
}

// CompleteCompanionShutdown is the companion's completeCompanionShutdown.
func (s *Service) CompleteCompanionShutdown(ctx context.Context, companionSessionID string, req domain.CompleteCompanionShutdownRequest) (domain.CompanionShutdownResult, error) {
	requestID, err := canonicalRequestID(req.RequestID)
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}
	handoff, err := domain.NormalizeCompanionText("handoff", req.Handoff, domain.MaxCompanionHandoffLength)
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}
	session, c, err := s.companionOf(ctx, companionSessionID)
	if err != nil {
		return domain.CompanionShutdownResult{}, err
	}
	sd, err := s.companionRepo.GetShutdown(ctx, requestID)
	if err != nil && !errors.As(err, new(*domain.NotFoundError)) {
		return domain.CompanionShutdownResult{}, err
	}
	if err != nil || sd.CompanionSessionID != session.ID || sd.ArchitectSessionID != session.OwnerSessionID || c.ArchitectSessionID != session.OwnerSessionID {
		return domain.CompanionShutdownResult{}, &domain.NotFoundError{Resource: "shutdown request for this companion session (only the request_id from your architect's shutdown message is accepted)", ID: requestID}
	}
	accepted := domain.CompanionShutdownResult{RequestID: requestID, Status: domain.CompanionAccepted}
	if sd.Status != domain.ShutdownRequested {
		if sd.Handoff == handoff {
			return accepted, nil
		}
		return domain.CompanionShutdownResult{}, &domain.ConflictError{Resource: "companion_shutdown", Field: "handoff", Message: "this shutdown was already completed with a different handoff; the recorded handoff stands"}
	}
	now := time.Now().UTC()
	sd.Status, sd.Handoff, sd.CompletedAt = domain.ShutdownCompleting, handoff, &now
	if err := s.companionRepo.UpdateShutdown(context.WithoutCancel(ctx), sd); err != nil {
		return domain.CompanionShutdownResult{}, fmt.Errorf("record the shutdown completion: %w; nothing was stopped", err)
	}
	c.Status, c.Error = domain.CompanionStopping, ""
	_ = s.updateCompanion(ctx, &c)
	s.logger.Info("companion shutdown completed; stopping after the acknowledgement", "companion", c.Name, "session_id", c.SessionID, "request_id", requestID)
	hop := companionAckAPI
	if c.Machine != "" {
		hop = companionAckGateway
	}
	written, ackWait, grace := s.companionRT.awaitAck(c.SessionID, requestID, hop), companionAckWait, companionStopGrace
	s.companionRT.wg.Add(1)
	go func() {
		defer s.companionRT.wg.Done()
		timer := time.NewTimer(ackWait)
		defer timer.Stop()
		ackNote := ""
		select {
		case <-written:
		case <-timer.C:
			ackNote = fmt.Sprintf("the acknowledgement of its completeCompanionShutdown call was not observed written to its transport within %s, so the companion may not have received it", ackWait)
			s.companionRT.forgetAck(c.SessionID)
			s.logger.Warn("companion shutdown acknowledgement not observed as written; stopping anyway (the handoff is recorded)", "companion", c.Name, "session_id", c.SessionID, "request_id", requestID, "transport", hop, "waited", ackWait)
		}
		time.Sleep(grace)
		s.finishCompanionShutdown(context.Background(), c, sd, ackNote)
	}()
	return accepted, nil
}

// finishCompanionShutdown terminates a companion whose handoff is recorded
// and reports the actual outcome to its architect session.
// ackNote, when set, says why the completion's acknowledgement was not
// observed written before the stop; the report carries it.
func (s *Service) finishCompanionShutdown(ctx context.Context, c domain.Companion, sd domain.CompanionShutdown, ackNote string) {
	err := s.stopCompanion(ctx, c.SessionID, sd)
	now := time.Now().UTC()
	if err != nil {
		sd.Status, sd.Error = domain.ShutdownStopFailed, err.Error()
		c.Status, c.Error = domain.CompanionStopFailed, "termination not confirmed: "+err.Error()
		s.logger.Error("companion termination not confirmed", "companion", c.Name, "session_id", c.SessionID, "request_id", sd.RequestID, "error", err)
	} else {
		sd.Status, sd.Error, sd.StoppedAt = domain.ShutdownStopped, "", &now
		c.Status, c.Error = domain.CompanionStopped, ""
	}
	if updateErr := s.companionRepo.UpdateShutdown(ctx, sd); updateErr != nil {
		s.logger.Error("record companion shutdown outcome", "request_id", sd.RequestID, "error", updateErr)
	}
	_ = s.updateCompanion(ctx, &c)
	s.reportShutdownOutcome(ctx, c, sd, ackNote)
}

// stopCompanion ends the companion session; an already removed session is
// already stopped.
func (s *Service) stopCompanion(ctx context.Context, sessionID string, sd domain.CompanionShutdown) error {
	ctx, done, err := s.beginTeardown(ctx, sessionID)
	if err != nil {
		return err
	}
	defer done()
	session, err := s.repo.GetSession(ctx, sessionID)
	if errors.As(err, new(*domain.NotFoundError)) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.endCompanionSession(ctx, session, "shutdown", "companion shut down", map[string]any{"request_id": sd.RequestID, "handoff": sd.Handoff})
}

func (s *Service) reportShutdownOutcome(ctx context.Context, c domain.Companion, sd domain.CompanionShutdown, ackNote string) {
	outcome := "stopped; termination confirmed"
	if sd.Status == domain.ShutdownStopFailed {
		outcome = "termination NOT confirmed: " + sd.Error + ". The companion may still be running; it stays stop_failed and blocks your conclusion until its termination is confirmed (the user can retry it by discarding the companion session)"
	}
	if ackNote != "" {
		outcome += ". Note: " + ackNote + "; its handoff was recorded before the stop"
	}
	body := fmt.Sprintf("Companion %s shutdown (request_id %s): %s.\n\nHandoff:\n%s", c.Name, sd.RequestID, outcome, sd.Handoff)
	if _, err := s.EnqueueAgentMessage(ctx, AgentMessageParams{
		Kind:         domain.AgentMessageCompanionShutdownOutcome,
		Recipient:    domain.AgentMessageRecipientSession + sd.ArchitectSessionID,
		ArchitectKey: sd.ArchitectKey,
		DedupeKey:    fmt.Sprintf("%s:%s:%s:%d", domain.AgentMessageCompanionShutdownOutcome, sd.RequestID, sd.Status, time.Now().UnixNano()),
		Body:         body,
		Payload:      map[string]any{"request_id": sd.RequestID, "companion": c.Name, "status": string(sd.Status), "handoff": sd.Handoff, "error": sd.Error},
	}); err != nil {
		s.logger.Error("report companion shutdown outcome", "request_id", sd.RequestID, "error", err)
	}
}

// DiscardCompanion is the user's explicit retry of a companion termination
// the daemon could not confirm (stop_failed), or the removal of a companion
// whose process already ended (failed). A running companion is never
// discarded: it ends through its architect's shutdown request.
func (s *Service) DiscardCompanion(ctx context.Context, sessionID string) error {
	repo, err := s.companionStore()
	if err != nil {
		return err
	}
	c, err := repo.GetCompanionBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	switch c.Status {
	case domain.CompanionStopFailed, domain.CompanionFailed:
	default:
		return &domain.ConflictError{Resource: "companion", Field: "status", Message: fmt.Sprintf("companion %s is %s; only a companion whose termination failed or whose process ended can be discarded — a running companion ends through its architect's requestCompanionShutdown", c.Name, c.Status)}
	}
	sd, sdErr := repo.GetShutdownBySession(ctx, sessionID)
	if err := s.discardCompanionSession(ctx, sessionID); err != nil {
		return fmt.Errorf("termination of companion %s still not confirmed: %w", c.Name, err)
	}
	if c.Status == domain.CompanionStopFailed {
		c.Status, c.Error = domain.CompanionStopped, ""
		_ = s.updateCompanion(ctx, &c)
		if sdErr == nil {
			now := time.Now().UTC()
			sd.Status, sd.Error, sd.StoppedAt = domain.ShutdownStopped, "", &now
			if err := repo.UpdateShutdown(ctx, sd); err != nil {
				s.logger.Error("record companion shutdown outcome", "request_id", sd.RequestID, "error", err)
			}
			s.reportShutdownOutcome(ctx, c, sd, "")
		} else {
			s.notifyCompanionFailed(ctx, c, "its termination was confirmed after the user discarded it")
		}
	} else {
		s.emitCompanionChanged(c.ArchitectKey, sessionID)
	}
	return nil
}

// checkShutdownRequestedCommit settles an uncommitted shutdown instruction
// against the request record.
func (s *Service) checkShutdownRequestedCommit(ctx context.Context, m domain.AgentMessage) messageCommitVerdict {
	requestID, _ := m.Payload["request_id"].(string)
	if requestID == "" || s.companionRepo == nil {
		return messageCommitVerdict{failed: "malformed shutdown instruction: request_id missing"}
	}
	_, err := s.companionRepo.GetShutdown(ctx, requestID)
	switch {
	case err == nil:
		return messageCommitVerdict{committed: true}
	case errors.As(err, new(*domain.NotFoundError)):
		return messageCommitVerdict{failed: "the shutdown request was never recorded"}
	default:
		return messageCommitVerdict{condition: "shutdown request cannot be read: " + err.Error()}
	}
}

func shutdownDedupeKey(requestID string) string {
	return string(domain.AgentMessageCompanionShutdownRequested) + ":" + requestID
}

// canonicalRequestID validates an architect-generated UUID.
func canonicalRequestID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &domain.ValidationError{Field: "request_id", Message: "is required: generate a UUID for this shutdown request"}
	}
	id, err := uuid.Parse(raw)
	if err != nil || len(raw) != 36 {
		return "", &domain.ValidationError{Field: "request_id", Message: "must be a UUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx)"}
	}
	return id.String(), nil
}
