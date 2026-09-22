package domain

import (
	sd "github.com/hiveryn/shared/domain"
)

// Re-export the architect event stream contract from shared. All JSON tags,
// field names, and underlying types are identical, so call sites read
// `domain.ArchitectEvent` unchanged.
type (
	ArchitectEvent       = sd.ArchitectEvent
	ArchitectEventReason = sd.ArchitectEventReason
)

const (
	ArchitectEventType = sd.ArchitectEventType

	ArchitectEventTicketCreated   = sd.ArchitectEventTicketCreated
	ArchitectEventTicketUpdated   = sd.ArchitectEventTicketUpdated
	ArchitectEventTicketMoved     = sd.ArchitectEventTicketMoved
	ArchitectEventTicketDeleted   = sd.ArchitectEventTicketDeleted
	ArchitectEventTicketConcluded = sd.ArchitectEventTicketConcluded
	ArchitectEventSessionStarted  = sd.ArchitectEventSessionStarted
	ArchitectEventSessionEnded    = sd.ArchitectEventSessionEnded
)
