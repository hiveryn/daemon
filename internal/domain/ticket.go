package domain

import (
	"context"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export pure ticket data types from shared. TicketService interface and
// any daemon-specific service contracts remain local.
type (
	TicketStatus               = sd.TicketStatus
	TicketOutcome              = sd.TicketOutcome
	TicketWarning              = sd.TicketWarning
	TicketReferenceType        = sd.TicketReferenceType
	PathReferenceKind          = sd.PathReferenceKind
	TicketReference            = sd.TicketReference
	TicketSummary              = sd.TicketSummary
	TicketConclusion           = sd.TicketConclusion
	Ticket                     = sd.Ticket
	TicketBoard                = sd.TicketBoard
	CreateTicketParams         = sd.CreateTicketParams
	EditTicketParams           = sd.EditTicketParams
	UpdateTicketMetadataParams = sd.UpdateTicketMetadataParams
	MoveTicketParams           = sd.MoveTicketParams
)

const (
	TicketReferenceTicket  = sd.TicketReferenceTicket
	TicketReferencePath    = sd.TicketReferencePath
	PathReferenceFile      = sd.PathReferenceFile
	PathReferenceDirectory = sd.PathReferenceDirectory
)

const (
	TicketStatusBacklog  = sd.TicketStatusBacklog
	TicketStatusProgress = sd.TicketStatusProgress
	TicketStatusDone     = sd.TicketStatusDone
)

const (
	TicketOutcomeCompleted   = sd.TicketOutcomeCompleted
	TicketOutcomeExploratory = sd.TicketOutcomeExploratory
	TicketOutcomeRejected    = sd.TicketOutcomeRejected
)

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
