package domain

import sd "github.com/hiveryn/shared/domain"

// Re-export the pure intent wire types from shared, same as session.go does for
// the session types.
type (
	IntentType    = sd.IntentType
	IntentOutcome = sd.IntentOutcome
	IntentPolicy  = sd.IntentPolicy
	IntentOrigin  = sd.IntentOrigin
	Intent        = sd.Intent
)

const (
	IntentTypeConcludeSession  = sd.IntentTypeConcludeSession
	IntentTypeCreateWorkTicket = sd.IntentTypeCreateWorkTicket

	IntentOutcomeApproved     = sd.IntentOutcomeApproved
	IntentOutcomeAutoApproved = sd.IntentOutcomeAutoApproved
	IntentOutcomeDeniedByUser = sd.IntentOutcomeDeniedByUser
	IntentOutcomeAutoDenied   = sd.IntentOutcomeAutoDenied
	IntentOutcomeError        = sd.IntentOutcomeError

	IntentPolicyAutoAllow     = sd.IntentPolicyAutoAllow
	IntentPolicyWaitThenAllow = sd.IntentPolicyWaitThenAllow
	IntentPolicyWaitThenDeny  = sd.IntentPolicyWaitThenDeny
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
	Result   R      // zero unless Outcome.Approved()
	Reason   string // denial reason, or error detail
}
