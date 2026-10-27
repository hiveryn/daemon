package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
	sd "github.com/hiveryn/shared/domain"
)

type InteractionMode = sd.InteractionMode

const (
	InteractionTerminal = sd.InteractionTerminal
	InteractionChat     = sd.InteractionChat
)

type ConversationCapabilities = sd.ConversationCapabilities
type SubmissionRequest = sd.SubmissionRequest
type Submission = sd.Submission
type ConversationItem = sd.ConversationItem
type ConversationPage = sd.ConversationPage
type ConversationQuery = sd.ConversationQuery

type SequenceQuery = sd.SequenceQuery
type SubmissionPage = sd.SubmissionPage
type ArchivedChat = sd.ArchivedChat
type ArchivedChatPage = sd.ArchivedChatPage

type ConversationRepository interface {
	ListSubmissions(context.Context, string, SequenceQuery) (SubmissionPage, error)
	ArchiveChat(context.Context, ArchivedChat) error
	ListArchivedChats(context.Context, SequenceQuery) (ArchivedChatPage, error)
	IsArchivedChat(context.Context, string) (bool, error)
	EnqueueSubmission(context.Context, string, SubmissionRequest, string) (Submission, error)
	GetSubmission(context.Context, string, string) (Submission, error)
	PendingSubmission(context.Context, string) (*Submission, error)
	ReserveSubmission(context.Context, string) (*Submission, error)
	UpdateSubmission(context.Context, Submission) error
	PutConversationItem(context.Context, string, ConversationItem) error
	ReadConversation(context.Context, string, ConversationQuery) (ConversationPage, error)
	SettleSubmissions(context.Context, string, string, string) error
	FailQueuedSubmissions(context.Context, string, string) error
}

// TerminalSubmission is a conversation-API input addressed to a terminal-owned
// session, with its durable dispatch reservation.
type TerminalSubmission struct {
	Submission
	ProviderRef string
	HandedAt    time.Time
	// HandedTerminal is the main terminal (provider process) that received it.
	HandedTerminal string
}

// TerminalConversationRepository persists dispatch reservations for input into
// existing terminal-owned sessions.
type TerminalConversationRepository interface {
	// OpenTerminalSubmissions lists queued and started submissions in admission order.
	OpenTerminalSubmissions(context.Context, string) ([]TerminalSubmission, error)
	// ReserveTerminalSubmission moves a queued submission to started with its
	// provider reference before any native hand-off; false if it was not queued.
	ReserveTerminalSubmission(ctx context.Context, sessionID, id, providerRef, terminalID string, handedAt time.Time) (bool, error)
	// ReleaseTerminalSubmission returns a started submission to queued when the
	// hand-off demonstrably never reached the provider.
	ReleaseTerminalSubmission(ctx context.Context, sessionID, id, reason string) error
	// TerminalProviderRefs maps provider references to submission ids.
	TerminalProviderRefs(context.Context, string) (map[string]string, error)
}

type ConversationService interface {
	ListSubmissions(context.Context, string, SequenceQuery) (SubmissionPage, error)
	ListArchivedChats(context.Context, SequenceQuery) (ArchivedChatPage, error)
	SubmitMessage(context.Context, string, SubmissionRequest) (Submission, error)
	GetSubmission(context.Context, string, string) (Submission, error)
	ReadConversation(context.Context, string, ConversationQuery) (ConversationPage, error)
}

// SubmissionIdentity makes an outbox reservation name its queue receipt before
// enqueue, closing the crash window between the two durable aggregates.
func SubmissionIdentity(sessionID, requestID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(sessionID+"\x00"+requestID)).String()
}
