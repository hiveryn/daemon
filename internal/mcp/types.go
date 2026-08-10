package mcp

import "github.com/hiveryn/daemon/internal/domain"

type SessionType string

const (
	SessionTypeArchitect SessionType = "architect"
	SessionTypeTicket    SessionType = "ticket"
	SessionTypeFreeform  SessionType = "freeform"
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
	References      []string `json:"references,omitempty" jsonschema:"Optional list of ticket IDs to reference"`
}

type ListAgentProfilesInput struct{}

type AgentProfileChoice struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"`
	Yolo  bool   `json:"yolo,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

type ListAgentProfilesOutput struct {
	AgentProfiles []AgentProfileChoice `json:"agent_profiles"`
}

type SpawnTicketSessionInput struct {
	TicketID string `json:"ticket_id" jsonschema:"Backlog ticket ID to spawn (required)"`
	Profile  string `json:"profile" jsonschema:"Explicit configured profile key agreed with the user (required; never inferred or defaulted)"`
}

type SpawnTicketSessionOutput struct {
	IntentEnvelopeFields
	SessionID string `json:"session_id,omitempty" jsonschema:"Created Hiveryn session ID. Present only when outcome is approved."`
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
	References      []string  `json:"references,omitempty" jsonschema:"New references list (optional)"`
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
	ConfigChanges  string `json:"config_changes,omitempty" jsonschema:"Edits to hiveryn.yaml repos, kickoffs, or prompts, as Markdown (optional)."`
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

type FreeformConcludeSessionInput struct {
	Summary         string             `json:"summary" jsonschema:"One or two line TL;DR of the session (required)."`
	Findings        string             `json:"findings" jsonschema:"The substance of the session, in markdown (required)."`
	Recommendations string             `json:"recommendations" jsonschema:"Recommended next steps, as Markdown (required; write \"None\" if there are none)."`
	OpenQuestions   string             `json:"open_questions" jsonschema:"Unresolved questions, as Markdown (required; write \"None\" if there are none)."`
	Commits         []domain.CommitRef `json:"commits,omitempty" jsonschema:"Commits produced, as {sha, repo} objects (optional)."`
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

type PreviewInBrowserTabInput struct {
	Target string `json:"target" jsonschema:"The file://, absolute path, http://localhost:*, or https:// URL to open (required)"`
	TabID  string `json:"tab_id,omitempty" jsonschema:"Existing tab id to navigate in place; omit to open a new tab"`
}

type PreviewInBrowserTabOutput struct {
	TabID  string `json:"tab_id"`
	Target string `json:"target"`
}

type GetBrowserTabsOutput struct {
	Tabs []BrowserTabSummary `json:"tabs"`
}

type BrowserTabSummary struct {
	TabID  string `json:"tab_id"`
	Target string `json:"target"`
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

// --- hiveryn.yaml config tools ---

// ArchitectConfigDoc is the declarative, whole-document architect config. The
// same shape is returned by readArchitectConfig and accepted by
// updateArchitectConfig, so you can read → edit → write it back without
// reshaping. Paths are stored verbatim.
type ArchitectConfigDoc struct {
	Repos   map[string]string   `json:"repos" jsonschema:"Repo key → path. Paths may be absolute or ~-prefixed and are stored verbatim."`
	Prompts ArchitectPromptsDoc `json:"prompts" jsonschema:"Prompt path wiring; all fields optional (omit to use the embedded defaults)."`
}

type ArchitectPromptsDoc struct {
	Architect ArchitectPromptPathsDoc `json:"architect" jsonschema:"Architect system/kickoff prompt paths — single, not repo-scoped."`
	Ticket    TicketKickoffsDoc       `json:"ticket" jsonschema:"Ticket kickoff prompt entries (repo-scoped)."`
}

type ArchitectPromptPathsDoc struct {
	System  string `json:"system,omitempty" jsonschema:"Path to the architect system prompt; empty = embedded default."`
	Kickoff string `json:"kickoff,omitempty" jsonschema:"Path to the architect kickoff prompt; empty = embedded default."`
}

type TicketKickoffsDoc struct {
	Kickoffs []TicketKickoffDoc `json:"kickoffs" jsonschema:"Ticket kickoff entries. At most one default (empty-repos) entry; each repo may appear in at most one entry."`
}

type TicketKickoffDoc struct {
	Path  string   `json:"path" jsonschema:"Path to the kickoff prompt file (required)."`
	Repos []string `json:"repos" jsonschema:"Repo keys this entry applies to; empty = the default entry used by repos without their own."`
}

// ResolvedConfig mirrors ArchitectConfigDoc with absolute paths and, per wired
// prompt, whether the file exists. Read-only — do not send it back.
type ResolvedConfig struct {
	Repos   map[string]string `json:"repos"`
	Prompts ResolvedPrompts   `json:"prompts"`
}

type ResolvedPrompts struct {
	Architect ResolvedArchitectPrompts `json:"architect"`
	Ticket    ResolvedTicket           `json:"ticket"`
}

type ResolvedArchitectPrompts struct {
	System  *ResolvedPath `json:"system"`
	Kickoff *ResolvedPath `json:"kickoff"`
}

type ResolvedTicket struct {
	Kickoffs []ResolvedKickoff `json:"kickoffs"`
}

type ResolvedPath struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type ResolvedKickoff struct {
	Path   string   `json:"path"`
	Repos  []string `json:"repos"`
	Exists bool     `json:"exists"`
}

type ReadArchitectConfigOutput struct {
	Config   ArchitectConfigDoc `json:"config"`
	Resolved ResolvedConfig     `json:"resolved"`
	Warnings []string           `json:"warnings"`
	Version  string             `json:"version"`
}

type UpdateArchitectConfigInput struct {
	Config  ArchitectConfigDoc `json:"config" jsonschema:"Full config document to write (declarative replace — anything omitted is dropped). Send back the document from readArchitectConfig with your edits applied."`
	Version string             `json:"version" jsonschema:"Opaque version token from your last readArchitectConfig (required). Rejected if it no longer matches the on-disk config; re-read and retry."`
}

type UpdateArchitectConfigOutput struct {
	Config   ArchitectConfigDoc `json:"config"`
	Resolved ResolvedConfig     `json:"resolved"`
	Created  []string           `json:"created"`
	Version  string             `json:"version"`
}

type ReadDefaultPromptInput struct {
	Kind string `json:"kind" jsonschema:"Which default template to fetch. One of: architect-system, architect-kickoff, ticket-kickoff."`
}

type ReadDefaultPromptOutput struct {
	Template  string                `json:"template"`
	Variables []PromptVariableEntry `json:"variables"`
}

type PromptVariableEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
