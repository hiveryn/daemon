package domain

import (
	"context"
	"time"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the Actions wire types from shared.
type (
	ActionProblem             = sd.ActionProblem
	ActionDefinition          = sd.ActionDefinition
	ActionList                = sd.ActionList
	ActionRunStatus           = sd.ActionRunStatus
	ActionRunTrigger          = sd.ActionRunTrigger
	ActionRun                 = sd.ActionRun
	LaunchActionRequest       = sd.LaunchActionRequest
	LaunchActionResult        = sd.LaunchActionResult
	ActionConclusionOutcome   = sd.ActionConclusionOutcome
	ConcludeActionRequest     = sd.ConcludeActionRequest
	ActionConclusion          = sd.ActionConclusion
	ActionEvent               = sd.ActionEvent
	ExecuteActionRequest      = sd.ExecuteActionRequest
	ExecuteActionResponse     = sd.ExecuteActionResponse
	AvailableActionList       = sd.AvailableActionList
	AddAvailableActionRequest = sd.AddAvailableActionRequest
	AddAvailableActionResult  = sd.AddAvailableActionResult
	ActionAgentActivity       = sd.ActionAgentActivity
	ActionAgentAttention      = sd.ActionAgentAttention
	ActionAttentionState      = sd.ActionAttentionState
	ActionAttentionSource     = sd.ActionAttentionSource
	ActionResult              = sd.ActionResult
	ActionWaitResult          = sd.ActionWaitResult
	ActionArtifactState       = sd.ActionArtifactState
	ActionArtifactCopy        = sd.ActionArtifactCopy
	ActionArtifactLocation    = sd.ActionArtifactLocation
	ActionArtifacts           = sd.ActionArtifacts
)

const (
	SessionTypeAction = sd.SessionTypeAction

	ActionEventType = sd.ActionEventType

	ActionRunPendingApproval = sd.ActionRunPendingApproval
	ActionRunDenied          = sd.ActionRunDenied
	ActionRunRunning         = sd.ActionRunRunning
	ActionRunCompleted       = sd.ActionRunCompleted
	ActionRunFailed          = sd.ActionRunFailed

	ActionRunTriggerManual    = sd.ActionRunTriggerManual
	ActionRunTriggerArchitect = sd.ActionRunTriggerArchitect
	ActionRunTriggerWorker    = sd.ActionRunTriggerWorker

	ActionConclusionCompleted = sd.ActionConclusionCompleted
	ActionConclusionFailed    = sd.ActionConclusionFailed

	ActionAttentionInputRequired  = sd.ActionAttentionInputRequired
	ActionAttentionNoneDetected   = sd.ActionAttentionNoneDetected
	ActionAttentionUnavailable    = sd.ActionAttentionUnavailable
	ActionAttentionSourceHook     = sd.ActionAttentionSourceHook
	ActionAttentionSourceTerminal = sd.ActionAttentionSourceTerminal

	ActionArtifactsPrepared = sd.ActionArtifactsPrepared
	ActionArtifactsNone     = sd.ActionArtifactsNone
	ActionArtifactsCopying  = sd.ActionArtifactsCopying
	ActionArtifactsReady    = sd.ActionArtifactsReady
	ActionArtifactsFailed   = sd.ActionArtifactsFailed
	ActionArtifactsExpired  = sd.ActionArtifactsExpired

	MaxActionSummaryLength         = sd.MaxActionSummaryLength
	MaxActionWaitSeconds           = sd.MaxActionWaitSeconds
	MaxActionSuggestions           = sd.MaxActionSuggestions
	MaxActionSuggestionWords       = sd.MaxActionSuggestionWords
	MaxActionSuggestionsTotalWords = sd.MaxActionSuggestionsTotalWords
)

// ValidActionName reports whether name is a well-formed action name.
func ValidActionName(name string) bool { return sd.ValidActionName(name) }

// ActionArtifactKind distinguishes the frozen package on the execution
// machine from the copies made for callers on other machines.
type ActionArtifactKind string

const (
	ActionArtifactPackage  ActionArtifactKind = "package"
	ActionArtifactDelivery ActionArtifactKind = "delivery"
)

// ActionArtifactRecord is the durable ownership record of one Hiveryn-managed
// artifact directory, written before the directory is created so that
// interrupted copies and expired directories can always be found and removed.
// It is keyed by (ExecutionID, Kind, Machine): one package per execution, one
// delivery per caller machine, plus staging:<uuid> records for candidate and
// retired snapshots. SSH is the alias used to reach Machine when
// the record was written. ManagedRoot is the managed directory Path lives in;
// empty means Path is not Hiveryn's to remove (a package delivered before
// frozen packages existed, kept at its original folder). A nil ExpiresAt
// never expires.
type ActionArtifactRecord struct {
	ExecutionID  string
	Kind         ActionArtifactKind
	Machine      string
	SSH          string
	Path         string
	ManagedRoot  string
	State        ActionArtifactState
	Error        string
	CleanupError string
	Files        int
	Bytes        int64
	CreatedAt    time.Time
	ReadyAt      *time.Time
	ExpiresAt    *time.Time
}

// Copy is the record's wire view.
func (r ActionArtifactRecord) Copy() ActionArtifactCopy {
	return ActionArtifactCopy{
		Machine: r.Machine, Path: r.Path, State: r.State, Error: r.Error, CleanupError: r.CleanupError,
		Files: r.Files, Bytes: r.Bytes, ReadyAt: r.ReadyAt, ExpiresAt: r.ExpiresAt,
	}
}

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
	// StartActionRun moves a pending_approval request to running with the
	// approved variant. It is a ConflictError when another execution of the
	// action is running (the single-run rule) or the request is no longer
	// pending.
	StartActionRun(ctx context.Context, id string, start ActionRunStart) error
	// PublishActionConclusion records the approved final outcome and optional
	// ready package atomically; preparation never publishes a package.
	PublishActionConclusion(ctx context.Context, id string, status ActionRunStatus, summary string, pkg *ActionArtifactRecord, at time.Time) error
	PrepareActionPackage(ctx context.Context, rec ActionArtifactRecord) error
	// EndPendingActionRun moves a pending_approval request to denied (reason:
	// the user's) or failed (errText: why it never started). It is a
	// ConflictError when the request is no longer pending.
	EndPendingActionRun(ctx context.Context, id string, status ActionRunStatus, reason, errText string, at time.Time) error
	// FailPendingActionRuns fails every pending_approval request with reason
	// and returns them; a restart loses their in-memory approval.
	FailPendingActionRuns(ctx context.Context, reason string, at time.Time) ([]ActionRun, error)
	// RecentActionConclusions returns up to limit concluded executions of the
	// action that carry an agent summary, newest first.
	RecentActionConclusions(ctx context.Context, action string, limit int) ([]ActionConclusion, error)

	// PutActionArtifact writes (inserts or replaces) one artifact record.
	PutActionArtifact(ctx context.Context, rec ActionArtifactRecord) error
	// ActionArtifacts returns an execution's artifact records.
	ActionArtifacts(ctx context.Context, executionID string) ([]ActionArtifactRecord, error)
	// ActionArtifactsToClean returns the records whose directories need
	// removal: copies left copying (interrupted), failed removals awaiting a
	// retry, and ready directories expired at now.
	ActionArtifactsToClean(ctx context.Context, now time.Time) ([]ActionArtifactRecord, error)
}

// ActionRunStart is what an approved request becomes when it starts: the
// variant and the execution location resolved at approval.
type ActionRunStart struct {
	ArtifactContract string
	ProfileName      string
	Machine          string
	RepoPath         string
	OutputDir        string
	At               time.Time
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
	DeliverActionArtifacts(ctx context.Context, sessionID, path string) (ActionArtifactCopy, error)
	ConcludeAction(ctx context.Context, sessionID string, req ConcludeActionRequest) (IntentResolution[ActionRun], error)
	RecentActionConclusions(ctx context.Context, sessionID string) ([]ActionConclusion, error)

	// Architect- and worker-facing, addressed by the calling architect or
	// ticket session; the project is always the session's, never the caller's
	// claim. AddAvailableAction is architect-only.
	AddAvailableAction(ctx context.Context, sessionID string, req AddAvailableActionRequest) (AddAvailableActionResult, error)
	AvailableActions(ctx context.Context, sessionID string) (AvailableActionList, error)
	RequestExecuteAction(ctx context.Context, sessionID string, req ExecuteActionRequest) (ExecuteActionResponse, error)
	GetActionResult(ctx context.Context, sessionID, executionID string) (ActionResult, error)
	WaitForActionResult(ctx context.Context, sessionID, executionID string, timeout time.Duration) (ActionWaitResult, error)
}
