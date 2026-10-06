package sessionruntime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/ntfy"
)

// Agent notifications and questions (see shared domain/question.go).
//
// notify publishes a phone alert and returns. askQuestion publishes a phone
// alert, raises a pending question in the originating desktop session and
// holds the agent's call open until exactly one resolution wins: the user's
// answer, the server-side expiry, the agent's call ending, the session ending
// or the daemon stopping. Provider cancellation is unreliable (a call may stay
// open after a cancel, or a cancel may arrive after the result), so the expiry
// is enforced here regardless of what the client does.
//
// A question is not an intent: it approves nothing and has no policy. Its
// pending state lives in memory with its waiter — an answer is only accepted
// while the agent's call is still waiting for it — and its lifecycle is
// published as durable "question" session events, which the desktop replays on
// reconnect. A restart cannot keep the agent's call alive, so
// ReconcileQuestions resolves every question the log still shows pending.

const (
	sessionEventTypeQuestion     = "question"
	sessionEventTypeNotification = "notification"

	questionReasonExpired     = "nobody answered within 1 hour"
	questionReasonAbandoned   = "the agent stopped waiting for the answer (its tool call was cancelled or disconnected)"
	questionReasonSessionEnd  = "the session ended before the question was answered"
	questionReasonShutdown    = "the daemon stopped while the question was pending"
	questionReasonRestarted   = "the daemon restarted while the question was pending"
	questionRecentRetention   = 24 * time.Hour
	notificationTitleMaxRunes = 120
)

// notificationPublisher is the ntfy publisher, an interface for tests.
type notificationPublisher interface {
	Publish(context.Context, ntfy.Message) error
	Server() string
}

// questionWaiter is one pending question and its single resolution. done is
// closed once final is set; whoever closes it won.
type questionWaiter struct {
	question domain.AgentQuestion
	done     chan struct{}
	final    domain.AgentQuestion
}

// questionStore holds pending questions and, briefly, resolved ones so a late
// answer gets a precise "no longer answerable" reason.
type questionStore struct {
	mu      sync.Mutex
	pending map[string]*questionWaiter
	recent  map[string]domain.AgentQuestion
}

func newQuestionStore() *questionStore {
	return &questionStore{
		pending: map[string]*questionWaiter{},
		recent:  map[string]domain.AgentQuestion{},
	}
}

func (st *questionStore) add(w *questionWaiter) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pending[w.question.ID] = w
}

// resolve is the single-winner transition out of pending. It returns the final
// question and true for the winner; a loser (already resolved) gets false.
func (st *questionStore) resolve(id string, status domain.QuestionStatus, answer, reason string, at time.Time) (domain.AgentQuestion, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	w, ok := st.pending[id]
	if !ok {
		return domain.AgentQuestion{}, false
	}
	delete(st.pending, id)
	final := w.question
	final.Status = status
	final.Answer = answer
	final.Reason = reason
	w.final = final
	close(w.done)
	for qid, q := range st.recent {
		if at.Sub(q.ExpiresAt) > questionRecentRetention {
			delete(st.recent, qid)
		}
	}
	st.recent[id] = final
	return final, true
}

// lookup returns a pending question, or the resolved one if it ended recently.
func (st *questionStore) lookup(id string) (domain.AgentQuestion, bool) {
	if st == nil {
		return domain.AgentQuestion{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if w, ok := st.pending[id]; ok {
		return w.question, true
	}
	q, ok := st.recent[id]
	return q, ok
}

func (st *questionStore) pendingIDs(sessionID string) []string {
	if st == nil {
		// Services assembled without New (tests) have no questions.
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	ids := []string{}
	for id, w := range st.pending {
		if sessionID == "" || w.question.Origin.SessionID == sessionID {
			ids = append(ids, id)
		}
	}
	return ids
}

// questionSubject identifies a session in phone alerts: Title names the
// project and session, so an alert is attributable without opening the
// desktop.
type questionSubject struct {
	Origin domain.QuestionOrigin
	Title  string
}

// notifierFromConfig builds the ntfy publisher from config.yaml, or returns nil
// when notifications.ntfy is absent.
func notifierFromConfig(cfg config.Config) notificationPublisher {
	if cfg.Notifications == nil || cfg.Notifications.Ntfy == nil {
		return nil
	}
	return ntfy.New(*cfg.Notifications.Ntfy, nil)
}

// SetNotificationPublisher replaces the ntfy publisher (nil disables phone
// delivery). The daemon builds it from config.yaml at startup.
func (s *Service) SetNotificationPublisher(p notificationPublisher) {
	s.notifier = p
}

// Notify publishes the agent's message to the user's phone. It returns only
// after the ntfy server accepted it; a missing configuration or a refused or
// failed publication is an error, never a success.
func (s *Service) Notify(ctx context.Context, sessionID, message string) (domain.NotifyResult, error) {
	message, err := domain.NormalizeNotifyMessage(message)
	if err != nil {
		return domain.NotifyResult{}, err
	}
	subject, err := s.questionSubject(ctx, sessionID)
	if err != nil {
		return domain.NotifyResult{}, err
	}
	if err := s.publishPhoneAlert(ctx, subject, ntfy.Message{
		Title:    subject.Title,
		Body:     message,
		Priority: ntfy.PriorityDefault,
		Tags:     []string{"bell"},
	}); err != nil {
		s.recordNotification(ctx, sessionID, "failed", message, err.Error())
		return domain.NotifyResult{}, err
	}
	s.recordNotification(ctx, sessionID, "sent", message, "")
	return domain.NotifyResult{Text: domain.NotificationSentText}, nil
}

// AskQuestion alerts the user's phone, raises the question in the desktop
// session and blocks until it resolves. A failed phone alert fails the call
// before any question exists, so the agent is never left believing the user
// was alerted.
func (s *Service) AskQuestion(ctx context.Context, sessionID string, req domain.AskQuestionRequest) (domain.AskQuestionResult, error) {
	req, err := req.Normalize()
	if err != nil {
		return domain.AskQuestionResult{}, err
	}
	subject, err := s.questionSubject(ctx, sessionID)
	if err != nil {
		return domain.AskQuestionResult{}, err
	}

	now := time.Now().UTC()
	timeout := s.questionTimeout
	if timeout <= 0 {
		timeout = domain.QuestionTimeout
	}
	question := domain.AgentQuestion{
		ID:               uuid.NewString(),
		Question:         req.Question,
		Answers:          req.Answers,
		RecommendedIndex: *req.RecommendedIndex,
		Origin:           subject.Origin,
		Status:           domain.QuestionPending,
		CreatedAt:        now,
		ExpiresAt:        now.Add(timeout),
	}

	if err := s.publishPhoneAlert(ctx, subject, ntfy.Message{
		Title:    subject.Title,
		Body:     questionAlertBody(question),
		Priority: ntfy.PriorityHigh,
		Tags:     []string{"question"},
	}); err != nil {
		return domain.AskQuestionResult{}, fmt.Errorf("%w; the question was NOT asked", err)
	}

	waiter := &questionWaiter{question: question, done: make(chan struct{})}
	s.questions.add(waiter)
	if err := s.publishQuestionRequired(ctx, question); err != nil {
		if final, won := s.questions.resolve(question.ID, domain.QuestionCancelled, "", "the question could not be shown in the desktop", time.Now().UTC()); won {
			s.publishQuestionResolved(ctx, final)
		}
		return domain.AskQuestionResult{}, fmt.Errorf("raise question in the desktop session: %w", err)
	}
	s.logger.Info("agent question pending",
		"session_id", sessionID, "question_id", question.ID, "expires_at", question.ExpiresAt)

	timer := time.NewTimer(time.Until(question.ExpiresAt))
	defer timer.Stop()
	select {
	case <-waiter.done:
	case <-timer.C:
		s.finishQuestion(ctx, question.ID, domain.QuestionExpired, "", questionReasonExpired)
	case <-ctx.Done():
		if _, won := s.finishQuestion(ctx, question.ID, domain.QuestionCancelled, "", questionReasonAbandoned); won {
			return domain.AskQuestionResult{}, fmt.Errorf("agent stopped waiting for question %s: %w", question.ID, ctx.Err())
		}
	}
	<-waiter.done
	final := waiter.final
	return domain.AskQuestionResult{QuestionID: final.ID, Status: final.Status, Text: questionResultText(final)}, nil
}

// AnswerQuestion submits the user's answer. Only a question that is pending in
// this session is answerable: anything else is a ConflictError saying why (or
// NotFound for an unknown id or another session's question), so a stale or
// duplicate answer is rejected rather than delivered nowhere.
func (s *Service) AnswerQuestion(ctx context.Context, sessionID, questionID, answer string) (domain.AgentQuestion, error) {
	answer, err := domain.NormalizeQuestionResponse(answer)
	if err != nil {
		return domain.AgentQuestion{}, err
	}
	current, ok := s.questions.lookup(questionID)
	if !ok || current.Origin.SessionID != sessionID {
		return domain.AgentQuestion{}, &domain.NotFoundError{Resource: "question", ID: questionID}
	}
	if final, won := s.finishQuestion(ctx, questionID, domain.QuestionAnswered, answer, ""); won {
		return final, nil
	}
	resolved, _ := s.questions.lookup(questionID)
	return domain.AgentQuestion{}, &domain.ConflictError{
		Resource: "question",
		Field:    "status",
		Message:  "no longer answerable: " + questionEndDescription(resolved),
	}
}

// finishQuestion resolves a pending question and publishes its durable
// resolved event. Only the winner publishes.
func (s *Service) finishQuestion(ctx context.Context, id string, status domain.QuestionStatus, answer, reason string) (domain.AgentQuestion, bool) {
	final, won := s.questions.resolve(id, status, answer, reason, time.Now().UTC())
	if won {
		s.logger.Info("agent question resolved",
			"session_id", final.Origin.SessionID, "question_id", id, "status", status, "reason", reason)
		s.publishQuestionResolved(ctx, final)
	}
	return final, won
}

// cancelSessionQuestions resolves every question still pending on an ending
// session. Called from the single session-end funnel.
func (s *Service) cancelSessionQuestions(ctx context.Context, sessionID string) {
	for _, id := range s.questions.pendingIDs(sessionID) {
		s.finishQuestion(ctx, id, domain.QuestionCancelled, "", questionReasonSessionEnd)
	}
}

// interruptQuestions resolves every pending question at shutdown, so blocked
// agents get an honest reply and the desktop stops offering them.
func (s *Service) interruptQuestions(ctx context.Context) {
	for _, id := range s.questions.pendingIDs("") {
		s.finishQuestion(ctx, id, domain.QuestionInterrupted, "", questionReasonShutdown)
	}
}

// ReconcileQuestions runs once at startup. Pending questions live in memory
// with their waiters, so any question the durable log still shows pending was
// orphaned by a crash or kill; it is resolved as interrupted so the desktop
// never offers an answer that would reach nobody.
func (s *Service) ReconcileQuestions(ctx context.Context) error {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	for _, session := range sessions {
		events, err := s.repo.ListSessionEvents(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("list session %s events: %w", session.ID, err)
		}
		for _, orphan := range unresolvedQuestions(events) {
			if _, live := s.questions.lookup(orphan); live {
				continue
			}
			if err := s.appendQuestionResolved(ctx, session.ID, orphan, domain.QuestionInterrupted, "", questionReasonRestarted); err != nil {
				return fmt.Errorf("resolve orphaned question %s: %w", orphan, err)
			}
		}
	}
	return nil
}

// unresolvedQuestions returns the ids of questions raised but never resolved in
// a session's log, oldest first.
func unresolvedQuestions(events []domain.SessionEvent) []string {
	order := []string{}
	open := map[string]bool{}
	for _, event := range events {
		if event.Type != sessionEventTypeQuestion {
			continue
		}
		id, _ := event.Raw["question_id"].(string)
		if id == "" {
			continue
		}
		switch event.Status {
		case sessionEventStatusReqd:
			if !open[id] {
				open[id] = true
				order = append(order, id)
			}
		case sessionEventStatusResolvd:
			delete(open, id)
		}
	}
	out := []string{}
	for _, id := range order {
		if open[id] {
			out = append(out, id)
		}
	}
	return out
}

func (s *Service) publishQuestionRequired(ctx context.Context, q domain.AgentQuestion) error {
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: q.Origin.SessionID,
		Type:      sessionEventTypeQuestion,
		Status:    sessionEventStatusReqd,
		Tool:      "askQuestion",
		Message:   q.Question,
		Raw: map[string]any{
			"question_id":       q.ID,
			"question":          q.Question,
			"answers":           q.Answers,
			"recommended_index": q.RecommendedIndex,
			"origin":            q.Origin,
			"created_at":        q.CreatedAt,
			"expires_at":        q.ExpiresAt,
		},
		At: q.CreatedAt,
	})
}

// publishQuestionResolved records the resolution. It must outlive the caller's
// context (an abandoned call's ctx is already done), and a failure is logged:
// the in-memory resolution has already happened and is authoritative.
func (s *Service) publishQuestionResolved(ctx context.Context, q domain.AgentQuestion) {
	if err := s.appendQuestionResolved(context.WithoutCancel(ctx), q.Origin.SessionID, q.ID, q.Status, q.Answer, q.Reason); err != nil {
		s.logger.Error("publish question resolved",
			"session_id", q.Origin.SessionID, "question_id", q.ID, "status", q.Status, "error", err)
	}
}

func (s *Service) appendQuestionResolved(ctx context.Context, sessionID, questionID string, status domain.QuestionStatus, answer, reason string) error {
	raw := map[string]any{"question_id": questionID, "status": string(status)}
	if answer != "" {
		raw["answer"] = answer
	}
	if reason != "" {
		raw["reason"] = reason
	}
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: sessionID,
		Type:      sessionEventTypeQuestion,
		Status:    sessionEventStatusResolvd,
		Tool:      "askQuestion",
		Message:   string(status),
		Raw:       raw,
		At:        time.Now().UTC(),
	})
}

// recordNotification logs a notify call in the session's event log. It is
// informational; a failure to record never changes the tool's outcome.
func (s *Service) recordNotification(ctx context.Context, sessionID, status, message, failure string) {
	raw := map[string]any{"message": message}
	if failure != "" {
		raw["error"] = failure
	}
	if err := s.appendAndPublishSessionEvent(context.WithoutCancel(ctx), domain.AppendSessionEventParams{
		SessionID: sessionID,
		Type:      sessionEventTypeNotification,
		Status:    status,
		Tool:      "notify",
		Message:   message,
		Raw:       raw,
		At:        time.Now().UTC(),
	}); err != nil {
		s.logger.Error("record notification event", "session_id", sessionID, "error", err)
	}
}

// publishPhoneAlert sends one ntfy message. Errors are actionable and carry the
// server's own reason; they never include credentials.
func (s *Service) publishPhoneAlert(ctx context.Context, subject questionSubject, msg ntfy.Message) error {
	if s.notifier == nil {
		return &domain.ConflictError{
			Resource: "notifications",
			Field:    "ntfy",
			Message:  "phone notifications are not configured: the user must add notifications.ntfy (server and topic) to ~/.hiveryn/config.yaml and restart the Hiveryn daemon; tell the user in the conversation instead",
		}
	}
	if err := s.notifier.Publish(ctx, msg); err != nil {
		s.logger.Warn("phone notification failed",
			"session_id", subject.Origin.SessionID, "server", s.notifier.Server(), "error", err)
		return fmt.Errorf("phone notification failed, the user was NOT notified: %w", err)
	}
	return nil
}

// questionSubject derives the origin and alert title from the stored session,
// never from the agent. Only a running session may notify or ask.
func (s *Service) questionSubject(ctx context.Context, sessionID string) (questionSubject, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return questionSubject{}, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return questionSubject{}, &domain.ConflictError{Resource: "session", Field: "status", Message: "session " + sessionID + " is not running"}
	}
	origin := domain.QuestionOrigin{
		ArchitectKey: session.ArchitectKey,
		SessionID:    session.ID,
		SessionType:  session.SessionType,
	}
	var title string
	switch session.SessionType {
	case domain.SessionTypeAction:
		origin.ExecutionID = session.ContextID
		name := "Action"
		if s.actions != nil {
			if run, err := s.actions.runs.GetActionRun(ctx, session.ContextID); err == nil {
				origin.Action = run.Action
				name = "Action " + run.Action
			} else {
				s.logger.Warn("resolve action for notification title", "session_id", sessionID, "error", err)
			}
		}
		title = name + " · " + shortID(session.ContextID)
	case domain.SessionTypeTicket:
		origin.TicketID = session.ContextID
		project, path := s.projectName(session.ArchitectKey)
		label := session.ContextID
		if path != "" {
			if ticket, err := s.tickets.GetTicket(ctx, path, session.ContextID); err == nil && strings.TrimSpace(ticket.Title) != "" {
				label = ticket.Title
			}
		}
		title = project + " · " + label
	default:
		project, _ := s.projectName(session.ArchitectKey)
		title = project + " · architect"
	}
	return questionSubject{Origin: origin, Title: truncateRunes(title, notificationTitleMaxRunes)}, nil
}

// projectName is the architect's configured name (falling back to its key) and
// workspace path.
func (s *Service) projectName(architectKey string) (string, string) {
	cfg, err := s.currentConfig()
	if err != nil {
		return architectKey, ""
	}
	architect, ok := cfg.Architects[architectKey]
	if !ok {
		return architectKey, ""
	}
	name := strings.TrimSpace(architect.Name)
	if name == "" {
		name = architectKey
	}
	return name, architect.Path
}

func questionAlertBody(q domain.AgentQuestion) string {
	var b strings.Builder
	b.WriteString(q.Question)
	b.WriteString("\n")
	for i, answer := range q.Answers {
		fmt.Fprintf(&b, "\n%d. %s", i+1, answer)
		if i == q.RecommendedIndex {
			b.WriteString(" (recommended)")
		}
	}
	fmt.Fprintf(&b, "\n\nAnswer in the Hiveryn desktop session before %s.", q.ExpiresAt.Local().Format("15:04"))
	return b.String()
}

// questionResultText is what the agent's call returns for a final question.
func questionResultText(q domain.AgentQuestion) string {
	switch q.Status {
	case domain.QuestionAnswered:
		return q.Answer
	case domain.QuestionExpired:
		return domain.QuestionTimeoutText
	default:
		return "The question ended without an answer: " + q.Reason + ". Stop here and wait for the user to get back."
	}
}

func questionEndDescription(q domain.AgentQuestion) string {
	switch q.Status {
	case domain.QuestionAnswered:
		return "it was already answered"
	case domain.QuestionExpired:
		return "it expired after 1 hour without an answer"
	case "":
		return "it is no longer pending"
	default:
		if q.Reason != "" {
			return q.Reason
		}
		return string(q.Status)
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}
