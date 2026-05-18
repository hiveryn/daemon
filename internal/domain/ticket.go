package domain

import (
	"context"
	"time"
)

type TicketStatus string

const (
	TicketStatusBacklog  TicketStatus = "backlog"
	TicketStatusProgress TicketStatus = "progress"
	TicketStatusDone     TicketStatus = "done"
)

func (s TicketStatus) Valid() bool {
	switch s {
	case TicketStatusBacklog, TicketStatusProgress, TicketStatusDone:
		return true
	default:
		return false
	}
}

type TicketWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TicketSummary struct {
	ID            string          `json:"id"`
	Status        TicketStatus    `json:"status"`
	Title         string          `json:"title"`
	Repo          string          `json:"repo,omitempty"`
	Created       *time.Time      `json:"created,omitempty"`
	Updated       *time.Time      `json:"updated,omitempty"`
	References    []string        `json:"references"`
	HasConclusion bool            `json:"has_conclusion"`
	Warnings      []TicketWarning `json:"warnings"`
}

type TicketConclusion struct {
	StartedAt       time.Time   `json:"started_at"`
	ConcludedAt     time.Time   `json:"concluded_at"`
	Agent           string      `json:"agent,omitempty"`
	Profile         string      `json:"profile,omitempty"`
	Rejected        bool        `json:"rejected"`
	RejectionReason string      `json:"rejection_reason,omitempty"`
	Commits         []CommitRef `json:"commits"`
	Body            string      `json:"body"`
}

type Ticket struct {
	TicketSummary
	Body       string            `json:"body"`
	Conclusion *TicketConclusion `json:"conclusion"`
}

type TicketBoard struct {
	Backlog  []TicketSummary `json:"backlog"`
	Progress []TicketSummary `json:"progress"`
	Done     []TicketSummary `json:"done"`
}

type CreateTicketParams struct {
	Title      string
	Repo       string
	Body       string
	References []string
	Now        time.Time
}

type EditTicketParams struct {
	OldString  string
	NewString  string
	ReplaceAll bool
	Now        time.Time
}

type UpdateTicketMetadataParams struct {
	Title      *string
	Repo       *string
	References *[]string
	Now        time.Time
}

type MoveTicketParams struct {
	To TicketStatus
}

type TicketService interface {
	ListTickets(context.Context, string) (TicketBoard, error)
	GetTicket(context.Context, string, string) (Ticket, error)
	CreateTicket(context.Context, string, CreateTicketParams) (Ticket, error)
	EditTicket(context.Context, string, string, EditTicketParams) (Ticket, error)
	UpdateTicketMetadata(context.Context, string, string, UpdateTicketMetadataParams) (Ticket, error)
	DeleteTicket(context.Context, string, string) error
	MoveTicket(context.Context, string, string, MoveTicketParams) (Ticket, error)
	ConcludeTicket(context.Context, string, string, TicketConclusion) (Ticket, error)
}
