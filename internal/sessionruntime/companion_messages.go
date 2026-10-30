package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

// Architect ↔ companion messages. Each call durably queues one message in the
// agent-message outbox, addressed to the exact counterpart session, and
// returns its id once committed ("accepted" = queued, not delivered or read).
// Sender, recipient and ownership come from the calling session and the
// companion records; a reply's target is derived from the message it answers,
// which must have been addressed to the caller. Delivery, receipts and the
// idle gate are the outbox's.

// companionMessageSendable reports whether a companion can still exchange
// messages: until its shutdown completes, it may report results.
func companionMessageSendable(status domain.CompanionStatus) bool {
	return status == domain.CompanionRunning || status == domain.CompanionShutdownRequested
}

// SendMessageToArchitect is the companion's sendMessageToArchitect.
func (s *Service) SendMessageToArchitect(ctx context.Context, companionSessionID string, req domain.CompanionMessageRequest) (domain.CompanionMessageResult, error) {
	text, err := domain.NormalizeCompanionText("message", req.Message, domain.MaxCompanionMessageLength)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	_, c, err := s.companionOf(ctx, companionSessionID)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	if !companionMessageSendable(c.Status) {
		return domain.CompanionMessageResult{}, &domain.ConflictError{Resource: "companion", Field: "status", Message: "companion " + c.Name + " is " + string(c.Status) + "; it can no longer send messages"}
	}
	id := uuid.NewString()
	body := fmt.Sprintf("From companion %s (message_id %s; answer with replyToCompanionMessage):\n\n%s", c.Name, id, text)
	return s.queueCompanionMessage(ctx, id, domain.AgentMessageCompanionMessage, c, c.ArchitectSessionID, body, "")
}

// ReplyToArchitectMessage is the companion's replyToArchitectMessage: the
// referenced message must be an architect message delivered to this companion.
func (s *Service) ReplyToArchitectMessage(ctx context.Context, companionSessionID string, req domain.CompanionReplyRequest) (domain.CompanionMessageResult, error) {
	text, err := domain.NormalizeCompanionText("message", req.Message, domain.MaxCompanionMessageLength)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	_, c, err := s.companionOf(ctx, companionSessionID)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	original, err := s.referencedMessage(ctx, req.MessageID, domain.AgentMessageArchitectMessage, companionSessionID, "an architect message received by this companion")
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	if !companionMessageSendable(c.Status) {
		return domain.CompanionMessageResult{}, &domain.ConflictError{Resource: "companion", Field: "status", Message: "companion " + c.Name + " is " + string(c.Status) + "; it can no longer send messages"}
	}
	id := uuid.NewString()
	body := fmt.Sprintf("Reply from companion %s to your message %s (message_id %s; answer with replyToCompanionMessage):\n\n%s", c.Name, original.ID, id, text)
	return s.queueCompanionMessage(ctx, id, domain.AgentMessageCompanionMessage, c, c.ArchitectSessionID, body, original.ID)
}

// SendMessageToCompanion is the architect's sendMessageToCompanion: a new
// assignment or message to a named companion of the calling session.
func (s *Service) SendMessageToCompanion(ctx context.Context, architectSessionID, companion string, req domain.CompanionMessageRequest) (domain.CompanionMessageResult, error) {
	text, err := domain.NormalizeCompanionText("message", req.Message, domain.MaxCompanionMessageLength)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	if _, err := s.architectOf(ctx, architectSessionID); err != nil {
		return domain.CompanionMessageResult{}, err
	}
	c, err := s.namedCompanion(ctx, architectSessionID, companion)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	switch c.Status {
	case domain.CompanionRunning:
	case domain.CompanionShutdownRequested, domain.CompanionStopping:
		return domain.CompanionMessageResult{}, &domain.ConflictError{Resource: "companion", Field: "status", Message: "companion " + c.Name + " is shutting down and accepts no new assignments"}
	default:
		return domain.CompanionMessageResult{}, &domain.ConflictError{Resource: "companion", Field: "status", Message: fmt.Sprintf("companion %s is not available (%s)%s", c.Name, c.Status, errorSuffix(c.Error))}
	}
	id := uuid.NewString()
	body := fmt.Sprintf("From the architect (message_id %s; answer with replyToArchitectMessage):\n\n%s", id, text)
	return s.queueCompanionMessage(ctx, id, domain.AgentMessageArchitectMessage, c, c.SessionID, body, "")
}

// ReplyToCompanionMessage is the architect's replyToCompanionMessage: the
// referenced message must be a companion message delivered to this architect
// session; its sender is the reply's recipient.
func (s *Service) ReplyToCompanionMessage(ctx context.Context, architectSessionID string, req domain.CompanionReplyRequest) (domain.CompanionMessageResult, error) {
	text, err := domain.NormalizeCompanionText("message", req.Message, domain.MaxCompanionMessageLength)
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	if _, err := s.architectOf(ctx, architectSessionID); err != nil {
		return domain.CompanionMessageResult{}, err
	}
	original, err := s.referencedMessage(ctx, req.MessageID, domain.AgentMessageCompanionMessage, architectSessionID, "a companion message received by this architect session")
	if err != nil {
		return domain.CompanionMessageResult{}, err
	}
	senderSession, _ := original.Payload["companion_session_id"].(string)
	c, err := s.companionRepo.GetCompanionBySession(ctx, senderSession)
	if err != nil || c.ArchitectSessionID != architectSessionID {
		return domain.CompanionMessageResult{}, &domain.NotFoundError{Resource: "companion of message", ID: original.ID}
	}
	if !companionMessageSendable(c.Status) {
		return domain.CompanionMessageResult{}, &domain.ConflictError{Resource: "companion", Field: "status", Message: fmt.Sprintf("companion %s is %s; the reply cannot be delivered%s", c.Name, c.Status, errorSuffix(c.Error))}
	}
	id := uuid.NewString()
	body := fmt.Sprintf("Reply from the architect to your message %s (message_id %s; answer with replyToArchitectMessage):\n\n%s", original.ID, id, text)
	return s.queueCompanionMessage(ctx, id, domain.AgentMessageArchitectMessage, c, c.SessionID, body, original.ID)
}

// queueCompanionMessage records one message to recipientSession and returns
// once it is committed to the outbox.
func (s *Service) queueCompanionMessage(ctx context.Context, id string, kind domain.AgentMessageKind, c domain.Companion, recipientSession, body, inReplyTo string) (domain.CompanionMessageResult, error) {
	payload := map[string]any{
		"architect_session_id": c.ArchitectSessionID,
		"companion_session_id": c.SessionID,
		"companion":            c.Name,
	}
	if inReplyTo != "" {
		payload["in_reply_to"] = inReplyTo
	}
	m, err := s.EnqueueAgentMessage(context.WithoutCancel(ctx), AgentMessageParams{
		ID:           id,
		Kind:         kind,
		Recipient:    domain.AgentMessageRecipientSession + recipientSession,
		ArchitectKey: c.ArchitectKey,
		DedupeKey:    string(kind) + ":" + id,
		Body:         body,
		Payload:      payload,
	})
	if err != nil {
		return domain.CompanionMessageResult{}, fmt.Errorf("queue the message: %w; it was NOT queued", err)
	}
	return domain.CompanionMessageResult{MessageID: m.ID, InReplyTo: inReplyTo, Status: domain.CompanionAccepted}, nil
}

// referencedMessage returns the message a reply answers: it must exist, be
// of kind and have been addressed to recipientSession. Anything else is the
// same NotFound, so a reply cannot probe another session's messages.
func (s *Service) referencedMessage(ctx context.Context, messageID string, kind domain.AgentMessageKind, recipientSession, what string) (domain.AgentMessage, error) {
	if strings.TrimSpace(messageID) == "" {
		return domain.AgentMessage{}, &domain.ValidationError{Field: "message_id", Message: "is required"}
	}
	repo, err := s.messageRepo()
	if err != nil {
		return domain.AgentMessage{}, err
	}
	m, err := repo.GetAgentMessage(ctx, strings.TrimSpace(messageID))
	if err != nil && !errors.As(err, new(*domain.NotFoundError)) {
		return domain.AgentMessage{}, err
	}
	if err != nil || m.Kind != kind || m.Recipient != domain.AgentMessageRecipientSession+recipientSession {
		return domain.AgentMessage{}, &domain.NotFoundError{Resource: "message_id (" + what + ")", ID: messageID}
	}
	return m, nil
}

// namedCompanion resolves a companion name within the architect session; the
// error names the session's companions.
func (s *Service) namedCompanion(ctx context.Context, architectSessionID, name string) (domain.Companion, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Companion{}, &domain.ValidationError{Field: "companion", Message: "is required"}
	}
	c, err := s.companionRepo.GetCompanion(ctx, architectSessionID, name)
	if err == nil {
		return c, nil
	}
	if !errors.As(err, new(*domain.NotFoundError)) {
		return domain.Companion{}, err
	}
	names := []string{}
	if all, listErr := s.companionRepo.ListCompanions(ctx, architectSessionID); listErr == nil {
		for _, other := range all {
			names = append(names, other.Name)
		}
	}
	known := "this architect session has no companions"
	if len(names) > 0 {
		known = "this architect session's companions: " + strings.Join(names, ", ")
	}
	return domain.Companion{}, &domain.NotFoundError{Resource: "companion", ID: name + " (" + known + ")"}
}

func errorSuffix(err string) string {
	if err == "" {
		return ""
	}
	return ": " + err
}
