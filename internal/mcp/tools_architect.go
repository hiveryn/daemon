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
