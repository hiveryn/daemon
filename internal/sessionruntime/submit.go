package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// Synchronous conversation input.
//
// POST /messages records a durable request identity and attempts delivery
// before answering; Hiveryn never keeps user input for later dispatch. The
// answer is one of: provider acceptance (the receipt, with accepted_at and the
// evidence that established it), a definitive rejection (a stable code; the
// input never reaches the provider later), or DELIVERY_UNKNOWN when a hand-off
// was attempted and its acceptance could not be established within the bound.
// An unknown input is reconciled from native evidence and never resent.
//
// The attempt runs detached from the request, bounded by submitDeliveryBound,
// so a client that disconnects or retries cannot cut a hand-off in half: a
// repeated request (same request_id and text) waits for the attempt in
// progress, or replays the recorded outcome. The hand-off slot admits one
// native hand-off per terminal session at a time, shared with the outbox
// dispatcher; a send that finds it taken is busy, not queued.

// submitDeliveryBound bounds one synchronous delivery attempt, from admission
// to established acceptance (a variable so tests can shorten it).
var submitDeliveryBound = 20 * time.Second

type deliveryState struct {
	mu sync.Mutex
	// handoffs holds the terminal sessions with a native hand-off in progress.
	handoffs map[string]chan struct{}
	// attempts holds the delivery attempt in progress per submission id.
	attempts map[string]chan struct{}
	// signals wakes a send waiting on its chat owner's evidence.
	signals map[string]chan struct{}
}

// tryHandoff claims the session's hand-off slot, or reports it taken.
func (s *Service) tryHandoff(sessionID string) (func(), bool) {
	d := &s.deliveries
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.handoffs[sessionID]; busy {
		return nil, false
	}
	if d.handoffs == nil {
		d.handoffs = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	d.handoffs[sessionID] = done
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.handoffs, sessionID)
			d.mu.Unlock()
			close(done)
		})
	}, true
}

// waitHandoff returns once no hand-off to the session is in progress.
func (s *Service) waitHandoff(ctx context.Context, sessionID string) error {
	for {
		s.deliveries.mu.Lock()
		done := s.deliveries.handoffs[sessionID]
		s.deliveries.mu.Unlock()
		if done == nil {
			return nil
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// beginAttempt registers a submission's attempt; finish ends it.
func (s *Service) beginAttempt(id string) (finish func()) {
	d := &s.deliveries
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.attempts == nil {
		d.attempts = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	d.attempts[id] = done
	return func() {
		d.mu.Lock()
		delete(d.attempts, id)
		d.mu.Unlock()
		close(done)
	}
}

func (s *Service) attemptInProgress(id string) chan struct{} {
	s.deliveries.mu.Lock()
	defer s.deliveries.mu.Unlock()
	return s.deliveries.attempts[id]
}

// deliverySignal returns the channel a chat send waits on; release removes it.
func (s *Service) deliverySignal(id string) (<-chan struct{}, func()) {
	d := &s.deliveries
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.signals == nil {
		d.signals = map[string]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	d.signals[id] = ch
	return ch, func() {
		d.mu.Lock()
		if d.signals[id] == ch {
			delete(d.signals, id)
		}
		d.mu.Unlock()
	}
}

// signalDelivery tells a waiting send that its submission changed.
func (s *Service) signalDelivery(id string) {
	s.deliveries.mu.Lock()
	ch := s.deliveries.signals[id]
	s.deliveries.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func validateSubmission(req domain.SubmissionRequest) error {
	if strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 200 {
		return &domain.ValidationError{Field: "request_id", Message: "must contain 1–200 characters"}
	}
	if strings.TrimSpace(req.Text) == "" || len(req.Text) > 1<<20 {
		return &domain.ValidationError{Field: "text", Message: "must contain 1–1048576 bytes"}
	}
	return nil
}

// SubmitMessage delivers user input synchronously (see the file comment).
func (s *Service) SubmitMessage(ctx context.Context, id string, req domain.SubmissionRequest) (domain.Submission, error) {
	if err := validateSubmission(req); err != nil {
		return domain.Submission{}, err
	}
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.Submission{}, err
	}
	sub, done, err := s.admitSubmission(ctx, id, req)
	if err != nil {
		return sub, err
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			// The attempt continues to its own outcome; the same request
			// replays it.
			return domain.Submission{}, ctx.Err()
		}
		if sub, err = repo.GetSubmission(ctx, id, sub.ID); err != nil {
			return sub, err
		}
	}
	return submissionOutcome(sub)
}

// admitSubmission records or recovers the request identity and starts its
// delivery attempt. done is closed when an attempt (this one, or one already
// in progress for the same request) finishes; nil when the record is final.
func (s *Service) admitSubmission(ctx context.Context, id string, req domain.SubmissionRequest) (domain.Submission, <-chan struct{}, error) {
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.Submission{}, nil, err
	}
	// Admission is serialized with teardown and with every other admission,
	// so a request identity and its attempt are registered together. Nothing
	// here waits on the network.
	s.launchMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.launchMu.Unlock()
		}
	}()
	prior, err := repo.GetSubmission(ctx, id, domain.SubmissionIdentity(id, req.RequestID))
	if err == nil {
		if prior.Text != req.Text || prior.Origin != "user" {
			return domain.Submission{}, nil, &domain.ConflictError{Resource: "submission", Field: "request_id", Message: "request_id was already used with different text or origin"}
		}
		return prior, s.attemptInProgress(prior.ID), nil
	}
	var missing *domain.NotFoundError
	if !errors.As(err, &missing) {
		return domain.Submission{}, nil, err
	}
	session, err := s.repo.GetSession(ctx, id)
	if errors.As(err, &missing) {
		archived, archiveErr := repo.IsArchivedChat(ctx, id)
		if archiveErr != nil {
			return domain.Submission{}, nil, archiveErr
		}
		if archived {
			return domain.Submission{}, nil, &domain.DeliveryError{Code: domain.DeliverySessionArchived, Message: "the session has ended and its conversation is archived; nothing was sent"}
		}
	}
	if err != nil {
		return domain.Submission{}, nil, err
	}
	var release func()
	code, reason := s.admissionRefusal(ctx, session, &release)
	status := domain.SubmissionDelivering
	if code != "" {
		status = domain.SubmissionRejected
	}
	sub, created, err := repo.InsertSubmission(ctx, id, req, "user", status, code, reason)
	if err != nil || !created || code != "" {
		if release != nil {
			release()
		}
		if err != nil || !created {
			return sub, s.attemptInProgress(sub.ID), err
		}
		return sub, nil, nil
	}
	finish := s.beginAttempt(sub.ID)
	s.launchMu.Unlock()
	locked = false

	// Detached from the request: the client leaving must not cut a hand-off
	// in half or strand the slot.
	attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitDeliveryBound)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer finish()
		defer cancel()
		if release != nil {
			defer release()
		}
		if session.InteractionMode == domain.InteractionChat {
			s.deliverChatInput(attemptCtx, session, sub)
		} else {
			s.deliverTerminalInput(attemptCtx, session, sub)
		}
		s.kickMessages()
	}()
	return sub, done, nil
}

// admissionRefusal returns why the session cannot take input now ("" when it
// can). For a terminal session it claims the hand-off slot into release.
func (s *Service) admissionRefusal(ctx context.Context, session domain.Session, release *func()) (string, string) {
	if _, ending := s.ending[session.ID]; ending {
		return domain.DeliverySessionEnding, "Not sent: the session is ending"
	}
	if session.Machine != "" && session.InteractionMode == domain.InteractionChat {
		return domain.DeliverySendUnsupported, "Not sent: remote SSH chat sessions are unsupported"
	}
	if session.InteractionMode != domain.InteractionChat && s.terminalConversationUnsupported(session) {
		return domain.DeliverySendUnsupported, "Not sent: " + s.terminalCapabilities(session).Limitation
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.DeliverySessionNotRunning, "Not sent: the session is not running"
	}
	if session.InteractionMode == domain.InteractionChat {
		return s.chatAdmissionRefusal(ctx, session)
	}
	if caps := s.terminalCapabilities(session); !caps.Send {
		return domain.DeliverySendUnavailable, "Not sent: " + caps.Limitation
	}
	claimed, ok := s.tryHandoff(session.ID)
	if !ok {
		return domain.DeliverySessionBusy, "Not sent: another input is being handed to this session right now"
	}
	*release = claimed
	return "", ""
}

// submissionOutcome turns a user receipt into the send's answer: success only
// once provider acceptance is established.
func submissionOutcome(sub domain.Submission) (domain.Submission, error) {
	if sub.AcceptedAt != nil {
		return sub, nil
	}
	code := sub.ErrorCode
	switch sub.Status {
	case domain.SubmissionRejected:
	case domain.SubmissionFailed:
		// Ended before any acceptance evidence: nothing reached the provider.
		if code == "" {
			code = domain.DeliveryRejected
		}
	default:
		code = domain.DeliveryUnknown
	}
	message := sub.Error
	if message == "" {
		message = "provider acceptance of this input is not established; it is never resent automatically"
	}
	return sub, &domain.DeliveryError{Code: code, Message: message, Submission: &sub}
}

// persistSubmission writes an attempt's outcome on a context of its own: the
// attempt's may already have expired.
func (s *Service) persistSubmission(sub domain.Submission) {
	repo, err := s.conversationRepo()
	if err != nil {
		s.logger.Error("persist submission outcome", "submission_id", sub.ID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := repo.UpdateSubmission(ctx, sub); err != nil {
		s.logger.Error("persist submission outcome", "submission_id", sub.ID, "status", sub.Status, "error", err)
	}
}

// ReconcileSubmissions runs once at startup, before restore: attempts the
// previous daemon left delivering become rejected (never handed over) or
// unknown (a hand-off was reserved).
func (s *Service) ReconcileSubmissions(ctx context.Context) error {
	repo, err := s.conversationRepo()
	if err != nil {
		return nil
	}
	return repo.ReconcileDeliveries(ctx)
}
