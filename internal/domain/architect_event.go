package domain

import "time"

type ArchitectEvent struct {
	Type         string    `json:"type"`
	ArchitectKey string    `json:"architect_key"`
	Reason       string    `json:"reason"`
	TicketID     string    `json:"ticket_id"`
	At           time.Time `json:"at"`
}
