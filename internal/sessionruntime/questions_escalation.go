package sessionruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// Worker question escalation.
//
// A ticket worker's askQuestion that the user has not answered after
// config.yaml's question_escalation_delay (default 120 s, 0 disables) is
// announced once to the project architect through the durable agent-message
// outbox (kind question_escalated, recipient architect:<key>), so the
// architect can unblock the worker from knowledge it already has. The notice
// changes nothing about the question: the user's phone alert was already
// sent, the one-hour expiry, the cancellation and restart semantics stay, and
// a failed notice leaves the user's pending question untouched. Nothing
// launches an architect session for it; an architect with no running session
// gets the notice in its next one — unless the question has resolved by then,
// in which case the dispatcher withdraws the undelivered notice. Only
// ticket-worker questions escalate; architect and Action questions never do.
//
// The architect answers with answerQuestion, authorized here: the caller must
// be a running architect session of the question's project and the question
// an escalated ticket-worker question of that project. Exactly one answer
// wins, user or architect, under the question store's lock; the loser gets a
// not_answerable outcome naming what won. The source of the answer travels
// with the resolution (answered_by) and to the worker as a prefix, so an
// architect answer is never read as the user's answer or approval.

const questionEscalationTool = "answerQuestion"

func questionEscalationDedupeKey(questionID string) string {
	return string(domain.AgentMessageQuestionEscalated) + ":" + questionID
}

// escalationDelayFor says whether this question escalates and after how long:
// only a ticket worker's, only when escalation is configured, and only when
// the delay ends before the question expires (an escalation after the expiry
// would announce nothing answerable).
func (s *Service) escalationDelayFor(q domain.AgentQuestion) (time.Duration, bool) {
	if q.Origin.SessionType != domain.SessionTypeTicket || q.Origin.ArchitectKey == "" {
		return 0, false
	}
	delay := s.questionEscalationDelay
	if delay <= 0 || delay >= time.Until(q.ExpiresAt) {
		return 0, false
	}
	return delay, true
}

// escalateQuestion marks the pending question escalated and records the
// notice to the architect. Any failure is logged and leaves the question
// pending and unescalated: the user was already alerted and can still answer.
func (s *Service) escalateQuestion(ctx context.Context, q domain.AgentQuestion) {
	now := time.Now().UTC()
	if !s.questions.markEscalated(q.ID, now) {
		return
	}
	elapsed := int(now.Sub(q.CreatedAt).Round(time.Second) / time.Second)
	_, err := s.EnqueueAgentMessage(context.WithoutCancel(ctx), AgentMessageParams{
		Kind:         domain.AgentMessageQuestionEscalated,
		Recipient:    domain.AgentMessageRecipientArchitect + q.Origin.ArchitectKey,
		ArchitectKey: q.Origin.ArchitectKey,
		DedupeKey:    questionEscalationDedupeKey(q.ID),
		Body:         questionEscalationBody(q, elapsed, now),
		Payload: map[string]any{
			"question_id":       q.ID,
			"session_id":        q.Origin.SessionID,
			"ticket_id":         q.Origin.TicketID,
			"question":          q.Question,
			"answers":           q.Answers,
			"recommended_index": q.RecommendedIndex,
			"created_at":        q.CreatedAt.UTC().Format(time.RFC3339Nano),
			"expires_at":        q.ExpiresAt.UTC().Format(time.RFC3339Nano),
			"elapsed_seconds":   elapsed,
		},
	})
	if err != nil {
		s.questions.unmarkEscalated(q.ID)
		s.logger.Error("escalate worker question to architect; the question stays pending for the user",
			"session_id", q.Origin.SessionID, "question_id", q.ID, "architect_key", q.Origin.ArchitectKey, "error", err)
		return
	}
	s.logger.Info("worker question escalated to architect",
		"session_id", q.Origin.SessionID, "question_id", q.ID, "architect_key", q.Origin.ArchitectKey, "elapsed_seconds", elapsed)
}

// questionEscalationBody is the notice the architect reads. The second
// paragraph is agreed wording; the expiry makes clear that the question does
// not stay answerable indefinitely.
func questionEscalationBody(q domain.AgentQuestion, elapsedSeconds int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Unanswered worker question on ticket %s (worker session %s). Question id: %s.\n", q.Origin.TicketID, q.Origin.SessionID, q.ID)
	fmt.Fprintf(&b, "Question: %s\n", q.Question)
	b.WriteString("Choices:")
	for i, answer := range q.Answers {
		fmt.Fprintf(&b, " %d. %s", i+1, answer)
		if i == q.RecommendedIndex {
			b.WriteString(" (recommended)")
		}
		if i < len(q.Answers)-1 {
			b.WriteString(";")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "The user has not answered this worker question after %d seconds. If you already know the answer and are confident, you may answer using %s. Otherwise, no action is required. The user has already been notified and can answer when they return.", elapsedSeconds, questionEscalationTool)
	remaining := q.ExpiresAt.Sub(now).Round(time.Minute)
	fmt.Fprintf(&b, " The question expires at %s (in about %s); after that nobody can answer it.", q.ExpiresAt.UTC().Format(time.RFC3339), remaining)
	return b.String()
}

// questionEscalationStale says why a question_escalated notice no longer
// announces anything answerable: the question resolved (or is unknown — a
// restart interrupted it). Empty while the question is still pending.
func (s *Service) questionEscalationStale(m domain.AgentMessage) string {
	questionID, _ := m.Payload["question_id"].(string)
	if questionID == "" {
		return "malformed question_escalated payload: question id missing"
	}
	q, ok := s.questions.lookup(questionID)
	switch {
	case !ok:
		return "withdrawn: question " + questionID + " is no longer pending (it ended before the notice was delivered)"
	case q.Status != domain.QuestionPending:
		return "withdrawn: question " + questionID + " was " + string(q.Status) + " before the notice was delivered"
	default:
		return ""
	}
}

// checkQuestionEscalatedCommit settles a pending_commit escalation (the
// enqueue was interrupted between begin and commit): the question must still
// be pending for the notice to be worth delivering.
func (s *Service) checkQuestionEscalatedCommit(_ context.Context, m domain.AgentMessage) messageCommitVerdict {
	if stale := s.questionEscalationStale(m); stale != "" {
		return messageCommitVerdict{failed: stale}
	}
	return messageCommitVerdict{committed: true}
}

// AnswerQuestionAsArchitect is the architect agent's answer to an escalated
// ticket-worker question of its project. The caller is identified by its own
// session, never by a claimed architect key. A question of another project,
// an unknown id or a non-ticket question is NotFound; a caller that is not a
// running architect session is a ConflictError. A question that is pending
// but not escalated, or already resolved, is a not_answerable outcome: the
// architect is told why and must not retry, and the winning resolution is
// never overwritten.
func (s *Service) AnswerQuestionAsArchitect(ctx context.Context, architectSessionID, questionID, answer string) (domain.ArchitectAnswerResult, error) {
	answer, err := domain.NormalizeQuestionResponse(answer)
	if err != nil {
		return domain.ArchitectAnswerResult{}, err
	}
	session, err := s.repo.GetSession(ctx, architectSessionID)
	if err != nil {
		return domain.ArchitectAnswerResult{}, err
	}
	if session.SessionType != domain.SessionTypeArchitect || session.ArchitectKey == "" {
		return domain.ArchitectAnswerResult{}, &domain.ConflictError{Resource: "session", Field: "session_type", Message: "only an architect session may answer an escalated worker question"}
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.ArchitectAnswerResult{}, &domain.ConflictError{Resource: "session", Field: "status", Message: "session " + architectSessionID + " is not running"}
	}
	current, ok := s.questions.lookup(questionID)
	if !ok || current.Origin.ArchitectKey != session.ArchitectKey || current.Origin.SessionType != domain.SessionTypeTicket {
		return domain.ArchitectAnswerResult{}, &domain.NotFoundError{Resource: "question", ID: questionID}
	}
	if current.Status == domain.QuestionPending && !s.questions.escalated(questionID) {
		return domain.ArchitectAnswerResult{
			Outcome:  domain.ArchitectAnswerNotAnswerable,
			Reason:   "the question has not been escalated to you: the user has it and is still within the escalation delay (or escalation is disabled); leave it to the user",
			Question: current,
		}, nil
	}
	if final, won := s.finishQuestion(ctx, questionID, domain.QuestionAnswered, questionAnswer{text: answer, by: domain.QuestionAnsweredByArchitect}, ""); won {
		s.logger.Info("architect answered escalated worker question",
			"architect_session_id", architectSessionID, "worker_session_id", final.Origin.SessionID, "question_id", questionID)
		return domain.ArchitectAnswerResult{Outcome: domain.ArchitectAnswerAnswered, Question: final}, nil
	}
	resolved, _ := s.questions.lookup(questionID)
	return domain.ArchitectAnswerResult{
		Outcome:  domain.ArchitectAnswerNotAnswerable,
		Reason:   "no longer answerable: " + questionEndDescription(resolved),
		Question: resolved,
	}, nil
}
