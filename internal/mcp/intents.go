package mcp

import "fmt"

// Tools that ask the user before acting return an outcome, not just a result.
//
// The important rule: a DENIAL IS NOT AN ERROR. Returning a tool error for
// "the user said no" reads to an agent as "bad input, fix it and try again",
// which produces exactly the retry loop this catalog exists to prevent. Only a
// genuine failure (outcome=error) becomes a ToolError; every other outcome is a
// successful tool result carrying the verdict.

const (
	intentOutcomeApproved     = "approved"
	intentOutcomeAutoApproved = "auto_approved"
	intentOutcomeDeniedByUser = "denied_by_user"
	intentOutcomeAutoDenied   = "auto_denied"
	intentOutcomeError        = "error"
)

// IntentEnvelopeFields is embedded into each tool's concrete output struct
// rather than made generic: mcp.AddTool infers the output JSON schema from the
// Go type, and a generic instantiation is a schema-inference risk not worth
// taking here.
type IntentEnvelopeFields struct {
	Outcome  string `json:"outcome" jsonschema:"The verdict: approved, auto_approved, denied_by_user, auto_denied, or error. Check this before assuming the action happened."`
	Guidance string `json:"guidance" jsonschema:"Plain-language instruction for what to do about this outcome."`
	Reason   string `json:"reason,omitempty" jsonschema:"Why it was denied, or what failed."`
	IntentID string `json:"intent_id,omitempty" jsonschema:"The intent this call resolved to."`
}

// intentGuidance spells out the retry semantics per outcome so an agent does
// not have to infer them.
var intentGuidance = map[string]string{
	intentOutcomeApproved:     "The user approved this action. It has been performed; the result is in this response.",
	intentOutcomeAutoApproved: "The user did not respond in time and the action was auto-approved. It has been performed; the result is in this response.",
	intentOutcomeDeniedByUser: "The user denied this action. It was NOT performed. Do not retry it. Read `reason`, tell the user you did not proceed, and ask what they want instead.",
	intentOutcomeAutoDenied:   "The user did not respond and this action defaults to denied. It was NOT performed. Do not retry it. Tell the user and ask them to confirm.",
	intentOutcomeError:        "The action failed before completing. It may or may not have taken effect. Read `reason`. A single retry is reasonable if the reason looks transient.",
}

// intentEnvelope builds the agent-facing envelope, or a ToolError when the
// intent genuinely failed.
func intentEnvelope(res intentResolutionResponse) (IntentEnvelopeFields, error) {
	if res.Outcome == intentOutcomeError {
		reason := res.Reason
		if reason == "" {
			reason = "intent failed without a reason"
		}
		return IntentEnvelopeFields{}, newInternalError(reason)
	}
	guidance, ok := intentGuidance[res.Outcome]
	if !ok {
		return IntentEnvelopeFields{}, newInternalError(fmt.Sprintf("daemon returned unknown intent outcome %q", res.Outcome))
	}
	return IntentEnvelopeFields{
		Outcome:  res.Outcome,
		Guidance: guidance,
		Reason:   res.Reason,
		IntentID: res.IntentID,
	}, nil
}

// intentApproved reports whether the tool's side effect actually ran.
func intentApproved(outcome string) bool {
	return outcome == intentOutcomeApproved || outcome == intentOutcomeAutoApproved
}
