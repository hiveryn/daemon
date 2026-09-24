package domain

import (
	"context"
	"time"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the Actions wire types from shared.
type (
	ActionProblem           = sd.ActionProblem
	ActionDefinition        = sd.ActionDefinition
	ActionList              = sd.ActionList
	ActionRunStatus         = sd.ActionRunStatus
	ActionRunTrigger        = sd.ActionRunTrigger
	ActionRun               = sd.ActionRun
	LaunchActionRequest     = sd.LaunchActionRequest
	LaunchActionResult      = sd.LaunchActionResult
	ActionConclusionOutcome = sd.ActionConclusionOutcome
	ConcludeActionRequest   = sd.ConcludeActionRequest
	ActionConclusion        = sd.ActionConclusion
	ActionEvent             = sd.ActionEvent
)

const (
	SessionTypeAction = sd.SessionTypeAction

	ActionEventType = sd.ActionEventType

	ActionRunPendingApproval = sd.ActionRunPendingApproval
	ActionRunDenied          = sd.ActionRunDenied
	ActionRunRunning         = sd.ActionRunRunning
	ActionRunCompleted       = sd.ActionRunCompleted
	ActionRunFailed          = sd.ActionRunFailed

	ActionRunTriggerManual = sd.ActionRunTriggerManual

	ActionConclusionCompleted = sd.ActionConclusionCompleted
	ActionConclusionFailed    = sd.ActionConclusionFailed

	MaxActionSummaryLength = sd.MaxActionSummaryLength
)

// ActionRunRepository persists action executions. The record is the durable
// history of an action: it is never pruned and outlives the agent session.
//
// Status changes are compare-and-set, so a terminal outcome is never
// overwritten, and at most one execution per action may be running — a
// second CreateActionRun for a running action is a ConflictError.
type ActionRunRepository interface {
	CreateActionRun(context.Context, ActionRun) error
	GetActionRun(context.Context, string) (ActionRun, error)
	// ListActionRuns returns executions newest first; action "" lists all.
	ListActionRuns(ctx context.Context, action string, limit int) ([]ActionRun, error)
	// RunningActionRuns returns every running execution, keyed by action.
	RunningActionRuns(context.Context) (map[string]ActionRun, error)
	SetActionRunSession(ctx context.Context, id, sessionID string) error
	// FinishActionRun moves a running execution to completed or failed with
	// its summary and/or error. It is a ConflictError when the execution is no
	// longer running.
	FinishActionRun(ctx context.Context, id string, status ActionRunStatus, summary, errText string, at time.Time) error
	// RecentActionConclusions returns up to limit concluded executions of the
	// action that carry an agent summary, newest first.
	RecentActionConclusions(ctx context.Context, action string, limit int) ([]ActionConclusion, error)
}

type ActionEventSubscription interface {
	C() <-chan ActionEvent
	Close()
}

// ActionService is the Actions runtime behind the HTTP API: definitions,
// manual launches, execution records and the action agent's own tools.
type ActionService interface {
	ListActions(context.Context) (ActionList, error)
	GetAction(context.Context, string) (ActionDefinition, error)
	LaunchAction(context.Context, string, LaunchActionRequest) (LaunchActionResult, error)
	ListActionRuns(ctx context.Context, action string, limit int) ([]ActionRun, error)
	GetActionRun(context.Context, string) (ActionRun, error)
	CancelActionRun(context.Context, string) (ActionRun, error)
	SubscribeActionEvents() ActionEventSubscription

	// Agent-facing, addressed by the calling action session.
	ConcludeAction(ctx context.Context, sessionID string, req ConcludeActionRequest) (ActionRun, error)
	RecentActionConclusions(ctx context.Context, sessionID string) ([]ActionConclusion, error)
}
