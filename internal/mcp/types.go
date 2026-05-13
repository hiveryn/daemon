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

type TicketOutput = domain.Ticket
