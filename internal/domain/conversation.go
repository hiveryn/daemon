package domain

import (
	"context"
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
