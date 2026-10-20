package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// messageTarget is a resolved recipient: the running session a message is
// delivered to, or why there is none right now.
type messageTarget struct {
	session   domain.Session
	run       domain.SessionRun
	agent     agentruntime.AgentKind
	env       map[string]string
	condition string
	// failed is set when the recipient can never be reached again.
	failed string
}

// messageText is what the agent reads. The id is embedded so the provider's
// own record can be searched for it.
func messageText(m domain.AgentMessage) string {
	return "Hiveryn message " + m.ID + ": " + m.Body
}

// dispatchAgentMessages is one pass of the dispatcher: confirm handed
// messages, then deliver the next pending message of each target.
func (s *Service) dispatchAgentMessages(ctx context.Context) error {
	repo := s.messages.repo
	s.upgradeAcknowledged(ctx)
	open, err := repo.ListOpenAgentMessages(ctx)
	if err != nil {
		return err
	}
	if len(open) == 0 {
		return nil
	}
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for message delivery: %w", err)
	}
	inflight := map[string]bool{}
	// Handed messages first, so serialization sees the current state.
	for i := range open {
		m := &open[i]
		if m.State != domain.AgentMessageHanded {
			continue
		}
		before := *m
		s.confirmHandedMessage(ctx, m, sessions)
		if m.State == domain.AgentMessageHanded {
			inflight[m.TargetSessionID] = true
		}
		s.persistMessageChange(ctx, before, *m)
	}
	for i := range open {
		m := &open[i]
		if m.State != domain.AgentMessagePending {
			continue
		}
		before := *m
		target := s.resolveMessageTarget(m.Recipient, sessions)
		switch {
		case target.failed != "":
			s.markMessageFailed(m, target.failed)
		case target.condition != "":
			m.Condition = target.condition
		case inflight[target.session.ID]:
			m.Condition = "waiting for the previous message to this session to appear in its conversation"
		case m.NextAttemptAt != nil && m.NextAttemptAt.After(s.messages.now()):
			// Backing off after a failed attempt; Condition already says so.
		default:
			s.attemptMessageDelivery(ctx, m, target)
			if m.State == domain.AgentMessageHanded {
				inflight[target.session.ID] = true
			}
		}
		s.persistMessageChange(ctx, before, *m)
	}
	return nil
}

// upgradeAcknowledged records the agent's reply for messages that appeared
// recently: appearance is usually confirmed before the reply exists.
func (s *Service) upgradeAcknowledged(ctx context.Context) {
	appeared, err := s.messages.repo.ListAppearedAgentMessages(ctx, s.messages.now().Add(-messageAckWindow))
	if err != nil {
		s.logger.Error("list appeared agent messages", "error", err)
		return
	}
	for _, m := range appeared {
		deliverer, adapter, ok := s.messageDeliverer(agentruntime.AgentKind(m.TargetAgent))
		if !ok {
			continue
		}
		confirmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		receipt, err := s.readMessageReceipt(confirmCtx, deliverer, adapter, m, nil)
		cancel()
		if err != nil || !receipt.Acknowledged {
			continue
		}
		before := m
		now := s.messages.now()
		m.State = domain.AgentMessageAcknowledged
		m.AcknowledgedAt = &now
		s.persistMessageChange(ctx, before, m)
	}
}

func (s *Service) persistMessageChange(ctx context.Context, before, after domain.AgentMessage) {
	if before.State == after.State && before.Condition == after.Condition && before.Attempts == after.Attempts &&
		before.LastError == after.LastError && before.ProviderRef == after.ProviderRef && before.TargetSessionID == after.TargetSessionID {
		return
	}
	after.UpdatedAt = s.messages.now()
	if err := s.messages.repo.UpdateAgentMessage(ctx, after); err != nil {
		s.logger.Error("update agent message", "message_id", after.ID, "state", after.State, "error", err)
		return
	}
	if before.State != after.State {
		s.logger.Info("agent message state", "message_id", after.ID, "kind", after.Kind, "recipient", after.Recipient,
			"from", before.State, "to", after.State, "target_session_id", after.TargetSessionID, "mechanism", after.Mechanism,
			"attempts", after.Attempts, "condition", after.Condition, "last_error", after.LastError)
	}
	s.emitArchitectEvent(after.ArchitectKey, domain.ArchitectEventAgentMessageChanged, "", "")
}

// resolveMessageTarget maps a recipient to its running session.
func (s *Service) resolveMessageTarget(recipient string, sessions []domain.Session) messageTarget {
	var candidate *domain.Session
	switch {
	case strings.HasPrefix(recipient, domain.AgentMessageRecipientArchitect):
		key := strings.TrimPrefix(recipient, domain.AgentMessageRecipientArchitect)
		for i := range sessions {
			sess := &sessions[i]
			if sess.ArchitectKey == key && sess.SessionType == domain.SessionTypeArchitect && sess.CurrentRun != nil && sess.CurrentRun.Status == domain.SessionRunStatusRunning {
				candidate = sess
				break
			}
		}
		if candidate == nil {
			return messageTarget{condition: "no running architect session for " + key + "; delivered to its next session"}
		}
	case strings.HasPrefix(recipient, domain.AgentMessageRecipientSession):
		id := strings.TrimPrefix(recipient, domain.AgentMessageRecipientSession)
		for i := range sessions {
			if sessions[i].ID == id {
				candidate = &sessions[i]
				break
			}
		}
		if candidate == nil {
			return messageTarget{failed: "recipient session " + id + " no longer exists"}
		}
	default:
		return messageTarget{failed: "unknown recipient scheme in " + recipient}
	}
	if candidate.CurrentRun == nil || candidate.CurrentRun.Status != domain.SessionRunStatusRunning {
		return messageTarget{condition: "recipient session " + candidate.ID + " is not running; delivered when it runs again"}
	}
	if candidate.Machine != "" {
		return messageTarget{condition: "recipient session runs on " + config.MachineLabel(candidate.Machine) + "; message delivery to remote sessions is not available, the message is retained"}
	}
	run := *candidate.CurrentRun
	if run.ProfileSnapshot == nil {
		return messageTarget{condition: "recipient run has no agent profile snapshot"}
	}
	kind, err := parseAgentKind(run.ProfileSnapshot.Agent)
	if err != nil {
		return messageTarget{condition: "recipient agent kind: " + err.Error()}
	}
	return messageTarget{session: *candidate, run: run, agent: kind, env: cloneStringMap(run.ProfileSnapshot.Env)}
}

func (s *Service) messageDeliverer(kind agentruntime.AgentKind) (agentruntime.MessageDeliverer, agentruntime.Adapter, bool) {
	adapter := s.adapters[kind]
	deliverer, ok := adapter.(agentruntime.MessageDeliverer)
	return deliverer, adapter, ok
}

// attemptMessageDelivery hands over or pushes one pending message.
func (s *Service) attemptMessageDelivery(ctx context.Context, m *domain.AgentMessage, target messageTarget) {
	deliverer, adapter, ok := s.messageDeliverer(target.agent)
	if !ok {
		m.Condition = fmt.Sprintf("agent %s cannot receive messages; the message is retained", target.agent)
		return
	}
	if target.run.NativeID == "" {
		m.Condition = "the agent's native session id is not known yet (no hook event received); retried on its next event"
		return
	}
	capability := deliverer.MessageCapability()
	idle, _ := s.messageIdle(target.session.ID)
	if m.TargetSessionID != target.session.ID {
		// A new target: provider identities from the previous one mean nothing here.
		m.ProviderRef = ""
	}
	m.TargetSessionID = target.session.ID
	m.TargetAgent = string(target.agent)
	m.TargetNativeID = target.run.NativeID
	m.TargetConfigRoot = adapter.ConfigRoot(target.env)
	m.TargetWorkdir = target.run.Workdir
	m.Mechanism = capability.Mechanism

	switch capability.Mode {
	case agentruntime.MessageDeliveryPull:
		_, armed := s.messages.mailbox.Armed(target.session.ID)
		switch {
		case !armed:
			m.Condition = "no waiter armed: the agent's next turn end or resume arms one (" + capability.Mechanism + "); the message is retained"
			return
		case !idle:
			m.Condition = "agent busy; handed over at its next idle point"
			return
		}
		handed, ok := s.messages.mailbox.Deliver(target.session.ID, ingest.MailboxMessage{ID: m.ID, Text: messageText(*m)})
		if !ok {
			m.Condition = "the waiter left before the hand-off; retried when the next one arms"
			return
		}
		s.setMessageIdle(target.session.ID, false)
		s.markMessageHanded(m, "", "handed to the "+handed.HookEvent+" waiter; awaiting the agent's record")
		return
	case agentruntime.MessageDeliveryPush:
		if capability.GateOnIdle && !idle {
			m.Condition = "agent busy; pushed at its next idle point"
			return
		}
		req := agentruntime.DeliverRequest{
			Env:             target.env,
			NativeSessionID: target.run.NativeID,
			MessageID:       m.ID,
			ProviderRef:     m.ProviderRef,
			Text:            messageText(*m),
		}
		if target.agent == agentruntime.AgentOpenCode {
			endpoint, ok := s.messageEndpoint(target.session.ID)
			if !ok {
				m.Condition = "the session's OpenCode server endpoint is unknown (launched before message delivery or by another daemon); delivered after its next launch"
				return
			}
			req.ServerPort, req.ServerPassword = endpoint.port, endpoint.password
		}
		deliverCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := deliverer.DeliverMessage(deliverCtx, req)
		cancel()
		if err != nil {
			s.recordMessageAttemptFailure(m, err)
			return
		}
		if capability.GateOnIdle {
			s.setMessageIdle(target.session.ID, false)
		}
		condition := "accepted by the provider (" + string(result.Outcome) + "); awaiting the agent's record"
		if !capability.GateOnIdle {
			condition = "queued by the provider; it runs as the agent's next turn once idle"
		}
		s.markMessageHanded(m, result.ProviderRef, condition)
	default:
		m.Condition = fmt.Sprintf("unsupported delivery mode %q", capability.Mode)
	}
}

func (s *Service) markMessageHanded(m *domain.AgentMessage, providerRef, condition string) {
	now := s.messages.now()
	m.State = domain.AgentMessageHanded
	m.Attempts++
	m.HandedAt = &now
	m.NextAttemptAt = nil
	m.LastError = ""
	if providerRef != "" {
		m.ProviderRef = providerRef
	}
	m.Condition = condition
}

func (s *Service) recordMessageAttemptFailure(m *domain.AgentMessage, err error) {
	m.Attempts++
	m.LastError = err.Error()
	delay := time.Duration(m.Attempts) * 30 * time.Second
	if delay > messageRetryBackoff {
		delay = messageRetryBackoff
	}
	next := s.messages.now().Add(delay)
	m.NextAttemptAt = &next
	m.Condition = fmt.Sprintf("delivery attempt %d failed: %v; retrying after %s", m.Attempts, err, delay)
	s.logger.Warn("agent message delivery failed", "message_id", m.ID, "target_session_id", m.TargetSessionID, "attempt", m.Attempts, "error", err)
}

// confirmHandedMessage checks the provider's own record of a handed message
// and decides: appeared (closed), still in flight, or lost (back to pending
// under the same identity).
func (s *Service) confirmHandedMessage(ctx context.Context, m *domain.AgentMessage, sessions []domain.Session) {
	deliverer, adapter, ok := s.messageDeliverer(agentruntime.AgentKind(m.TargetAgent))
	if !ok {
		s.repend(m, "the target agent cannot be inspected")
		return
	}
	var target *domain.Session
	for i := range sessions {
		if sessions[i].ID == m.TargetSessionID {
			target = &sessions[i]
			break
		}
	}
	targetRunning := target != nil && target.CurrentRun != nil && target.CurrentRun.Status == domain.SessionRunStatusRunning &&
		target.CurrentRun.NativeID == m.TargetNativeID
	var env map[string]string
	if target != nil && target.CurrentRun != nil && target.CurrentRun.ProfileSnapshot != nil {
		env = cloneStringMap(target.CurrentRun.ProfileSnapshot.Env)
	}

	confirmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	receipt, confirmErr := s.readMessageReceipt(confirmCtx, deliverer, adapter, *m, env)
	now := s.messages.now()
	if confirmErr == nil && receipt.Appeared {
		m.State = domain.AgentMessageAppeared
		m.AppearedAt = &now
		m.EndedAt = &now
		m.Condition = ""
		if receipt.Acknowledged {
			m.State = domain.AgentMessageAcknowledged
			m.AcknowledgedAt = &now
		}
		return
	}
	handedFor := now.Sub(*m.HandedAt)
	idle, idleAt := s.messageIdle(m.TargetSessionID)
	switch {
	case !targetRunning:
		// The session ended or was relaunched: nothing can confirm a late
		// appearance there, so the message goes to the recipient's next session.
		reason := "the target session ended before the message appeared in its record"
		if confirmErr != nil {
			reason += " (its record could not be read: " + confirmErr.Error() + "; redelivery may repeat it)"
		}
		s.repend(m, reason)
	case confirmErr == nil && receipt.Queued:
		m.Condition = "queued by the provider; it runs as the agent's next turn once idle (after an interrupted turn: after the user's next completed turn)"
	case confirmErr == nil && idle && idleAt.After(m.HandedAt.Add(messageLostAfter)):
		s.repend(m, "not found in the agent's record after its next turn ended")
	case confirmErr == nil && m.Mechanism != "" && !deliverer.MessageCapability().GateOnIdle && handedFor > messageHandedCap:
		s.repend(m, "neither queued nor in the agent's record after "+handedFor.Round(time.Second).String())
	case confirmErr != nil && handedFor > messageHandedCap:
		s.repend(m, "its record could not be read for "+handedFor.Round(time.Second).String()+": "+confirmErr.Error()+"; redelivery may repeat it")
	case confirmErr != nil:
		m.Condition = "handed over; awaiting the agent's record (not readable yet: " + confirmErr.Error() + ")"
	default:
		m.Condition = "handed over; awaiting the agent's record"
	}
}

// repend returns a handed message to pending under the same identity.
func (s *Service) repend(m *domain.AgentMessage, reason string) {
	m.State = domain.AgentMessagePending
	m.LastError = reason
	m.Condition = "redelivering: " + reason
	m.HandedAt = nil
	s.logger.Warn("agent message redelivery", "message_id", m.ID, "target_session_id", m.TargetSessionID, "attempts", m.Attempts, "reason", reason)
}

// readMessageReceipt resolves the target's provider record and asks the
// adapter whether the message is in it.
func (s *Service) readMessageReceipt(ctx context.Context, deliverer agentruntime.MessageDeliverer, adapter agentruntime.Adapter, m domain.AgentMessage, env map[string]string) (agentruntime.MessageReceipt, error) {
	handle, err := adapter.LocateTranscript(ctx, agentruntime.LocateRequest{NativeSessionID: m.TargetNativeID, ConfigRoot: m.TargetConfigRoot, Workdir: m.TargetWorkdir})
	if err != nil && agentruntime.AgentKind(m.TargetAgent) != agentruntime.AgentCodex {
		return agentruntime.MessageReceipt{}, err
	}
	if env == nil {
		env = map[string]string{}
	}
	if m.TargetConfigRoot != "" && agentruntime.AgentKind(m.TargetAgent) == agentruntime.AgentCodex {
		env["CODEX_HOME"] = m.TargetConfigRoot
	}
	receipt, confirmErr := deliverer.ConfirmMessage(ctx, agentruntime.ConfirmRequest{
		TranscriptHandle: handle,
		Env:              env,
		NativeSessionID:  m.TargetNativeID,
		MessageID:        m.ID,
		ProviderRef:      m.ProviderRef,
	})
	if confirmErr != nil {
		return agentruntime.MessageReceipt{}, confirmErr
	}
	if err != nil && !receipt.Queued {
		// Codex without a rollout yet: only the queue could be read.
		return receipt, errors.New("rollout not found yet: " + err.Error())
	}
	return receipt, nil
}
