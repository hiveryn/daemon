package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// Ticket-conclusion notices: the first agent message producer. Only a
// worker's approved conclusion notifies (an architect's own moveTicketToDone
// does not); the message is begun before the conclusion files are written and
// committed once the ticket is in done, so a crash in between leaves a
// pending_commit row that reconciliation settles against the ticket itself.

func ticketConcludedDedupeKey(architectKey, ticketID string, concludedAt time.Time) string {
	return fmt.Sprintf("%s:%s:%s:%s", domain.AgentMessageTicketConcluded, architectKey, ticketID, concludedAt.UTC().Format(time.RFC3339Nano))
}

func ticketConcludedBody(ticketID string, outcome domain.TicketOutcome) string {
	return fmt.Sprintf("Ticket %s has been concluded. Outcome: %s. Use readTicket to review its conclusion.", ticketID, outcome)
}

// beginTicketConcludedMessage records the pending_commit notice for a
// conclusion about to be written.
func (s *Service) beginTicketConcludedMessage(ctx context.Context, session domain.Session, conclusion domain.TicketConclusion) (domain.AgentMessage, error) {
	return s.BeginAgentMessage(ctx, AgentMessageParams{
		Kind:         domain.AgentMessageTicketConcluded,
		Recipient:    domain.AgentMessageRecipientArchitect + session.ArchitectKey,
		ArchitectKey: session.ArchitectKey,
		DedupeKey:    ticketConcludedDedupeKey(session.ArchitectKey, session.ContextID, conclusion.ConcludedAt),
		Body:         ticketConcludedBody(session.ContextID, conclusion.Outcome),
		Payload: map[string]any{
			"ticket_id":    session.ContextID,
			"outcome":      string(conclusion.Outcome),
			"concluded_at": conclusion.ConcludedAt.UTC().Format(time.RFC3339Nano),
			"session_id":   session.ID,
		},
	})
}

// checkTicketConcludedCommit settles a pending_commit notice against the
// ticket on disk: done with this conclusion → committed; done with another →
// superseded; gone → failed; otherwise the partial state is reported and kept.
func (s *Service) checkTicketConcludedCommit(ctx context.Context, m domain.AgentMessage) messageCommitVerdict {
	ticketID, _ := m.Payload["ticket_id"].(string)
	concludedRaw, _ := m.Payload["concluded_at"].(string)
	concludedAt, err := time.Parse(time.RFC3339Nano, concludedRaw)
	if ticketID == "" || err != nil {
		return messageCommitVerdict{failed: "malformed ticket_concluded payload: ticket id or concluded_at missing"}
	}
	architect, err := s.currentArchitect(m.ArchitectKey)
	if err != nil {
		return messageCommitVerdict{condition: "architect " + m.ArchitectKey + " cannot be resolved: " + err.Error()}
	}
	ticket, err := s.tickets.GetTicket(ctx, architect.Path, ticketID)
	if err != nil {
		if errors.As(err, new(*domain.NotFoundError)) {
			return messageCommitVerdict{failed: "ticket " + ticketID + " no longer exists"}
		}
		return messageCommitVerdict{condition: "ticket " + ticketID + " cannot be read: " + err.Error()}
	}
	matches := ticket.Conclusion != nil && ticket.Conclusion.ConcludedAt.Equal(concludedAt)
	switch {
	case ticket.Status == domain.TicketStatusDone && matches:
		return messageCommitVerdict{committed: true}
	case ticket.Status == domain.TicketStatusDone:
		return messageCommitVerdict{failed: "superseded: the ticket is done with another conclusion"}
	case matches:
		return messageCommitVerdict{condition: fmt.Sprintf("conclusion.md was written but the ticket is still in %s: partial conclusion write; a later conclusion supersedes this notice", ticket.Status)}
	default:
		return messageCommitVerdict{condition: fmt.Sprintf("conclusion not persisted; the ticket is in %s", ticket.Status)}
	}
}
