package domain

import (
	"context"
	"time"
)

type SessionStatus string

type SessionType string

const (
	SessionTypeArchitect SessionType = "architect"
	SessionTypeWork      SessionType = "work"
	SessionTypeCollab    SessionType = "collab"
)

const (
	SessionStatusRunning   SessionStatus = "running"
	SessionStatusCompleted SessionStatus = "completed"
	SessionStatusFailed    SessionStatus = "failed"
)

type Session struct {
	ID             string        `json:"id"`
	ProfileName    string        `json:"profile_name"`
	ArchitectKey   string        `json:"architect_key"`
	SessionType    string        `json:"session_type"`
	Prompt         string        `json:"prompt"`
	Instructions   string        `json:"instructions"`
	Status         SessionStatus `json:"status"`
	TicketID       string        `json:"ticket_id,omitempty"`
	NativeID       string        `json:"native_id,omitempty"`
	MainTerminalID string        `json:"main_terminal_id,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type SessionEvent struct {
	ID                string            `json:"id"`
	SessionID         string            `json:"session_id"`
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

type SessionListFilter struct {
	Status SessionStatus
}

type CreateSessionParams struct {
	ID           string
	ProfileName  string
	ArchitectKey string
	SessionType  string
	Prompt       string
	Instructions string
	Status       SessionStatus
	TicketID     string
	NativeID     string
}

type AppendSessionEventParams struct {
	SessionID         string
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
	CreateSession(context.Context, CreateSessionParams) (Session, error)
	GetSession(context.Context, string) (Session, error)
	ListSessions(context.Context, SessionListFilter) ([]Session, error)
	UpdateSessionStatus(context.Context, string, SessionStatus) error
	UpdateSessionNativeID(context.Context, string, string) error
	EndSession(context.Context, string) error
	DeleteSession(context.Context, string) error
	ListSessionEvents(context.Context, string) ([]SessionEvent, error)
	AppendSessionEvent(context.Context, AppendSessionEventParams) (SessionEvent, error)
	FailRunningSessions(context.Context) error
}

type ConcludeSessionParams struct {
	Body            string
	Commits         []string
	Rejected        bool
	RejectionReason string
}

type ConcludeSessionResult struct {
	SessionID    string `json:"session_id"`
	ArchitectKey string `json:"architect_key"`
	TicketID     string `json:"ticket_id,omitempty"`
}

type SpawnArchitectSessionRequest struct {
	ArchitectKey string
	ProfileName  string
	Cols         uint16
	Rows         uint16
}

type SpawnArchitectSessionResult struct {
	Session        Session
	MainTerminalID string `json:"main_terminal_id"`
}

type SpawnWorkSessionRequest struct {
	ArchitectKey string
	TicketID     string
	ProfileName  string
	Mode         string
	Cols         uint16
	Rows         uint16
}

type SpawnWorkSessionResult struct {
	Session        Session
	MainTerminalID string `json:"main_terminal_id"`
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
	SpawnArchitectSession(context.Context, SpawnArchitectSessionRequest) (SpawnArchitectSessionResult, error)
	SpawnWorkSession(context.Context, SpawnWorkSessionRequest) (SpawnWorkSessionResult, error)
	ConcludeSession(context.Context, string, ConcludeSessionParams) (ConcludeSessionResult, error)
	ReadConclusion(context.Context, string, string) (ArchitectConclusion, error)
	ReadRecentConclusion(context.Context, string) (ArchitectConclusion, error)
	ListConclusions(context.Context, string, int) ([]ConclusionSummary, error)
	GetSession(context.Context, string) (Session, error)
	ListSessions(context.Context, SessionListFilter) ([]Session, error)
	ListSessionEvents(context.Context, string) ([]SessionEvent, error)
	SubscribeSessionEvents(context.Context, string) (SessionEventSubscription, error)
	AttachTerminal(context.Context, string, string) (TerminalAttachment, error)
	CreateTerminal(context.Context, string, CreateTerminalParams) (TerminalInfo, error)
	ListTerminals(context.Context, string) ([]TerminalInfo, error)
	ListSessionTabs(context.Context, string) ([]SessionTab, error)
	KillTerminal(context.Context, string, string) error
}
