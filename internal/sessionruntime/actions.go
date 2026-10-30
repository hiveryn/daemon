package sessionruntime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/ntfy"
)

// Action requests (requestAction; shared domain/companion.go).
//
// A companion proposes one concrete external action to the user. The phone
// alert goes first — a failed alert fails the call before any request exists
// — then the request is raised as a durable action_request/required event on
// the companion's session and the call waits for exactly one resolution: the
// user's decision in the desktop (accept, deny, reply), the one-hour expiry
// (enforced here, regardless of the client), a shutdown request for the
// companion, the session ending, the agent's call ending or the daemon
// stopping. Only accept authorizes, and only the proposal as submitted;
// Hiveryn executes nothing and never decides for the user. Like a question,
// a pending request lives in memory with its waiter, so a decision is
// accepted only while the agent can still receive it; ReconcileActions
// resolves requests a restart orphaned. Nothing here is an intent, a policy
// or a gate engine.

const (
	actionReasonExpired   = "the user did not decide within one hour"
	actionReasonAbandoned = "the agent stopped waiting for the decision (its tool call was cancelled or disconnected)"
	actionReasonShutdown  = "shutdown of this companion was requested before the user decided"
	actionReasonEnded     = "the session ended before the user decided"
	actionReasonStopped   = "the daemon stopped while the request was pending"
	actionReasonRestarted = "the daemon restarted while the request was pending"
	actionRecentRetention = 24 * time.Hour
)

type actionWaiter struct {
	request domain.ActionRequest
	done    chan struct{}
	final   domain.ActionRequest
	// raised: its required event is published. Closing a session resolves
	// only raised waiters, so a resolved event never precedes its required
	// event; an unraised one resolves itself in raise.
	raised bool
}

// actionStore holds pending requests and, briefly, resolved ones so a late
// decision gets a precise reason. closed records the sessions that admit no
// more requests (shutdown requested, session ended, process failed), and
// stopped the daemon's own shutdown. Admission and withdrawal are decided
// under the same lock, so a request whose phone alert was still in flight
// when its session closed can never become pending afterwards. Markers are
// never removed — removing one would readmit such a late request — at one
// short entry per ended session for the daemon's lifetime.
type actionStore struct {
	mu      sync.Mutex
	pending map[string]*actionWaiter
	recent  map[string]domain.ActionRequest
	closed  map[string]string
	stopped string
}

func newActionStore() *actionStore {
	return &actionStore{pending: map[string]*actionWaiter{}, recent: map[string]domain.ActionRequest{}, closed: map[string]string{}}
}

// add admits a request unless its session is closed, returning why not.
func (st *actionStore) add(w *actionWaiter) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if reason, closed := st.closedReason(w.request.Origin.SessionID); closed {
		return reason, false
	}
	st.pending[w.request.ID] = w
	return "", true
}

// raise marks a request's required event published. False with the reason
// when its session closed meanwhile: the caller resolves it.
func (st *actionStore) raise(id string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	w, ok := st.pending[id]
	if !ok {
		return "", true // already decided; the waiter is done
	}
	if reason, closed := st.closedReason(w.request.Origin.SessionID); closed {
		return reason, false
	}
	w.raised = true
	return "", true
}

func (st *actionStore) closedReason(sessionID string) (string, bool) {
	if st.stopped != "" {
		return st.stopped, true
	}
	reason, ok := st.closed[sessionID]
	return reason, ok
}

// close stops admitting requests for sessionID ("" = every session, the
// daemon stopping) and returns the raised requests to withdraw.
func (st *actionStore) close(sessionID, reason string) []string {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if sessionID == "" {
		if st.stopped == "" {
			st.stopped = reason
		}
	} else if _, ok := st.closed[sessionID]; !ok {
		st.closed[sessionID] = reason
	}
	ids := []string{}
	for id, w := range st.pending {
		if w.raised && (sessionID == "" || w.request.Origin.SessionID == sessionID) {
			ids = append(ids, id)
		}
	}
	return ids
}

// resolve is the single-winner transition out of pending.
func (st *actionStore) resolve(id string, status domain.ActionRequestStatus, message, reason string, at time.Time) (domain.ActionRequest, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	w, ok := st.pending[id]
	if !ok {
		return domain.ActionRequest{}, false
	}
	delete(st.pending, id)
	final := w.request
	final.Status = status
	final.Message = message
	final.Reason = reason
	if status == domain.ActionAccepted || status == domain.ActionDenied || status == domain.ActionReplied {
		final.DecidedAt = &at
	}
	w.final = final
	close(w.done)
	for rid, r := range st.recent {
		if at.Sub(r.ExpiresAt) > actionRecentRetention {
			delete(st.recent, rid)
		}
	}
	st.recent[id] = final
	return final, true
}

func (st *actionStore) lookup(id string) (domain.ActionRequest, bool) {
	if st == nil {
		return domain.ActionRequest{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if w, ok := st.pending[id]; ok {
		return w.request, true
	}
	r, ok := st.recent[id]
	return r, ok
}

// pendingFor lists a session's pending requests, oldest first.
func (st *actionStore) pendingFor(sessionID string) []domain.ActionRequest {
	out := []domain.ActionRequest{}
	if st == nil || sessionID == "" {
		return out
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, w := range st.pending {
		if w.request.Origin.SessionID == sessionID {
			out = append(out, w.request)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// RequestAction raises a companion's action request and blocks until it
// resolves. Request errors (validation, a session that may not ask, a failed
// phone alert) are returned as errors; a request that ended without a
// decision is a result carrying ErrorCode, which the MCP tool reports as a
// tool error.
func (s *Service) RequestAction(ctx context.Context, sessionID string, req domain.RequestActionRequest) (domain.RequestActionResult, error) {
	req, err := req.Normalize()
	if err != nil {
		return domain.RequestActionResult{}, err
	}
	session, c, err := s.companionOf(ctx, sessionID)
	if err != nil {
		return domain.RequestActionResult{}, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.RequestActionResult{}, &domain.ConflictError{Resource: "session", Field: "status", Message: "session " + sessionID + " is not running"}
	}
	if c.Status != domain.CompanionRunning {
		return domain.RequestActionResult{
			ErrorCode: domain.ActionRequestCancelledCode,
			Error:     "Shutdown of this companion was requested; no new action requests are accepted and no action is authorized. Preserve your work and call completeCompanionShutdown with the request_id from the shutdown message.",
		}, nil
	}
	now := time.Now().UTC()
	timeout := s.actionTimeout
	if timeout <= 0 {
		timeout = domain.ActionRequestTimeout
	}
	request := domain.ActionRequest{
		ID:          uuid.NewString(),
		Title:       req.Title,
		Description: req.Description,
		Origin: domain.ActionOrigin{
			ArchitectKey:       session.ArchitectKey,
			ArchitectSessionID: session.OwnerSessionID,
			SessionID:          session.ID,
			SessionType:        session.SessionType,
			Companion:          c.Name,
		},
		Status:    domain.ActionPending,
		CreatedAt: now,
		ExpiresAt: now.Add(timeout),
	}
	project, _ := s.projectName(session.ArchitectKey)
	if err := s.publishPhoneAlert(ctx, questionSubject{Origin: domain.QuestionOrigin{ArchitectKey: session.ArchitectKey, SessionID: session.ID, SessionType: session.SessionType}}, ntfy.Message{
		Title:    truncateRunes(project+" · "+c.Name+" · action request", notificationTitleMaxRunes),
		Body:     fmt.Sprintf("%s\n\nAccept, deny or reply in the Hiveryn desktop before %s.", request.Title, request.ExpiresAt.Local().Format("15:04")),
		Priority: ntfy.PriorityHigh,
		Tags:     []string{"lock"},
	}); err != nil {
		return domain.RequestActionResult{
			ErrorCode: domain.ActionRequestNotificationFailedCode,
			Error:     fmt.Sprintf("The phone notification failed, so the request was NOT raised and no action is authorized: %v", err),
		}, nil
	}
	// The phone alert can take seconds; the session may have closed (its
	// shutdown requested, its session ended) meanwhile. Admission is checked
	// again atomically with that withdrawal, so a late request never becomes
	// pending and can never be accepted.
	waiter := &actionWaiter{request: request, done: make(chan struct{})}
	if reason, ok := s.actions.add(waiter); !ok {
		request.Status, request.Reason = withdrawnStatus(reason), reason
		s.logger.Info("action request withdrawn before it was raised", "session_id", sessionID, "action_id", request.ID, "reason", reason)
		return actionResult(request), nil
	}
	if err := s.publishActionRequired(ctx, request); err != nil {
		if final, won := s.actions.resolve(request.ID, domain.ActionCancelled, "", "the request could not be recorded in the desktop session", time.Now().UTC()); won {
			s.publishActionResolved(ctx, final)
		}
		return domain.RequestActionResult{}, fmt.Errorf("record the action request in the session: %w; no request was raised and no action is authorized", err)
	}
	if reason, ok := s.actions.raise(request.ID); !ok {
		s.finishAction(ctx, request.ID, withdrawnStatus(reason), "", reason)
	}
	s.emitCompanionChanged(session.ArchitectKey, session.ID)
	s.logger.Info("action request pending", "session_id", sessionID, "action_id", request.ID, "expires_at", request.ExpiresAt)

	timer := time.NewTimer(time.Until(request.ExpiresAt))
	defer timer.Stop()
	select {
	case <-waiter.done:
	case <-timer.C:
		s.finishAction(ctx, request.ID, domain.ActionExpired, "", actionReasonExpired)
	case <-ctx.Done():
		if _, won := s.finishAction(ctx, request.ID, domain.ActionCancelled, "", actionReasonAbandoned); won {
			return domain.RequestActionResult{}, fmt.Errorf("agent stopped waiting for action request %s: %w", request.ID, ctx.Err())
		}
	}
	<-waiter.done
	return actionResult(waiter.final), nil
}

// actionResult is what the companion's call returns for a resolved request.
func actionResult(r domain.ActionRequest) domain.RequestActionResult {
	switch r.Status {
	case domain.ActionAccepted, domain.ActionDenied:
		return domain.RequestActionResult{Outcome: string(r.Status)}
	case domain.ActionReplied:
		return domain.RequestActionResult{Outcome: string(r.Status), Message: r.Message}
	case domain.ActionExpired:
		return domain.RequestActionResult{ErrorCode: domain.ActionRequestTimeoutCode, Error: domain.ActionRequestTimeoutText}
	case domain.ActionInterrupted:
		return domain.RequestActionResult{ErrorCode: domain.ActionRequestInterruptedCode, Error: "The request ended without a decision: " + r.Reason + ". No action is authorized; submit a new request with requestAction once Hiveryn is available."}
	default:
		if r.Reason == actionReasonShutdown {
			return domain.RequestActionResult{ErrorCode: domain.ActionRequestCancelledCode, Error: "Shutdown of this companion was requested before the user decided. The request is withdrawn and no action is authorized; do not execute it. Preserve your work and call completeCompanionShutdown with the request_id from the shutdown message, listing this proposal as not executed in your handoff."}
		}
		return domain.RequestActionResult{ErrorCode: domain.ActionRequestCancelledCode, Error: "The request ended without a decision: " + r.Reason + ". No action is authorized."}
	}
}

// DecideAction records the user's decision. Only a request pending in this
// session is decidable: anything else is a ConflictError saying why, or
// NotFound for an unknown id or another session's request.
func (s *Service) DecideAction(ctx context.Context, sessionID, actionID string, req domain.ActionDecisionRequest) (domain.ActionRequest, error) {
	req, err := req.Normalize()
	if err != nil {
		return domain.ActionRequest{}, err
	}
	current, ok := s.actions.lookup(actionID)
	if !ok || current.Origin.SessionID != sessionID {
		return domain.ActionRequest{}, &domain.NotFoundError{Resource: "action request", ID: actionID}
	}
	status := map[domain.ActionDecision]domain.ActionRequestStatus{
		domain.ActionDecisionAccept: domain.ActionAccepted,
		domain.ActionDecisionDeny:   domain.ActionDenied,
		domain.ActionDecisionReply:  domain.ActionReplied,
	}[req.Decision]
	if final, won := s.finishAction(ctx, actionID, status, req.Message, ""); won {
		return final, nil
	}
	resolved, _ := s.actions.lookup(actionID)
	reason := resolved.Reason
	if reason == "" {
		reason = "it was already " + string(resolved.Status)
	}
	return domain.ActionRequest{}, &domain.ConflictError{Resource: "action request", Field: "status", Message: "no longer decidable: " + reason}
}

// ListActions returns the session's pending requests.
func (s *Service) ListActions(ctx context.Context, sessionID string) ([]domain.ActionRequest, error) {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.actions.pendingFor(sessionID), nil
}

func (s *Service) finishAction(ctx context.Context, id string, status domain.ActionRequestStatus, message, reason string) (domain.ActionRequest, bool) {
	final, won := s.actions.resolve(id, status, message, reason, time.Now().UTC())
	if won {
		s.logger.Info("action request resolved", "session_id", final.Origin.SessionID, "action_id", id, "status", status, "reason", reason)
		s.publishActionResolved(ctx, final)
		s.emitCompanionChanged(final.Origin.ArchitectKey, final.Origin.SessionID)
	}
	return final, won
}

// closeSessionActions stops admitting requests for a session and withdraws
// the pending ones. A session never reopens: it is called when shutdown is
// requested, the session ends or its process failed for good.
func (s *Service) closeSessionActions(ctx context.Context, sessionID, reason string) {
	for _, id := range s.actions.close(sessionID, reason) {
		s.finishAction(ctx, id, domain.ActionCancelled, "", reason)
	}
}

// withdrawnStatus is the resolution of a request withdrawn for reason.
func withdrawnStatus(reason string) domain.ActionRequestStatus {
	if reason == actionReasonStopped {
		return domain.ActionInterrupted
	}
	return domain.ActionCancelled
}

// interruptActions resolves every pending request at shutdown and admits no
// new ones.
func (s *Service) interruptActions(ctx context.Context) {
	for _, id := range s.actions.close("", actionReasonStopped) {
		s.finishAction(ctx, id, domain.ActionInterrupted, "", actionReasonStopped)
	}
}

// ReconcileActions resolves, at startup, every request the durable log still
// shows pending: its waiter died with the previous process.
func (s *Service) ReconcileActions(ctx context.Context) error {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	for _, session := range sessions {
		if session.SessionType != domain.SessionTypeCompanion {
			continue
		}
		events, err := s.repo.ListSessionEvents(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("list session %s events: %w", session.ID, err)
		}
		open := map[string]bool{}
		order := []string{}
		for _, event := range events {
			if event.Type != domain.SessionEventTypeActionRequest {
				continue
			}
			id, _ := event.Raw["action_id"].(string)
			switch {
			case id == "":
			case event.Status == sessionEventStatusReqd && !open[id]:
				open[id] = true
				order = append(order, id)
			case event.Status == sessionEventStatusResolvd:
				delete(open, id)
			}
		}
		for _, id := range order {
			if !open[id] {
				continue
			}
			if _, live := s.actions.lookup(id); live {
				continue
			}
			if err := s.appendActionResolved(ctx, session.ID, domain.ActionRequest{ID: id, Status: domain.ActionInterrupted, Reason: actionReasonRestarted}); err != nil {
				return fmt.Errorf("resolve orphaned action request %s: %w", id, err)
			}
		}
	}
	return nil
}

func (s *Service) publishActionRequired(ctx context.Context, r domain.ActionRequest) error {
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: r.Origin.SessionID,
		Type:      domain.SessionEventTypeActionRequest,
		Status:    sessionEventStatusReqd,
		Tool:      "requestAction",
		Message:   r.Title,
		Raw: map[string]any{
			"action_id":   r.ID,
			"title":       r.Title,
			"description": r.Description,
			"origin":      r.Origin,
			"created_at":  r.CreatedAt,
			"expires_at":  r.ExpiresAt,
		},
		At: r.CreatedAt,
	})
}

func (s *Service) publishActionResolved(ctx context.Context, r domain.ActionRequest) {
	if err := s.appendActionResolved(context.WithoutCancel(ctx), r.Origin.SessionID, r); err != nil {
		s.logger.Error("publish action request resolved", "session_id", r.Origin.SessionID, "action_id", r.ID, "status", r.Status, "error", err)
	}
}

func (s *Service) appendActionResolved(ctx context.Context, sessionID string, r domain.ActionRequest) error {
	raw := map[string]any{"action_id": r.ID, "status": string(r.Status)}
	if r.Message != "" {
		raw["message"] = r.Message
	}
	if r.Reason != "" {
		raw["reason"] = r.Reason
	}
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: sessionID,
		Type:      domain.SessionEventTypeActionRequest,
		Status:    sessionEventStatusResolvd,
		Tool:      "requestAction",
		Message:   string(r.Status),
		Raw:       raw,
		At:        time.Now().UTC(),
	})
}
