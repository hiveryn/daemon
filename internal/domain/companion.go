package domain

import (
	"context"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the companion and action request contract from shared.
type (
	Companion                        = sd.Companion
	CompanionList                    = sd.CompanionList
	CompanionStatus                  = sd.CompanionStatus
	CompanionShutdown                = sd.CompanionShutdown
	CompanionShutdownStatus          = sd.CompanionShutdownStatus
	CompanionMessageRequest          = sd.CompanionMessageRequest
	CompanionReplyRequest            = sd.CompanionReplyRequest
	CompanionMessageResult           = sd.CompanionMessageResult
	CompanionShutdownRequest         = sd.CompanionShutdownRequest
	CompleteCompanionShutdownRequest = sd.CompleteCompanionShutdownRequest
	CompanionShutdownResult          = sd.CompanionShutdownResult

	ActionRequest         = sd.ActionRequest
	ActionOrigin          = sd.ActionOrigin
	ActionRequestStatus   = sd.ActionRequestStatus
	RequestActionRequest  = sd.RequestActionRequest
	RequestActionResult   = sd.RequestActionResult
	ActionDecision        = sd.ActionDecision
	ActionDecisionRequest = sd.ActionDecisionRequest
)

const (
	CompanionStarting          = sd.CompanionStarting
	CompanionRunning           = sd.CompanionRunning
	CompanionShutdownRequested = sd.CompanionShutdownRequested
	CompanionStopping          = sd.CompanionStopping
	CompanionStopped           = sd.CompanionStopped
	CompanionStopFailed        = sd.CompanionStopFailed
	CompanionStartFailed       = sd.CompanionStartFailed
	CompanionFailed            = sd.CompanionFailed

	ShutdownRequested  = sd.ShutdownRequested
	ShutdownCompleting = sd.ShutdownCompleting
	ShutdownStopped    = sd.ShutdownStopped
	ShutdownStopFailed = sd.ShutdownStopFailed

	MaxCompanionMessageLength = sd.MaxCompanionMessageLength
	MaxCompanionHandoffLength = sd.MaxCompanionHandoffLength
	CompanionAccepted         = sd.CompanionAccepted

	ActionPending     = sd.ActionPending
	ActionAccepted    = sd.ActionAccepted
	ActionDenied      = sd.ActionDenied
	ActionReplied     = sd.ActionReplied
	ActionExpired     = sd.ActionExpired
	ActionCancelled   = sd.ActionCancelled
	ActionInterrupted = sd.ActionInterrupted

	ActionDecisionAccept = sd.ActionDecisionAccept
	ActionDecisionDeny   = sd.ActionDecisionDeny
	ActionDecisionReply  = sd.ActionDecisionReply

	ActionRequestTimeout                = sd.ActionRequestTimeout
	SessionEventTypeActionRequest       = sd.SessionEventTypeActionRequest
	ActionRequestTimeoutCode            = sd.ActionRequestTimeoutCode
	ActionRequestTimeoutText            = sd.ActionRequestTimeoutText
	ActionRequestCancelledCode          = sd.ActionRequestCancelledCode
	ActionRequestInterruptedCode        = sd.ActionRequestInterruptedCode
	ActionRequestNotificationFailedCode = sd.ActionRequestNotificationFailedCode
)

var NormalizeCompanionText = sd.NormalizeCompanionText

// CompanionRepository persists companion records and shutdown requests.
// Records are keyed by (architect session, name) and removed with the
// architect session; shutdown requests by their architect-issued request id.
type CompanionRepository interface {
	CreateCompanion(context.Context, Companion) error
	GetCompanion(ctx context.Context, architectSessionID, name string) (Companion, error)
	GetCompanionBySession(ctx context.Context, sessionID string) (Companion, error)
	ListCompanions(ctx context.Context, architectSessionID string) ([]Companion, error)
	ListAllCompanions(context.Context) ([]Companion, error)
	// UpdateCompanion replaces status, session id, agent, error and updated_at.
	UpdateCompanion(context.Context, Companion) error
	// CreateShutdown records a request and moves the companion to
	// shutdown_requested in one transaction. The companion must still be
	// running (else ConflictError); a duplicate request id is a ConflictError.
	CreateShutdown(context.Context, CompanionShutdown) error
	GetShutdown(ctx context.Context, requestID string) (CompanionShutdown, error)
	GetShutdownBySession(ctx context.Context, companionSessionID string) (CompanionShutdown, error)
	UpdateShutdown(context.Context, CompanionShutdown) error
}

// CompanionService is the companion surface of the session service: the
// agent-facing tools (addressed by the calling session, which the daemon
// authorizes), the desktop's listing and action decision.
type CompanionService interface {
	ListArchitectCompanions(ctx context.Context, architectKey string) (CompanionList, error)
	SendMessageToArchitect(ctx context.Context, companionSessionID string, req CompanionMessageRequest) (CompanionMessageResult, error)
	ReplyToArchitectMessage(ctx context.Context, companionSessionID string, req CompanionReplyRequest) (CompanionMessageResult, error)
	SendMessageToCompanion(ctx context.Context, architectSessionID, companion string, req CompanionMessageRequest) (CompanionMessageResult, error)
	ReplyToCompanionMessage(ctx context.Context, architectSessionID string, req CompanionReplyRequest) (CompanionMessageResult, error)
	RequestCompanionShutdown(ctx context.Context, architectSessionID string, req CompanionShutdownRequest) (CompanionShutdownResult, error)
	CompleteCompanionShutdown(ctx context.Context, companionSessionID string, req CompleteCompanionShutdownRequest) (CompanionShutdownResult, error)
	// CompanionShutdownAcknowledged: the accepted completion response was
	// written and flushed; the companion's stop may proceed.
	CompanionShutdownAcknowledged(companionSessionID string)
	RequestAction(ctx context.Context, sessionID string, req RequestActionRequest) (RequestActionResult, error)
	DecideAction(ctx context.Context, sessionID, actionID string, req ActionDecisionRequest) (ActionRequest, error)
	ListActions(ctx context.Context, sessionID string) ([]ActionRequest, error)
	// DiscardCompanion is the user's retry of an unconfirmed termination
	// (stop_failed) or removal of a companion whose process ended (failed).
	DiscardCompanion(ctx context.Context, sessionID string) error
}
