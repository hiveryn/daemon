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

type ListTicketsOutput struct {
	Tickets []TicketSummary `json:"tickets"`
}

type TicketOutput = domain.Ticket

type TicketSummary = domain.TicketSummary
