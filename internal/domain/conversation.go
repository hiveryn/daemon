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

type DeliveryError = sd.DeliveryError
type ConversationError = sd.ConversationError

const (
	SubmissionQueued     = sd.SubmissionQueued
	SubmissionDelivering = sd.SubmissionDelivering
	SubmissionRejected   = sd.SubmissionRejected
	SubmissionUnknown    = sd.SubmissionUnknown
	SubmissionAccepted   = sd.SubmissionAccepted
	SubmissionRunning    = sd.SubmissionRunning
	SubmissionBlocked    = sd.SubmissionBlocked
	SubmissionCompleted  = sd.SubmissionCompleted
	SubmissionFailed     = sd.SubmissionFailed
	SubmissionUncertain  = sd.SubmissionUncertain

	DeliverySessionBusy         = sd.DeliverySessionBusy
	DeliverySessionNotRunning   = sd.DeliverySessionNotRunning
	DeliverySessionEnding       = sd.DeliverySessionEnding
	DeliverySessionArchived     = sd.DeliverySessionArchived
	DeliverySendUnavailable     = sd.DeliverySendUnavailable
	DeliverySendUnsupported     = sd.DeliverySendUnsupported
	DeliveryRejected            = sd.DeliveryRejected
	DeliveryInterrupted         = sd.DeliveryInterrupted
	DeliveryQueuedBeforeCutover = sd.DeliveryQueuedBeforeCutover
	DeliveryUnknown             = sd.DeliveryUnknown

	HistoryUnsupported = sd.HistoryUnsupported
	HistoryUnavailable = sd.HistoryUnavailable
	HistoryReadFailed  = sd.HistoryReadFailed
)

// SequenceQuery pages a session's submissions by admission sequence (daemon
// internal: the single-client API exposes no receipt reads).
type SequenceQuery struct {
	AfterSequence int64
	Limit         int
}
type SubmissionPage struct {
	Submissions  []Submission
	NextSequence int64
	HasMore      bool
}

// ArchivedChat is the retention record of an ended session's observed
// history, stored as JSON. It is not exposed: an ended session's history is
// read by its known ID and sends to it are refused as SESSION_ARCHIVED.
type ArchivedChat struct {
	ID           string      `json:"id"`
	ArchitectKey string      `json:"architect_key"`
	SessionType  SessionType `json:"session_type"`
	ContextID    string      `json:"context_id"`
	Agent        string      `json:"agent"`
	Model        string      `json:"model,omitempty"`
	ProfileName  string      `json:"profile_name"`
	CreatedAt    time.Time   `json:"created_at"`
	EndedAt      time.Time   `json:"ended_at"`
	Lifecycle    string      `json:"lifecycle"`
	// InteractionMode of the ended session; empty on records archived before
	// terminal-session history was retained (all of those were chat).
	InteractionMode InteractionMode `json:"interaction_mode,omitempty"`
}

type ConversationRepository interface {
	ListSubmissions(context.Context, string, SequenceQuery) (SubmissionPage, error)
	ArchiveChat(context.Context, ArchivedChat) error
	IsArchivedChat(context.Context, string) (bool, error)
	// InsertSubmission records a request identity in its initial status, or
	// returns the existing record of the same request (created=false).
	InsertSubmission(ctx context.Context, sessionID string, req SubmissionRequest, origin, status, code, reason string) (sub Submission, created bool, err error)
	GetSubmission(context.Context, string, string) (Submission, error)
	PendingSubmission(context.Context, string) (*Submission, error)
	ReserveSubmission(context.Context, string) (*Submission, error)
	UpdateSubmission(context.Context, Submission) error
	// SettleDelivery concludes a still-delivering attempt; rejected applies
	// only while no hand-off was reserved. False: other evidence moved it on.
	SettleDelivery(ctx context.Context, sessionID, id, status, code, reason string) (bool, error)
	// ReconcileDeliveries settles attempts a previous daemon left delivering.
	ReconcileDeliveries(context.Context) error
	// PutConversationItems records items given in native record order; see
	// store.PutConversationItems for how display order (sequence) follows it.
	PutConversationItems(context.Context, string, []ConversationItem) error
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
	// OpenTerminalSubmissions lists delivering, unknown, accepted and running
	// submissions in admission order.
	OpenTerminalSubmissions(context.Context, string) ([]TerminalSubmission, error)
	// ReserveTerminalSubmission records the provider reference of a delivering
	// submission before any native hand-off; false if it is not an unreserved delivery.
	ReserveTerminalSubmission(ctx context.Context, sessionID, id, providerRef, terminalID string, handedAt time.Time) (bool, error)
	// TerminalProviderRefs maps provider references to submission ids.
	TerminalProviderRefs(context.Context, string) (map[string]string, error)
}

type ConversationService interface {
	SubmitMessage(context.Context, string, SubmissionRequest) (Submission, error)
	ReadConversation(context.Context, string, ConversationQuery) (ConversationPage, error)
}

// SubmissionIdentity makes an outbox reservation name its chat receipt before
// admission, closing the crash window between the two durable aggregates.
func SubmissionIdentity(sessionID, requestID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(sessionID+"\x00"+requestID)).String()
}
