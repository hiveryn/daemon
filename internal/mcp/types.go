package mcp

import "github.com/hiveryn/daemon/internal/domain"

type SessionType string

const (
	SessionTypeArchitect SessionType = "architect"
	SessionTypeTicket    SessionType = "ticket"
)

type ReadTicketInput struct {
	ID string `json:"id" jsonschema:"The ticket ID to read"`
}

type ListTicketsInput struct {
	Status string `json:"status" jsonschema:"Filter by ticket status (required). One of: backlog, progress, done"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum results to return (default 10)"`
}

type CreateWorkTicketInput struct {
	Title           string   `json:"title" jsonschema:"The ticket title (required)"`
	Repo            string   `json:"repo" jsonschema:"Primary repository key for this ticket (required)"`
	AdditionalRepos []string `json:"additional_repos,omitempty" jsonschema:"Additional repository keys in scope; unique and distinct from repo"`
	Body            string   `json:"body,omitempty" jsonschema:"The ticket body/description"`
	References      []string `json:"references,omitempty" jsonschema:"Optional list of same-board ticket IDs or absolute filesystem paths. Paths are read-only context and never expand writable repository scope."`
}

type EditTicketBodyInput struct {
	ID         string `json:"id" jsonschema:"The ticket ID to edit (required)"`
	OldString  string `json:"oldString" jsonschema:"Text to find in the ticket body (required)"`
	NewString  string `json:"newString" jsonschema:"Replacement text (required)"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace all occurrences (default false)"`
}

type UpdateTicketInput struct {
	ID              string    `json:"id" jsonschema:"The ticket ID to update (required)"`
	Title           string    `json:"title,omitempty" jsonschema:"New ticket title (optional)"`
	Repo            string    `json:"repo,omitempty" jsonschema:"New repo key (optional)"`
	AdditionalRepos *[]string `json:"additional_repos,omitempty" jsonschema:"Replacement additional repository keys (optional; pass an empty array to clear)"`
	References      []string  `json:"references,omitempty" jsonschema:"New list of same-board ticket IDs or absolute read-only filesystem paths (optional)"`
}

type DeleteTicketInput struct {
	ID string `json:"id" jsonschema:"The ticket ID to delete"`
}

type DeleteTicketOutput struct {
	Deleted bool `json:"deleted"`
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
	FollowUps       string             `json:"follow_ups,omitempty" jsonschema:"Candidate follow-up tickets for work discovered but not done, as Markdown (optional). Reference existing ticket IDs; create the tickets first with createWorkTicket."`
	OpenQuestions   string             `json:"open_questions,omitempty" jsonschema:"Unresolved questions or risks, as Markdown (optional)."`
	Commits         []domain.CommitRef `json:"commits,omitempty" jsonschema:"Commits produced, as {sha, repo} objects. Required (at least one) when outcome=completed; not required for exploratory or rejected."`
	RejectionReason string             `json:"rejection_reason,omitempty" jsonschema:"Required when outcome=rejected. Explain why no work was produced."`
}

type MoveTicketToDoneInput struct {
	ID              string             `json:"id" jsonschema:"The ticket ID to move to done (required)"`
	Body            string             `json:"body" jsonschema:"Conclusion summary — what was done and why this ticket is complete, or why it is being closed (required)."`
	Outcome         string             `json:"outcome" jsonschema:"One of: completed, exploratory, rejected (required). completed = shipped commits (commits required, at least one). exploratory = investigation/spike/test-run that produced no commits. rejected = ticket rejected outright (rejection_reason required)."`
	Commits         []domain.CommitRef `json:"commits,omitempty" jsonschema:"List of commits produced as objects with sha and repo. Required (at least one) when outcome=completed; not required for exploratory or rejected."`
	RejectionReason string             `json:"rejection_reason,omitempty" jsonschema:"Required when outcome=rejected. Explain why the ticket is being rejected."`
}

type MoveTicketToDoneOutput struct {
	Success  bool   `json:"success"`
	TicketID string `json:"ticket_id"`
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

type ReadConclusionInput struct {
	ConclusionID string `json:"conclusionId" jsonschema:"The conclusion ID to read (required)"`
}

type ReadConclusionOutput struct {
	StartedAt   string `json:"started_at"`
	ConcludedAt string `json:"concluded_at"`
	Agent       string `json:"agent,omitempty"`
	Body        string `json:"body"`
}

type ListConclusionsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum results to return (default 10)"`
}

type ListConclusionsOutput struct {
	Conclusions []ConclusionSummaryOutput `json:"conclusions"`
}

type ConclusionSummaryOutput struct {
	ID          string `json:"id"`
	ConcludedAt string `json:"concluded_at"`
}

type ListTicketsOutput struct {
	Tickets []TicketSummary `json:"tickets"`
}

type ReadTicketConclusionInput struct {
	TicketID string `json:"ticketId" jsonschema:"The ticket ID whose conclusion to read (required)"`
}

type TicketOutput = domain.Ticket

// CreateWorkTicketOutput carries the approval verdict alongside the ticket.
// Ticket is present only when the outcome is approved/auto_approved — always
// check Outcome before assuming the ticket exists.
type CreateWorkTicketOutput struct {
	IntentEnvelopeFields
	Ticket *TicketOutput `json:"ticket,omitempty" jsonschema:"The created ticket. Present only when outcome is approved or auto_approved."`
}

type TicketConclusionOutput = domain.TicketConclusion

type TicketSummary = domain.TicketSummary

// --- workspace inspection tools (architect only) ---

// CheckWorkspaceInput is empty on purpose. The workspace is resolved from the
// session's own architect identity, so there is nothing for the agent to pass
// and no way for it to inspect a workspace that is not its own.
type CheckWorkspaceInput struct{}

type CheckWorkspaceOutput = domain.WorkspaceReport

type DescribeArtifactInput struct {
	Kind string `json:"kind" jsonschema:"Which artifact to describe. One of: HIVERYN_YAML, PROJECT_OVERVIEW, PROJECT_STATE, ROADMAP_CURRENT, ARCHITECT_SYSTEM, WORKFLOW. Tickets and conclusions are not artifact kinds — use their own tools."`
}

type DescribeArtifactOutput = domain.ArtifactSchema
