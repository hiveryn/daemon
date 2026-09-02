package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hiveryn/daemon/internal/domain"
)

func (s *Server) handleReadRoadmap(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ReadRoadmapInput,
) (*mcp.CallToolResult, ReadRoadmapOutput, error) {
	view := strings.TrimSpace(input.View)
	switch view {
	case "", "current", "archive":
	default:
		return nil, ReadRoadmapOutput{}, newValidationError("view", "must be current or archive")
	}
	if input.Depth != nil {
		if view == "archive" {
			return nil, ReadRoadmapOutput{}, newValidationError("depth", "applies to the current view only")
		}
		if *input.Depth < 0 {
			return nil, ReadRoadmapOutput{}, newValidationError("depth", "must be >= 0")
		}
	}
	// Forward the trimmed values so what passed local validation is what the
	// daemon sees.
	input.View = view
	input.ID = strings.TrimSpace(input.ID)
	output, err := s.readRoadmap(ctx, input)
	if err != nil {
		return nil, ReadRoadmapOutput{}, err
	}
	return nil, output, nil
}

// applyRoadmapOp validates the version, wraps one operation into the daemon's
// batch contract, and sends it. All eight mutation tools funnel through here.
func (s *Server) applyRoadmapOp(ctx context.Context, version string, title *string, ops []domain.RoadmapOp) (UpdateRoadmapOutput, error) {
	if strings.TrimSpace(version) == "" {
		return UpdateRoadmapOutput{}, newValidationError("version", "is required; read the roadmap first")
	}
	return s.putRoadmap(ctx, domain.UpdateRoadmapParams{Version: version, Title: title, Ops: ops})
}

func requireID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", newValidationError("id", "is required")
	}
	return trimmed, nil
}

func (s *Server) handleCreateRoadmapItem(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input CreateRoadmapItemInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	id, err := requireID(input.ID)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	op := domain.RoadmapOp{
		Type:    domain.RoadmapOpCreate,
		ID:      id,
		Kind:    domain.RoadmapItemKind(strings.TrimSpace(input.Kind)),
		Title:   &input.Title,
		Outcome: &input.Outcome,
		Order:   input.Order,
	}
	if status := strings.TrimSpace(input.Status); status != "" {
		roadmapStatus := domain.RoadmapItemStatus(status)
		op.Status = &roadmapStatus
	}
	if parent := strings.TrimSpace(input.ParentID); parent != "" {
		op.ParentID = &parent
	}
	if input.SuccessCriteria != nil {
		op.SuccessCriteria = &input.SuccessCriteria
	}
	if input.DependsOn != nil {
		op.DependsOn = &input.DependsOn
	}
	output, err := s.applyRoadmapOp(ctx, input.Version, nil, []domain.RoadmapOp{op})
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleUpdateRoadmapItem(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input UpdateRoadmapItemInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	id, err := requireID(input.ID)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	if input.Title == nil && input.Status == nil && input.Outcome == nil &&
		input.SuccessCriteria == nil && input.DependsOn == nil {
		return nil, UpdateRoadmapOutput{}, newValidationError("update", "provide at least one of: title, status, outcome, success_criteria, depends_on")
	}
	op := domain.RoadmapOp{
		Type:            domain.RoadmapOpUpdate,
		ID:              id,
		Title:           input.Title,
		Outcome:         input.Outcome,
		SuccessCriteria: input.SuccessCriteria,
		DependsOn:       input.DependsOn,
	}
	if input.Status != nil {
		status := domain.RoadmapItemStatus(*input.Status)
		op.Status = &status
	}
	output, err := s.applyRoadmapOp(ctx, input.Version, nil, []domain.RoadmapOp{op})
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleMoveRoadmapItem(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input MoveRoadmapItemInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	id, err := requireID(input.ID)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	// ParentID passes through as a tri-state: omitted keeps the current
	// parent (pure reorder), an explicit empty string means top level.
	op := domain.RoadmapOp{Type: domain.RoadmapOpMove, ID: id, ParentID: input.ParentID, Order: input.Order}
	output, err := s.applyRoadmapOp(ctx, input.Version, nil, []domain.RoadmapOp{op})
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) ticketLinkOp(ctx context.Context, opType domain.RoadmapOpType, input LinkRoadmapTicketInput) (UpdateRoadmapOutput, error) {
	id, err := requireID(input.ID)
	if err != nil {
		return UpdateRoadmapOutput{}, err
	}
	if strings.TrimSpace(input.TicketID) == "" {
		return UpdateRoadmapOutput{}, newValidationError("ticket_id", "is required")
	}
	op := domain.RoadmapOp{Type: opType, ID: id, TicketID: strings.TrimSpace(input.TicketID)}
	return s.applyRoadmapOp(ctx, input.Version, nil, []domain.RoadmapOp{op})
}

func (s *Server) handleLinkRoadmapTicket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input LinkRoadmapTicketInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	output, err := s.ticketLinkOp(ctx, domain.RoadmapOpLinkTicket, input)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleUnlinkRoadmapTicket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input UnlinkRoadmapTicketInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	output, err := s.ticketLinkOp(ctx, domain.RoadmapOpUnlinkTicket, input)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleArchiveRoadmapItem(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ArchiveRoadmapItemInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	id, err := requireID(input.ID)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	op := domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: id, Summary: input.Summary}
	output, err := s.applyRoadmapOp(ctx, input.Version, nil, []domain.RoadmapOp{op})
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleRestoreRoadmapItem(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input RestoreRoadmapItemInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	id, err := requireID(input.ID)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	// ParentID passes through untouched: omitted means "original location",
	// an explicit empty string means top level.
	op := domain.RoadmapOp{Type: domain.RoadmapOpRestore, ID: id, ParentID: input.ParentID, Order: input.Order}
	output, err := s.applyRoadmapOp(ctx, input.Version, nil, []domain.RoadmapOp{op})
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleSetRoadmapTitle(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input SetRoadmapTitleInput,
) (*mcp.CallToolResult, UpdateRoadmapOutput, error) {
	if strings.TrimSpace(input.Title) == "" {
		return nil, UpdateRoadmapOutput{}, newValidationError("title", "is required")
	}
	output, err := s.applyRoadmapOp(ctx, input.Version, &input.Title, nil)
	if err != nil {
		return nil, UpdateRoadmapOutput{}, err
	}
	return nil, output, nil
}
