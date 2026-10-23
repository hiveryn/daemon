package domain

import (
	"context"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the agent notification and question contract from shared.
type (
	QuestionStatus         = sd.QuestionStatus
	QuestionOrigin         = sd.QuestionOrigin
	AgentQuestion          = sd.AgentQuestion
	NotifyRequest          = sd.NotifyRequest
	NotifyResult           = sd.NotifyResult
	AskQuestionRequest     = sd.AskQuestionRequest
	AskQuestionResult      = sd.AskQuestionResult
	AnswerQuestionRequest  = sd.AnswerQuestionRequest
	QuestionAnswerSource   = sd.QuestionAnswerSource
	ArchitectAnswerOutcome = sd.ArchitectAnswerOutcome
	ArchitectAnswerResult  = sd.ArchitectAnswerResult
)

const (
	QuestionPending     = sd.QuestionPending
	QuestionAnswered    = sd.QuestionAnswered
	QuestionExpired     = sd.QuestionExpired
	QuestionCancelled   = sd.QuestionCancelled
	QuestionInterrupted = sd.QuestionInterrupted

	MaxNotifyMessageLength    = sd.MaxNotifyMessageLength
	MaxQuestionLength         = sd.MaxQuestionLength
	MinQuestionAnswers        = sd.MinQuestionAnswers
	MaxQuestionAnswers        = sd.MaxQuestionAnswers
	MaxQuestionAnswerLength   = sd.MaxQuestionAnswerLength
	MaxQuestionResponseLength = sd.MaxQuestionResponseLength

	QuestionAnsweredByUser      = sd.QuestionAnsweredByUser
	QuestionAnsweredByArchitect = sd.QuestionAnsweredByArchitect

	ArchitectAnswerAnswered      = sd.ArchitectAnswerAnswered
	ArchitectAnswerNotAnswerable = sd.ArchitectAnswerNotAnswerable

	QuestionTimeout               = sd.QuestionTimeout
	NotificationSentText          = sd.NotificationSentText
	QuestionTimeoutText           = sd.QuestionTimeoutText
	QuestionArchitectAnswerPrefix = sd.QuestionArchitectAnswerPrefix
)

var (
	NormalizeNotifyMessage    = sd.NormalizeNotifyMessage
	NormalizeQuestionResponse = sd.NormalizeQuestionResponse
)

// QuestionService is the agent notification and question surface. Notify and
// AskQuestion are agent-facing (AskQuestion blocks until the question
// resolves); AnswerQuestion is the desktop's answer, scoped to the session.
// AnswerQuestionAsArchitect is the architect agent's answer to an escalated
// ticket-worker question of its own project, addressed by the architect's
// session: a question of another project, an unknown id or a non-architect
// caller is NotFound, and a settled or unescalated question is a
// not_answerable outcome rather than an error.
type QuestionService interface {
	Notify(ctx context.Context, sessionID, message string) (NotifyResult, error)
	AskQuestion(ctx context.Context, sessionID string, req AskQuestionRequest) (AskQuestionResult, error)
	AnswerQuestion(ctx context.Context, sessionID, questionID, answer string) (AgentQuestion, error)
	AnswerQuestionAsArchitect(ctx context.Context, architectSessionID, questionID, answer string) (ArchitectAnswerResult, error)
}
