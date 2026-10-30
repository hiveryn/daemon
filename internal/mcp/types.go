package mcp

import "github.com/hiveryn/daemon/internal/domain"

type SessionType string

const (
	SessionTypeArchitect SessionType = "architect"
	SessionTypeTicket    SessionType = "ticket"
	SessionTypeCompanion SessionType = "companion"
)

type ReadTicketInput struct {
	ID string `json:"id" jsonschema:"The ticket ID to read"`
}

// Conclude tools take discrete structured fields; the daemon renders them into
// the canonical conclusion.md body (fixed section order). Fields without
// ,omitempty are required by the JSON schema and re-enforced by the daemon.
// Every presentational section is a Markdown string authored by the agent
// (bullets/prose as text) — no conclude section is a required array, so none can
// be dropped by the MCP client. Required sections with nothing to report take
// the literal Markdown "None". Only commits stays a structured array, because it
// is persisted and read back as data, not merely rendered.

type ArchitectConcludeSessionInput struct {
	Summary        string `json:"summary" jsonschema:"One or two line TL;DR of the session (required)."`
	Narrative      string `json:"narrative" jsonschema:"What happened this session, in markdown (required)."`
	TicketsTouched string `json:"tickets_touched,omitempty" jsonschema:"Board delta — tickets created, updated, or deleted this session, as Markdown (optional)."`
	Decisions      string `json:"decisions,omitempty" jsonschema:"Key decisions locked this session, as Markdown (optional)."`
	ConfigChanges  string `json:"config_changes,omitempty" jsonschema:"Edits to hiveryn.yaml, project documents or workflows, as Markdown (optional)."`
	UserPriorities string `json:"user_priorities,omitempty" jsonschema:"User priorities expressed this session, as Markdown (optional)."`
	OpenQuestions  string `json:"open_questions,omitempty" jsonschema:"Unresolved questions or risks, as Markdown (optional)."`
	NextSteps      string `json:"next_steps" jsonschema:"Concrete next steps to resume from, as Markdown (required; write \"None\" if there are none)."`
}

type TicketConcludeSessionInput struct {
	Summary         string             `json:"summary" jsonschema:"One or two line TL;DR of the outcome (required)."`
	Outcome         string             `json:"outcome" jsonschema:"One of: completed, exploratory, rejected (required). completed = shipped commits (commits required, at least one). exploratory = investigation/spike/test-run that produced no commits (commits may be empty; implementation is still required as your findings writeup). rejected = ticket rejected outright (rejection_reason required)."`
	Implementation  string             `json:"implementation,omitempty" jsonschema:"What was built (completed) or what was found (exploratory), in markdown. Required unless outcome=rejected."`
	Deviations      string             `json:"deviations,omitempty" jsonschema:"Where the build diverged from the ticket spec, and why, as Markdown (optional)."`
	Verification    string             `json:"verification,omitempty" jsonschema:"Tests/lints/typecheck status and how it was checked (optional)."`
	FollowUps       string             `json:"follow_ups,omitempty" jsonschema:"Candidate follow-up tickets for work discovered but not done, as Markdown (optional). Describe proposed follow-up outcomes for the architect to assess; workers cannot create tickets."`
	OpenQuestions   string             `json:"open_questions,omitempty" jsonschema:"Unresolved questions or risks, as Markdown (optional)."`
	Commits         []domain.CommitRef `json:"commits,omitempty" jsonschema:"Commits produced, as {sha, repo} objects. Required (at least one) when outcome=completed; not required for exploratory or rejected."`
	RejectionReason string             `json:"rejection_reason,omitempty" jsonschema:"Required when outcome=rejected. Explain why no work was produced."`
}

// ConcludeSessionOutput carries the approval verdict alongside the conclusion
// result. Session is present only when the outcome is approved/auto_approved —
// a denied conclusion means the session is still running.
type ConcludeSessionOutput struct {
	IntentEnvelopeFields
	Session *ConcludeSessionResult `json:"session,omitempty" jsonschema:"The concluded session. Present only when outcome is approved or auto_approved."`
}

type ConcludeSessionResult struct {
	SessionID string `json:"session_id"`
	TicketID  string `json:"ticket_id,omitempty"`
}

type TicketOutput = domain.Ticket

type TicketConclusionOutput = domain.TicketConclusion

type TicketSummary = domain.TicketSummary
