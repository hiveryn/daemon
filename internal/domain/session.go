package domain

import (
	"context"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export pure data types from shared. All JSON tags, field names, and
// underlying types are identical so existing code continues to compile and
// behave exactly as before.
type (
	SessionType             = sd.SessionType
	SessionCreatedBy        = sd.SessionCreatedBy
	SessionRunStatus        = sd.SessionRunStatus
	SessionRunFailureReason = sd.SessionRunFailureReason

	AgentProfileSnapshot       = sd.AgentProfileSnapshot
	MCPServerSnapshot          = sd.MCPServerSnapshot
	SessionIntent              = sd.SessionIntent
	SessionRun                 = sd.SessionRun
	SessionEvent               = sd.SessionEvent
	CreateSessionIntentRequest = sd.CreateSessionIntentRequest
	CreateSessionIntentParams  = sd.CreateSessionIntentParams
	CreateSessionRunRequest    = sd.CreateSessionRunRequest
	CreateSessionRunParams     = sd.CreateSessionRunParams
	CreateSessionRunResult     = sd.CreateSessionRunResult
	AppendSessionEventParams   = sd.AppendSessionEventParams
	ConcludeSessionParams      = sd.ConcludeSessionParams
	ConcludeSessionResult      = sd.ConcludeSessionResult
	MoveTicketToDoneParams     = sd.MoveTicketToDoneParams
	MoveTicketToDoneResult     = sd.MoveTicketToDoneResult
	TerminalInfo               = sd.TerminalInfo
	TerminalPlacement          = sd.TerminalPlacement
	CreateTerminalParams       = sd.CreateTerminalParams
	SessionTab                 = sd.SessionTab
	PreviewBrowserTabParams    = sd.PreviewBrowserTabParams
	BrowserTabInfo             = sd.BrowserTabInfo
	ArchitectConclusion        = sd.ArchitectConclusion
	ConclusionSummary          = sd.ConclusionSummary
)

// Daemon-local agent status values used by the runtime bridge and stored on
// SessionRun. These are intentionally not in shared.
const (
	AgentStatusActive  = "active"
	AgentStatusIdle    = "idle"
	AgentStatusWaiting = "waiting"
	AgentStatusStopped = "stopped"
)

// Re-export the canonical enum values so that code throughout the tree can
// keep writing domain.SessionTypeArchitect, domain.SessionRunStatusRunning,
// &domain.NotFoundError{...}, etc. with zero changes.
const (
	SessionTypeArchitect = sd.SessionTypeArchitect
	SessionTypeTicket    = sd.SessionTypeTicket
	SessionTypeFreeform  = sd.SessionTypeFreeform

	SessionCreatedByDesktop      = sd.SessionCreatedByDesktop
	SessionCreatedByArchitectMCP = sd.SessionCreatedByArchitectMCP

	SessionRunStatusRunning   = sd.SessionRunStatusRunning
	SessionRunStatusCompleted = sd.SessionRunStatusCompleted
	SessionRunStatusFailed    = sd.SessionRunStatusFailed

	SessionRunFailureLaunchFailed  = sd.SessionRunFailureLaunchFailed
	SessionRunFailureProcessExited = sd.SessionRunFailureProcessExited
	SessionRunFailureRestoreFailed = sd.SessionRunFailureRestoreFailed
	SessionRunFailureUserCancelled = sd.SessionRunFailureUserCancelled

	TerminalPlacementTab   = sd.TerminalPlacementTab
	TerminalPlacementSplit = sd.TerminalPlacementSplit
)

type SessionRepository interface {
	CreateIntent(context.Context, CreateSessionIntentParams) (SessionIntent, error)
	GetIntent(context.Context, string) (SessionIntent, error)
	ListIntents(context.Context) ([]SessionIntent, error)
	DeleteIntent(context.Context, string) error
	CreateRun(context.Context, CreateSessionRunParams) (SessionRun, error)
	GetRun(context.Context, string) (SessionRun, error)
	GetCurrentRun(context.Context, string) (*SessionRun, error)
	DeleteRun(context.Context, string) error
	MarkRunCompleted(context.Context, string) error
	MarkRunFailed(context.Context, string, SessionRunFailureReason) error
	UpdateRunNativeID(context.Context, string, string) error
	UpdateRunAgentStatus(context.Context, string, string) error
	ListSessionEvents(context.Context, string) ([]SessionEvent, error)
	AppendSessionEvent(context.Context, AppendSessionEventParams) (SessionEvent, error)
}

type SessionEventSubscription interface {
	C() <-chan SessionEvent
	Close()
}

type TerminalAttachment interface {
	Output() <-chan []byte
	Write([]byte) error
	Resize(cols, rows uint16) error
	Close() error
}

type SessionService interface {
	CreateIntent(context.Context, CreateSessionIntentRequest) (SessionIntent, error)
	CreateRun(context.Context, string, CreateSessionRunRequest) (CreateSessionRunResult, error)
	ConcludeSession(context.Context, string, ConcludeSessionParams) (ConcludeSessionResult, error)
	UnspawnTicketSession(context.Context, string) (ConcludeSessionResult, error)
	RequestConclusion(context.Context, string, ConcludeSessionParams) (ConcludeSessionResult, error)
	MoveTicketToDone(context.Context, string, string, MoveTicketToDoneParams) (MoveTicketToDoneResult, error)
	ApproveConclusion(context.Context, string) (ConcludeSessionResult, error)
	RejectConclusion(context.Context, string, string) error
	ReadConclusion(context.Context, string, string) (ArchitectConclusion, error)
	ReadRecentConclusion(context.Context, string) (ArchitectConclusion, error)
	ListConclusions(context.Context, string, int) ([]ConclusionSummary, error)
	GetIntent(context.Context, string) (SessionIntent, error)
	ListIntents(context.Context) ([]SessionIntent, error)
	ListSessionEvents(context.Context, string) ([]SessionEvent, error)
	SubscribeSessionEvents(context.Context, string) (SessionEventSubscription, error)
	AttachTerminal(context.Context, string, string) (TerminalAttachment, error)
	CreateTerminal(context.Context, string, CreateTerminalParams) (TerminalInfo, error)
	ListTerminals(context.Context, string) ([]TerminalInfo, error)
	ListSessionTabs(context.Context, string) ([]SessionTab, error)
	KillTerminal(context.Context, string, string) error
	PreviewBrowserTab(context.Context, string, PreviewBrowserTabParams) (BrowserTabInfo, error)
	CloseBrowserTab(context.Context, string, string) error
}
