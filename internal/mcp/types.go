package mcp

import "github.com/hiveryn/daemon/internal/domain"

type SessionType string

const (
	SessionTypeArchitect SessionType = "architect"
	SessionTypeWork      SessionType = "work"
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
	Body            string   `json:"body" jsonschema:"Session conclusion summary — outcome, files changed, and follow-up work or blockers (required)."`
	Commits         []string `json:"commits,omitempty" jsonschema:"List of commit SHAs produced during this session. Required for work ticket sessions unless rejected=true."`
	Rejected        bool     `json:"rejected,omitempty" jsonschema:"Set to true if the session produced no work and should be marked as rejected. Work ticket sessions only."`
	RejectionReason string   `json:"rejection_reason,omitempty" jsonschema:"Required when rejected=true. Explain why the session produced no commits. Work ticket sessions only."`
}

type ArchitectConcludeSessionInput struct {
	Body string `json:"body" jsonschema:"Session conclusion summary — topics covered, decisions made, tickets created, follow-up work, and blockers (required)."`
}

type ConcludeSessionOutput struct {
	Success   bool   `json:"success"`
	SessionID string `json:"session_id"`
	TicketID  string `json:"ticket_id,omitempty"`
}

type ListTicketsOutput struct {
	Tickets []TicketSummary `json:"tickets"`
}

type TicketOutput = domain.Ticket

type TicketSummary = domain.TicketSummary
