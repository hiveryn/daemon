package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerArchitectTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeArchitectSession",
		Description: "End the architect session. Records a summary of decisions, tickets created, and next steps. The terminal is killed and the session cannot be resumed. The conclusion is sent to the user for approval before taking effect. Fails if any active ticket sessions are in progress.",
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

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "moveTicketToDone",
		Description: "Move a ticket directly to done without a worker session — for tickets the architect resolved themselves (backlog → done), or to manually close a ticket whose worker session is dead or stuck (progress → done; fails if a worker session is currently running for the ticket). Supports rejection via rejected=true with a rejection_reason.",
	}, s.handleMoveTicketToDone)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readArchitectConclusion",
		Description: "Read an architect session conclusion by ID.",
	}, s.handleReadConclusion)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readRecentArchitectConclusion",
		Description: "Read the most recent architect session conclusion.",
	}, s.handleReadRecentConclusion)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "listArchitectConclusions",
		Description: "List recent architect session conclusions. Returns summaries with IDs and timestamps.",
	}, s.handleListConclusions)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readTicketConclusion",
		Description: "Read the conclusion for a specific ticket by ID. Errors with NOT_FOUND if the ticket has no conclusion.",
	}, s.handleReadTicketConclusion)

	s.registerConfigTools()
}

// registerConfigTools registers the hiveryn.yaml config-management tools. They
// own the yaml wiring, validation, and path resolution; the agent writes the
// prompt/markdown file contents itself. Editing is a read → modify → write-back
// cycle: readArchitectConfig returns the whole config plus a version token,
// updateArchitectConfig writes the whole config back guarded by that token.
func (s *Server) registerConfigTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readArchitectConfig",
		Description: "Read the architect's hiveryn.yaml: the whole config (repos, architect prompts, ticket kickoffs), a resolved view (absolute paths + which wired prompt files exist), warnings for missing ones, and an opaque version token to pass to updateArchitectConfig.",
	}, s.handleReadArchitectConfig)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "updateArchitectConfig",
		Description: "Whole-document replace of the architect's hiveryn.yaml, guarded by the version token from readArchitectConfig. Send back the config you read with edits applied; missing wired prompt files are auto-created from defaults. You author prompt file contents yourself.",
	}, s.handleUpdateArchitectConfig)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readDefaultPrompt",
		Description: "Get the embedded default template for a prompt kind plus the template variables valid for it — a starting point for authoring a prompt file.",
	}, s.handleReadDefaultPrompt)
}

func (s *Server) handleReadTicketConclusion(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ReadTicketConclusionInput,
) (*mcp.CallToolResult, TicketConclusionOutput, error) {
	if strings.TrimSpace(input.TicketID) == "" {
		return nil, TicketConclusionOutput{}, newValidationError("ticketId", "cannot be empty")
	}

	ticket, err := s.readTicket(ctx, input.TicketID)
	if err != nil {
		return nil, TicketConclusionOutput{}, err
	}

	if ticket.Conclusion == nil {
		return nil, TicketConclusionOutput{}, newNotFoundError("conclusion not found")
	}

	return nil, *ticket.Conclusion, nil
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

func (s *Server) handleMoveTicketToDone(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input MoveTicketToDoneInput,
) (*mcp.CallToolResult, MoveTicketToDoneOutput, error) {
	if strings.TrimSpace(input.ID) == "" {
		return nil, MoveTicketToDoneOutput{}, newValidationError("id", "cannot be empty")
	}
	if strings.TrimSpace(input.Body) == "" {
		return nil, MoveTicketToDoneOutput{}, newValidationError("body", "is required")
	}
	if input.Rejected && strings.TrimSpace(input.RejectionReason) == "" {
		return nil, MoveTicketToDoneOutput{}, newValidationError("rejection_reason", "is required when rejected is true")
	}
	for _, commit := range input.Commits {
		if strings.TrimSpace(commit.SHA) == "" {
			return nil, MoveTicketToDoneOutput{}, newValidationError("commits", "commit sha is required")
		}
		if strings.TrimSpace(commit.Repo) == "" {
			return nil, MoveTicketToDoneOutput{}, newValidationError("commits", "commit repo is required")
		}
	}

	output, err := s.moveTicketToDone(ctx, input)
	if err != nil {
		return nil, MoveTicketToDoneOutput{}, err
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
	if s.sessionType == SessionTypeFreeform {
		if input.Rejected {
			return nil, ConcludeSessionOutput{}, newValidationError("rejected", "freeform sessions do not support rejected mode")
		}
		if strings.TrimSpace(input.RejectionReason) != "" {
			return nil, ConcludeSessionOutput{}, newValidationError("rejection_reason", "freeform sessions do not accept a rejection reason")
		}
	}
	for _, commit := range input.Commits {
		if strings.TrimSpace(commit.SHA) == "" {
			return nil, ConcludeSessionOutput{}, newValidationError("commits", "commit sha is required")
		}
		if strings.TrimSpace(commit.Repo) == "" {
			return nil, ConcludeSessionOutput{}, newValidationError("commits", "commit repo is required")
		}
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

func (s *Server) handleReadConclusion(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ReadConclusionInput,
) (*mcp.CallToolResult, ReadConclusionOutput, error) {
	if strings.TrimSpace(input.ConclusionID) == "" {
		return nil, ReadConclusionOutput{}, newValidationError("conclusionId", "cannot be empty")
	}

	output, err := s.readConclusion(ctx, input.ConclusionID)
	if err != nil {
		return nil, ReadConclusionOutput{}, err
	}

	return nil, output, nil
}

func (s *Server) handleReadRecentConclusion(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, ReadConclusionOutput, error) {
	output, err := s.readRecentConclusion(ctx)
	if err != nil {
		return nil, ReadConclusionOutput{}, err
	}

	return nil, output, nil
}

func (s *Server) handleListConclusions(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ListConclusionsInput,
) (*mcp.CallToolResult, ListConclusionsOutput, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}

	output, err := s.listConclusions(ctx, limit)
	if err != nil {
		return nil, ListConclusionsOutput{}, err
	}

	return nil, output, nil
}
