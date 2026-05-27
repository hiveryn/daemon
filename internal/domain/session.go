package domain

import (
	"context"
	"time"
)

type SessionType string

type SessionCreatedBy string

type SessionRunStatus string

type SessionRunFailureReason string

const (
	SessionTypeArchitect SessionType = "architect"
	SessionTypeTicket    SessionType = "ticket"
	SessionTypeFreeform  SessionType = "freeform"
)

const (
	SessionCreatedByDesktop      SessionCreatedBy = "desktop"
	SessionCreatedByArchitectMCP SessionCreatedBy = "architect_mcp"
)

const (
	SessionRunStatusRunning   SessionRunStatus = "running"
	SessionRunStatusCompleted SessionRunStatus = "completed"
	SessionRunStatusFailed    SessionRunStatus = "failed"
)

const (
	SessionRunFailureLaunchFailed  SessionRunFailureReason = "launch_failed"
	SessionRunFailureProcessExited SessionRunFailureReason = "process_exited"
	SessionRunFailureRestoreFailed SessionRunFailureReason = "restore_failed"
	SessionRunFailureUserCancelled SessionRunFailureReason = "user_cancelled"
)

type AgentProfileSnapshot struct {
	Agent string            `json:"agent"`
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
}

type SessionIntent struct {
	ID           string           `json:"id"`
	ArchitectKey string           `json:"architect_key"`
	SessionType  SessionType      `json:"session_type"`
	ContextID    string           `json:"context_id"`
	Prompt       string           `json:"prompt"`
	Workdir      string           `json:"workdir"`
	Instructions string           `json:"instructions,omitempty"`
	CreatedBy    SessionCreatedBy `json:"created_by,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	CurrentRun   *SessionRun      `json:"current_run,omitempty"`
}

type SessionRun struct {
	ID              string                  `json:"id"`
	SessionIntentID string                  `json:"session_intent_id"`
	Status          SessionRunStatus        `json:"status"`
	ProfileName     string                  `json:"profile_name"`
	ProfileSnapshot *AgentProfileSnapshot   `json:"profile_snapshot,omitempty"`
	Workdir         string                  `json:"workdir"`
	NativeID        string                  `json:"native_id,omitempty"`
	FailureReason   SessionRunFailureReason `json:"failure_reason,omitempty"`
	MainTerminalID  string                  `json:"main_terminal_id,omitempty"`
	StartedAt       *time.Time              `json:"started_at,omitempty"`
	EndedAt         *time.Time              `json:"ended_at,omitempty"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
}

type SessionEvent struct {
	ID                string            `json:"id"`
	SessionIntentID   string            `json:"session_intent_id"`
	RunID             string            `json:"run_id,omitempty"`
	Seq               int64             `json:"seq"`
	Type              string            `json:"type"`
	Status            string            `json:"status,omitempty"`
	Tool              string            `json:"tool,omitempty"`
	Message           string            `json:"message,omitempty"`
	NativeID          string            `json:"native_id,omitempty"`
	PrimaryNativeID   string            `json:"primary_native_id,omitempty"`
	NativeSessionRole string            `json:"native_session_role,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Raw               map[string]any    `json:"raw,omitempty"`
	At                time.Time         `json:"at"`
}

type CreateSessionIntentRequest struct {
	SessionType  SessionType `json:"session_type"`
	ArchitectKey string      `json:"architect_key"`
	TicketID     string      `json:"ticket_id,omitempty"`
	Prompt       string      `json:"prompt,omitempty"`
	Workdir      string      `json:"workdir,omitempty"`
	Slug         string      `json:"slug,omitempty"`
}

type CreateSessionIntentParams struct {
	ID           string
	ArchitectKey string
	SessionType  SessionType
	ContextID    string
	Prompt       string
	Workdir      string
	Instructions string
	CreatedBy    SessionCreatedBy
}

type CreateSessionRunRequest struct {
	ProfileName string `json:"profile_name"`
	Cols        uint16 `json:"cols,omitempty"`
	Rows        uint16 `json:"rows,omitempty"`
}

type CreateSessionRunParams struct {
	ID              string
	SessionIntentID string
	ProfileName     string
	ProfileSnapshot AgentProfileSnapshot
	Workdir         string
	NativeID        string
	StartedAt       time.Time
}

type CreateSessionRunResult struct {
	Run            SessionRun `json:"run"`
	MainTerminalID string     `json:"main_terminal_id,omitempty"`
}

type AppendSessionEventParams struct {
	SessionIntentID   string
	RunID             string
	Type              string
	Status            string
	Tool              string
	Message           string
	NativeID          string
	PrimaryNativeID   string
	NativeSessionRole string
	Metadata          map[string]string
	Raw               map[string]any
	At                time.Time
}

type SessionRepository interface {
	CreateIntent(context.Context, CreateSessionIntentParams) (SessionIntent, error)
	GetIntent(context.Context, string) (SessionIntent, error)
	ListIntents(context.Context) ([]SessionIntent, error)
	DeleteIntent(context.Context, string) error
	CreateRun(context.Context, CreateSessionRunParams) (SessionRun, error)
	GetRun(context.Context, string) (SessionRun, error)
	GetCurrentRun(context.Context, string) (*SessionRun, error)
	MarkRunCompleted(context.Context, string) error
	MarkRunFailed(context.Context, string, SessionRunFailureReason) error
	UpdateRunNativeID(context.Context, string, string) error
	ListSessionEvents(context.Context, string) ([]SessionEvent, error)
	AppendSessionEvent(context.Context, AppendSessionEventParams) (SessionEvent, error)
}

type ConcludeSessionParams struct {
	Body            string
	Commits         []CommitRef
	Rejected        bool
	RejectionReason string
}

type ConcludeSessionResult struct {
	SessionID    string `json:"session_id"`
	ArchitectKey string `json:"architect_key"`
	TicketID     string `json:"ticket_id,omitempty"`
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

type TerminalInfo struct {
	TerminalID string `json:"terminal_id"`
	SessionID  string `json:"session_id"`
	Command    string `json:"command"`
	Status     string `json:"status"`
}

type CreateTerminalParams struct{}

type SessionTab struct {
	Type       string `json:"type"`
	TerminalID string `json:"id,omitempty"`
	Command    string `json:"command,omitempty"`
	Status     string `json:"status,omitempty"`
}

type ArchitectConclusion struct {
	StartedAt   time.Time `json:"started_at"`
	ConcludedAt time.Time `json:"concluded_at"`
	Agent       string    `json:"agent,omitempty"`
	Body        string    `json:"body"`
}

type ConclusionSummary struct {
	ID          string    `json:"id"`
	ConcludedAt time.Time `json:"concluded_at"`
}

type SessionService interface {
	CreateIntent(context.Context, CreateSessionIntentRequest) (SessionIntent, error)
	CreateRun(context.Context, string, CreateSessionRunRequest) (CreateSessionRunResult, error)
	ConcludeSession(context.Context, string, ConcludeSessionParams) (ConcludeSessionResult, error)
	RequestConclusion(context.Context, string, ConcludeSessionParams) (ConcludeSessionResult, error)
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
}
