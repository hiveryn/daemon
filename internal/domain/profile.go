package domain

import (
	"context"
	"time"
)

type AgentKind string

const (
	AgentKindClaude   AgentKind = "claude"
	AgentKindCodex    AgentKind = "codex"
	AgentKindOpenCode AgentKind = "opencode"
)

func (k AgentKind) Valid() bool {
	switch k {
	case AgentKindClaude, AgentKindCodex, AgentKindOpenCode:
		return true
	default:
		return false
	}
}

type AgentProfile struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	AgentKind AgentKind         `json:"agent_kind"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type AgentProfileInput struct {
	Name      string            `json:"name"`
	AgentKind AgentKind         `json:"agent_kind"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
}

type ProfileRepository interface {
	List(ctx context.Context) ([]AgentProfile, error)
	Create(ctx context.Context, input AgentProfileInput) (AgentProfile, error)
	Get(ctx context.Context, id string) (AgentProfile, error)
	Update(ctx context.Context, id string, input AgentProfileInput) (AgentProfile, error)
	Delete(ctx context.Context, id string) error
}
