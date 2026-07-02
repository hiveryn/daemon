package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) handleReadArchitectConfig(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, ReadArchitectConfigOutput, error) {
	output, err := s.readArchitectConfig(ctx)
	if err != nil {
		return nil, ReadArchitectConfigOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleUpdateArchitectConfig(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input UpdateArchitectConfigInput,
) (*mcp.CallToolResult, UpdateArchitectConfigOutput, error) {
	if strings.TrimSpace(input.Version) == "" {
		return nil, UpdateArchitectConfigOutput{}, newValidationError("version", "is required; read the config first")
	}
	output, err := s.updateArchitectConfig(ctx, input)
	if err != nil {
		return nil, UpdateArchitectConfigOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleReadDefaultPrompt(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ReadDefaultPromptInput,
) (*mcp.CallToolResult, ReadDefaultPromptOutput, error) {
	kind := strings.TrimSpace(input.Kind)
	switch kind {
	case "architect-system", "architect-kickoff", "ticket-kickoff":
	default:
		return nil, ReadDefaultPromptOutput{}, newValidationError("kind", "must be one of: architect-system, architect-kickoff, ticket-kickoff")
	}
	output, err := s.readDefaultPrompt(ctx, kind)
	if err != nil {
		return nil, ReadDefaultPromptOutput{}, err
	}
	return nil, output, nil
}
