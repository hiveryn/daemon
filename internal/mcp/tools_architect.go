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

	// Worker launches: the user's from the desktop, or an architect's
	// spawnTicketWorker request, which the user approves (or lets expire).
	s.registerSpawnTicketWorkerTool()

	// Escalated worker questions: the architect may answer a ticket worker's
	// question the user left unanswered. Workers never get this tool.
	s.registerAnswerQuestionTool()

	// Companions of this architect session.
	s.registerArchitectCompanionTools()

	// Read-only context of this project — the workspace and every configured
	// repo where it lives — so a remote companion's AGENTS.md is readable.
	s.registerContextTools()
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
