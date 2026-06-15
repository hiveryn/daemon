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
