package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerArchitectTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "Conclude the architect session. Provide a summary of topics covered, decisions made, tickets created, and any follow-up work.",
	}, s.handleArchitectConcludeSession)

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
		Name:        "editTicketBody",
		Description: "Perform exact string replacements in a ticket body.",
	}, s.handleEditTicketBody)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "updateTicket",
		Description: "Update ticket metadata fields (title, repo, references). Only present fields are updated; omitted fields are left unchanged.",
	}, s.handleUpdateTicket)

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

func (s *Server) handleEditTicketBody(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input EditTicketBodyInput,
) (*mcp.CallToolResult, TicketOutput, error) {
	if strings.TrimSpace(input.ID) == "" {
		return nil, TicketOutput{}, newValidationError("id", "cannot be empty")
	}
	if input.OldString == "" {
		return nil, TicketOutput{}, newValidationError("oldString", "cannot be empty")
	}

	ticket, err := s.editTicketBody(ctx, input.ID, input.OldString, input.NewString, input.ReplaceAll)
	if err != nil {
		return nil, TicketOutput{}, err
	}

	return nil, ticket, nil
}

func (s *Server) handleUpdateTicket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input UpdateTicketInput,
) (*mcp.CallToolResult, TicketOutput, error) {
	if strings.TrimSpace(input.ID) == "" {
		return nil, TicketOutput{}, newValidationError("id", "cannot be empty")
	}

	ticket, err := s.updateTicket(ctx, input)
	if err != nil {
		return nil, TicketOutput{}, err
	}

	return nil, ticket, nil
}

func (s *Server) handleConcludeSession(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ConcludeSessionInput,
) (*mcp.CallToolResult, ConcludeSessionOutput, error) {
	if strings.TrimSpace(input.Body) == "" {
		return nil, ConcludeSessionOutput{}, newValidationError("body", "is required")
	}

	if s.sessionID == "" {
		return nil, ConcludeSessionOutput{}, newInternalError("HIVERYN_SESSION_ID not set")
	}

	output, err := s.concludeSession(ctx, input)
	if err != nil {
		return nil, ConcludeSessionOutput{}, err
	}

	return nil, output, nil
}

func (s *Server) handleArchitectConcludeSession(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ArchitectConcludeSessionInput,
) (*mcp.CallToolResult, ConcludeSessionOutput, error) {
	if strings.TrimSpace(input.Body) == "" {
		return nil, ConcludeSessionOutput{}, newValidationError("body", "is required")
	}

	if s.sessionID == "" {
		return nil, ConcludeSessionOutput{}, newInternalError("HIVERYN_SESSION_ID not set")
	}

	output, err := s.concludeSession(ctx, ConcludeSessionInput{Body: input.Body})
	if err != nil {
		return nil, ConcludeSessionOutput{}, err
	}

	return nil, output, nil
}
