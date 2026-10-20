package domain

import (
	"context"
	"time"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the agent message wire types from shared.
type (
	AgentMessage      = sd.AgentMessage
	AgentMessageKind  = sd.AgentMessageKind
	AgentMessageState = sd.AgentMessageState
	AgentMessageList  = sd.AgentMessageList
)

const (
	AgentMessageTicketConcluded = sd.AgentMessageTicketConcluded
	// AgentMessageActionConcluded: an Action agent's approved, persisted
	// conclusion, addressed to the agent that requested the execution.
	// Payload: execution_id, action, outcome, package (bool). Declared here
	// because the kind is produced and reconciled by the daemon alone; the
	// record shape is shared.
	AgentMessageActionConcluded AgentMessageKind = "action_concluded"

	AgentMessagePendingCommit = sd.AgentMessagePendingCommit
	AgentMessagePending       = sd.AgentMessagePending
	AgentMessageHanded        = sd.AgentMessageHanded
	AgentMessageAppeared      = sd.AgentMessageAppeared
	AgentMessageAcknowledged  = sd.AgentMessageAcknowledged
	AgentMessageFailed        = sd.AgentMessageFailed

	AgentMessageRecipientArchitect = sd.AgentMessageRecipientArchitect
	AgentMessageRecipientSession   = sd.AgentMessageRecipientSession
)

// AgentMessageRepository is the durable outbox. Rows are written by one
// dispatcher at a time, so updates replace the row; CreateAgentMessage is the
// one place identity is enforced (dedupe_key UNIQUE → ConflictError).
type AgentMessageRepository interface {
	CreateAgentMessage(context.Context, AgentMessage) error
	GetAgentMessage(context.Context, string) (AgentMessage, error)
	GetAgentMessageByDedupeKey(context.Context, string) (AgentMessage, error)
	UpdateAgentMessage(context.Context, AgentMessage) error
	// ListOpenAgentMessages returns every message still needing delivery
	// work (pending_commit, pending, handed), oldest first.
	ListOpenAgentMessages(context.Context) ([]AgentMessage, error)
	// ListAppearedAgentMessages returns messages that appeared (not yet
	// acknowledged) at or after since, oldest first, so a later reply can
	// upgrade them.
	ListAppearedAgentMessages(context.Context, time.Time) ([]AgentMessage, error)
	// ListAgentMessages returns a project's messages, newest first, at most
	// limit (0: default).
	ListAgentMessages(ctx context.Context, architectKey string, limit int) ([]AgentMessage, error)
	// PruneAgentMessages deletes terminal messages that ended before cutoff.
	PruneAgentMessages(context.Context, time.Time) (int, error)
}
