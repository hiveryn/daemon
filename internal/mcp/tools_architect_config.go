package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) handleListRepos(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, ListReposOutput, error) {
	output, err := s.listRepos(ctx)
	if err != nil {
		return nil, ListReposOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleListKickoffs(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, ListKickoffsOutput, error) {
	output, err := s.listKickoffs(ctx)
	if err != nil {
		return nil, ListKickoffsOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleGetArchitectPrompts(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, ArchitectPromptsOutput, error) {
	output, err := s.getArchitectPrompts(ctx)
	if err != nil {
		return nil, ArchitectPromptsOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleDescribePromptSchema(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input DescribePromptSchemaInput,
) (*mcp.CallToolResult, PromptSchemaOutput, error) {
	kind := strings.TrimSpace(input.Kind)
	if kind != "architect" && kind != "ticket" {
		return nil, PromptSchemaOutput{}, newValidationError("kind", "must be one of: architect, ticket")
	}
	output, err := s.describePromptSchema(ctx, kind)
	if err != nil {
		return nil, PromptSchemaOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleAddRepo(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input AddRepoInput,
) (*mcp.CallToolResult, RepoConfigEntry, error) {
	if strings.TrimSpace(input.Key) == "" {
		return nil, RepoConfigEntry{}, newValidationError("key", "is required")
	}
	if strings.TrimSpace(input.Path) == "" {
		return nil, RepoConfigEntry{}, newValidationError("path", "is required")
	}
	output, err := s.addRepo(ctx, input)
	if err != nil {
		return nil, RepoConfigEntry{}, err
	}
	return nil, output, nil
}

func (s *Server) handleRemoveRepo(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input RemoveRepoInput,
) (*mcp.CallToolResult, RemoveRepoOutput, error) {
	if strings.TrimSpace(input.Key) == "" {
		return nil, RemoveRepoOutput{}, newValidationError("key", "is required")
	}
	output, err := s.removeRepo(ctx, input.Key)
	if err != nil {
		return nil, RemoveRepoOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleAddKickoff(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input AddKickoffInput,
) (*mcp.CallToolResult, AddKickoffOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, AddKickoffOutput{}, newValidationError("path", "is required")
	}
	output, err := s.addKickoff(ctx, input)
	if err != nil {
		return nil, AddKickoffOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleUpdateKickoff(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input UpdateKickoffInput,
) (*mcp.CallToolResult, UpdateKickoffOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, UpdateKickoffOutput{}, newValidationError("path", "is required")
	}
	output, err := s.updateKickoff(ctx, input)
	if err != nil {
		return nil, UpdateKickoffOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleRemoveKickoff(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input RemoveKickoffInput,
) (*mcp.CallToolResult, RemoveKickoffOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, RemoveKickoffOutput{}, newValidationError("path", "is required")
	}
	output, err := s.removeKickoff(ctx, input)
	if err != nil {
		return nil, RemoveKickoffOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleSetArchitectSystem(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input SetArchitectPromptInput,
) (*mcp.CallToolResult, SetPromptOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, SetPromptOutput{}, newValidationError("path", "is required")
	}
	output, err := s.setArchitectSystem(ctx, input)
	if err != nil {
		return nil, SetPromptOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleSetArchitectKickoff(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input SetArchitectPromptInput,
) (*mcp.CallToolResult, SetPromptOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, SetPromptOutput{}, newValidationError("path", "is required")
	}
	output, err := s.setArchitectKickoff(ctx, input)
	if err != nil {
		return nil, SetPromptOutput{}, err
	}
	return nil, output, nil
}
