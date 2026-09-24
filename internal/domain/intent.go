package domain

import (
	"context"
	"time"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the pure intent wire types from shared, same as session.go does for
// the session types.
type (
	IntentType    = sd.IntentType
	IntentOutcome = sd.IntentOutcome
	IntentPolicy  = sd.IntentPolicy
	IntentOrigin  = sd.IntentOrigin
	Intent        = sd.Intent

	IntentInputType      = sd.IntentInputType
	IntentInputOption    = sd.IntentInputOption
	IntentInputField     = sd.IntentInputField
	IntentInputValues    = sd.IntentInputValues
	IntentInputIssue     = sd.IntentInputIssue
	ApproveIntentRequest = sd.ApproveIntentRequest

	DeferredIntent       = sd.DeferredIntent
	DeferredIntentStatus = sd.DeferredIntentStatus
)

const (
	IntentTypeConcludeSession  = sd.IntentTypeConcludeSession
	IntentTypeCreateWorkTicket = sd.IntentTypeCreateWorkTicket
	IntentTypeExecuteAction    = sd.IntentTypeExecuteAction

	IntentOutcomeApproved     = sd.IntentOutcomeApproved
	IntentOutcomeAutoApproved = sd.IntentOutcomeAutoApproved
	IntentOutcomeDeniedByUser = sd.IntentOutcomeDeniedByUser
	IntentOutcomeAutoDenied   = sd.IntentOutcomeAutoDenied
	IntentOutcomeError        = sd.IntentOutcomeError

	IntentPolicyAutoAllow     = sd.IntentPolicyAutoAllow
	IntentPolicyWaitThenAllow = sd.IntentPolicyWaitThenAllow
	IntentPolicyWaitThenDeny  = sd.IntentPolicyWaitThenDeny
	IntentPolicyManual        = sd.IntentPolicyManual

	DeferredIntentPendingApproval = sd.DeferredIntentPendingApproval
	DeferredIntentDenied          = sd.DeferredIntentDenied
	DeferredIntentRunning         = sd.DeferredIntentRunning
	DeferredIntentCompleted       = sd.DeferredIntentCompleted
	DeferredIntentFailed          = sd.DeferredIntentFailed

	IntentInputText     = sd.IntentInputText
	IntentInputTextarea = sd.IntentInputTextarea
	IntentInputChoice   = sd.IntentInputChoice
	IntentInputBoolean  = sd.IntentInputBoolean

	MaxIntentInputFields  = sd.MaxIntentInputFields
	MaxIntentInputOptions = sd.MaxIntentInputOptions
)

// IntentResolution is what a blocked agent call returns once its intent
// resolves. It is daemon-local rather than shared because it is a service
// return type, not a wire shape, and because it is generic over the tool's
// result.
//
// The result is carried here rather than returned bare so that
// modify-before-approve stays a non-breaking addition later: the user's edited
// result simply comes back in Result with Outcome still approved.
type IntentResolution[R any] struct {
	IntentID string
	Outcome  IntentOutcome
	Result   R                 // zero unless Outcome.Approved()
	Inputs   IntentInputValues // the validated values the operation ran with; nil unless approved
	Reason   string            // denial reason, or error detail
}

// DeferredIntentRepository persists deferred intents, so an intent's outcome
// stays addressable by its id after the in-memory intent (and its captured
// Exec) is gone — after resolution, session teardown or a daemon restart.
//
// Transitions are compare-and-set on the current status: a record moves only
// from the status the caller expects, which keeps a terminal outcome from
// ever being overwritten.
type DeferredIntentRepository interface {
	CreateDeferredIntent(context.Context, DeferredIntent) error
	GetDeferredIntent(context.Context, string) (DeferredIntent, error)
	// TransitionDeferredIntent writes next (status, inputs, result, reason,
	// error, approved_at, ended_at) only when the stored status is from. It
	// returns a ConflictError when the record is in another status and a
	// NotFoundError when there is none.
	TransitionDeferredIntent(ctx context.Context, from DeferredIntentStatus, next DeferredIntent) error
	// FailOpenDeferredIntents fails every record still pending_approval or
	// running, with the respective reason, and returns how many it failed.
	FailOpenDeferredIntents(ctx context.Context, pendingReason, runningReason string, at time.Time) (int, error)
	// PruneDeferredIntents deletes terminal records that ended before cutoff.
	PruneDeferredIntents(ctx context.Context, cutoff time.Time) (int, error)
}
