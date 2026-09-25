package mcp

import (
	"context"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerArchitectTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeArchitectSession",
		Description: "End the architect session. Provide the structured fields (summary, narrative, open_questions, next_steps, plus optional tickets_touched/decisions/config_changes/user_priorities); the daemon renders them into the conclusion. The terminal is killed and the session cannot be resumed. The conclusion is sent to the user for approval before taking effect. Fails if any active ticket sessions are in progress.",
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
		Description: "Create a new work ticket in the backlog. The user is asked to approve before the ticket is created; this call blocks until they answer. Returns `outcome` — check it before assuming the ticket exists. approved/auto_approved means it was created and `ticket` is populated; denied_by_user/auto_denied means it was NOT created and you must not retry.",
	}, s.handleCreateWorkTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "editTicketBody",
		Description: "Perform exact string replacements in a ticket body.",
	}, s.handleEditTicketBody)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "updateTicket",
		Description: "Update ticket metadata fields (title, writable repo scope, references). References may be same-board ticket IDs or absolute filesystem paths used as read-only context; they never grant write access. Only present fields are updated; omitted fields are left unchanged.",
	}, s.handleUpdateTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "deleteTicket",
		Description: "Delete a ticket by ID.",
	}, s.handleDeleteTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "moveTicketToDone",
		Description: "Move a ticket directly to done without a worker session — for tickets the architect resolved themselves (backlog → done), or to manually close a ticket whose worker session is dead or stuck (progress → done; fails if a worker session is currently running for the ticket). Requires outcome=completed/exploratory/rejected; rejected requires rejection_reason.",
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

	// The architect edits hiveryn.yaml, the project documents and workflows
	// with its own file tools, guided by describeArtifact and checked by
	// checkWorkspace. There is no config-authoring tool, no prompt tool and no
	// spawn tool: prompts are built in, and only the user launches workers.
	s.registerWorkspaceTools()

	// Actions: discovery of this architect's availableActions, the deferred
	// executeAction request and its result/wait under the one execution id.
	// Only architects get these; action agents cannot invoke Actions.
	s.registerAgentActionTools()
	s.registerAddAvailableActionTool()
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
) (*mcp.CallToolResult, CreateWorkTicketOutput, error) {
	if strings.TrimSpace(input.Title) == "" {
		return nil, CreateWorkTicketOutput{}, newValidationError("title", "is required")
	}

	output, err := s.createWorkTicket(ctx, input)
	if err != nil {
		return nil, CreateWorkTicketOutput{}, err
	}

	return nil, output, nil
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
	outcome := domain.TicketOutcome(input.Outcome)
	if !outcome.Valid() {
		return nil, MoveTicketToDoneOutput{}, newValidationError("outcome", "must be one of: completed, exploratory, rejected")
	}
	if outcome == domain.TicketOutcomeRejected && strings.TrimSpace(input.RejectionReason) == "" {
		return nil, MoveTicketToDoneOutput{}, newValidationError("rejection_reason", "is required when outcome is rejected")
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

func (s *Server) handleArchitectConcludeSession(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ArchitectConcludeSessionInput,
) (*mcp.CallToolResult, ConcludeSessionOutput, error) {
	if s.sessionID == "" {
		return nil, ConcludeSessionOutput{}, newInternalError("HIVERYN_SESSION_ID not set")
	}

	output, err := s.concludeSession(ctx, concludeRequest{
		Summary:        input.Summary,
		Narrative:      input.Narrative,
		TicketsTouched: input.TicketsTouched,
		Decisions:      input.Decisions,
		ConfigChanges:  input.ConfigChanges,
		UserPriorities: input.UserPriorities,
		OpenQuestions:  input.OpenQuestions,
		NextSteps:      input.NextSteps,
	})
	if err != nil {
		return nil, ConcludeSessionOutput{}, err
	}

	return nil, output, nil
}

func (s *Server) handleTicketConcludeSession(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input TicketConcludeSessionInput,
) (*mcp.CallToolResult, ConcludeSessionOutput, error) {
	if err := validateCommitShapes(input.Commits); err != nil {
		return nil, ConcludeSessionOutput{}, err
	}
	if !domain.TicketOutcome(input.Outcome).Valid() {
		return nil, ConcludeSessionOutput{}, newValidationError("outcome", "must be one of: completed, exploratory, rejected")
	}
	if s.sessionID == "" {
		return nil, ConcludeSessionOutput{}, newInternalError("HIVERYN_SESSION_ID not set")
	}

	output, err := s.concludeSession(ctx, concludeRequest{
		Summary:         input.Summary,
		Implementation:  input.Implementation,
		Deviations:      input.Deviations,
		Verification:    input.Verification,
		FollowUps:       input.FollowUps,
		OpenQuestions:   input.OpenQuestions,
		Commits:         input.Commits,
		Outcome:         input.Outcome,
		RejectionReason: input.RejectionReason,
	})
	if err != nil {
		return nil, ConcludeSessionOutput{}, err
	}

	return nil, output, nil
}

func validateCommitShapes(commits []domain.CommitRef) error {
	for _, commit := range commits {
		if strings.TrimSpace(commit.SHA) == "" {
			return newValidationError("commits", "commit sha is required")
		}
		if strings.TrimSpace(commit.Repo) == "" {
			return newValidationError("commits", "commit repo is required")
		}
	}
	return nil
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
