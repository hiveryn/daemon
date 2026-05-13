package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerArchitectTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readTicket",
		Description: "Read full ticket details by ID.",
	}, s.handleReadTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "listTickets",
		Description: "List tickets filtered by status. Returns a filtered, capped slice of TicketSummary.",
	}, s.handleListTickets)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "createWorkTicket",
		Description: "Create a new work ticket in the backlog. Returns the created Ticket.",
	}, s.handleCreateWorkTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "deleteTicket",
		Description: "Delete a ticket by ID.",
	}, s.handleDeleteTicket)
}

func (s *Server) handleReadTicket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ReadTicketInput,
) (*mcp.CallToolResult, TicketOutput, error) {
	if strings.TrimSpace(input.ID) == "" {
		return nil, TicketOutput{}, newValidationError("id", "cannot be empty")
	}

	ticket, err := s.readTicket(ctx, input.ID)
	if err != nil {
		return nil, TicketOutput{}, err
	}

	return nil, ticket, nil
}

func (s *Server) handleListTickets(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ListTicketsInput,
) (*mcp.CallToolResult, ListTicketsOutput, error) {
	validStatuses := map[string]bool{"backlog": true, "progress": true, "done": true}
	if !validStatuses[input.Status] {
		return nil, ListTicketsOutput{}, newValidationError("status", "must be one of: backlog, progress, done")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}

	output, err := s.listTickets(ctx, input.Status, limit)
	if err != nil {
		return nil, ListTicketsOutput{}, err
	}

	return nil, output, nil
}

func (s *Server) handleCreateWorkTicket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input CreateWorkTicketInput,
) (*mcp.CallToolResult, TicketOutput, error) {
	if strings.TrimSpace(input.Title) == "" {
		return nil, TicketOutput{}, newValidationError("title", "is required")
	}

	ticket, err := s.createWorkTicket(ctx, input)
	if err != nil {
		return nil, TicketOutput{}, err
	}

	return nil, ticket, nil
}

func (s *Server) handleDeleteTicket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input DeleteTicketInput,
) (*mcp.CallToolResult, DeleteTicketOutput, error) {
	if strings.TrimSpace(input.ID) == "" {
		return nil, DeleteTicketOutput{}, newValidationError("id", "cannot be empty")
	}

	output, err := s.deleteTicket(ctx, input.ID)
	if err != nil {
		return nil, DeleteTicketOutput{}, err
	}

	return nil, output, nil
}
