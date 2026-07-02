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
	Title      string   `json:"title" jsonschema:"The ticket title (required)"`
	Repo       string   `json:"repo,omitempty" jsonschema:"Stable repo key for this ticket"`
	Body       string   `json:"body,omitempty" jsonschema:"The ticket body/description"`
	References []string `json:"references,omitempty" jsonschema:"Optional list of ticket IDs to reference"`
}

type EditTicketBodyInput struct {
	ID         string `json:"id" jsonschema:"The ticket ID to edit (required)"`
	OldString  string `json:"oldString" jsonschema:"Text to find in the ticket body (required)"`
	NewString  string `json:"newString" jsonschema:"Replacement text (required)"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace all occurrences (default false)"`
}

type UpdateTicketInput struct {
	ID         string   `json:"id" jsonschema:"The ticket ID to update (required)"`
	Title      string   `json:"title,omitempty" jsonschema:"New ticket title (optional)"`
	Repo       string   `json:"repo,omitempty" jsonschema:"New repo key (optional)"`
	References []string `json:"references,omitempty" jsonschema:"New references list (optional)"`
}

type DeleteTicketInput struct {
	ID string `json:"id" jsonschema:"The ticket ID to delete"`
}

type DeleteTicketOutput struct {
	Deleted bool `json:"deleted"`
}

type ConcludeSessionInput struct {
	Body            string             `json:"body" jsonschema:"Session conclusion summary — outcome, files changed, and follow-up work or blockers (required)."`
	Commits         []domain.CommitRef `json:"commits,omitempty" jsonschema:"List of commits produced during this session as objects with sha and repo. Required for work ticket sessions unless rejected=true."`
	Rejected        bool               `json:"rejected,omitempty" jsonschema:"Set to true if the session produced no work and should be marked as rejected. Work ticket sessions only."`
	RejectionReason string             `json:"rejection_reason,omitempty" jsonschema:"Required when rejected=true. Explain why the session produced no commits. Work ticket sessions only."`
}

type ArchitectConcludeSessionInput struct {
	Body string `json:"body" jsonschema:"Session conclusion summary — topics covered, decisions made, tickets created, follow-up work, and blockers (required)."`
}

type MoveTicketToDoneInput struct {
	ID              string             `json:"id" jsonschema:"The ticket ID to move to done (required)"`
	Body            string             `json:"body" jsonschema:"Conclusion summary — what was done and why this ticket is complete, or why it is being closed (required)."`
	Commits         []domain.CommitRef `json:"commits,omitempty" jsonschema:"List of commits produced as objects with sha and repo (optional — the architect may have no commits to report)."`
	Rejected        bool               `json:"rejected,omitempty" jsonschema:"Set to true to close the ticket as rejected instead of completed."`
	RejectionReason string             `json:"rejection_reason,omitempty" jsonschema:"Required when rejected=true. Explain why the ticket is being rejected."`
}

type MoveTicketToDoneOutput struct {
	Success  bool   `json:"success"`
	TicketID string `json:"ticket_id"`
}

type ConcludeSessionOutput struct {
	Success   bool   `json:"success"`
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
