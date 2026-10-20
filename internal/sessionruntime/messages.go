package sessionruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/domain"
)

// Agent messages (shared domain/message.go): a durable outbox of text the
// daemon delivers into running agent sessions through each provider's native
// route — Claude's asyncRewake waiter (pull), Codex's thread queue and
// OpenCode's session server (push) — never by typing into a terminal.
//
// Producers (messages_conclusion.go: ticket conclusions to the architect;
// messages_action.go: the end of an agent-requested Action execution to the
// requesting agent) call BeginAgentMessage (state pending_commit) before a
// non-transactional side effect and CommitAgentMessage — or
// settleAgentMessage, which re-reads the record through the kind's check —
// after it, or EnqueueAgentMessage for a one-shot. A single dispatcher goroutine owns every
// state change after that: it resolves the recipient to a running session,
// gates on idle where the provider needs it, hands over or pushes, confirms
// the provider's own receipt before any redelivery, backs off on transport
// failures and records why a message is not progressing (Condition). Nothing
// here launches a session: a message addressed to an architect with no running
// session waits for its next one. Open messages survive restart in SQLite;
// pending_commit rows are reconciled against their producer's side effect.
//
// Delivery is effectively once: one message is in flight per target session,
// identity is stable across retries, and the provider's record is consulted
// before redelivery — both when deciding that a handed message is lost and
// again right before sending a previously handed message once more. An
// absence counts only when the record reaches past the hand-off
// (MessageReceipt.RecordedThrough); a record that ends before it is stale
// evidence and waits. Hand-offs are bounded (messageMaxHandoffs): a message
// that cannot be confirmed after that many is failed visibly rather than sent
// again. The windows that remain are documented in each adapter's
// MessageCapability.Coverage.

const (
	// messageTick is the dispatcher's periodic pass (receipt checks, retries).
	messageTick = 30 * time.Second
	// messageRetention bounds how long terminal messages stay listed.
	messageRetention = 30 * 24 * time.Hour
	// messageCommitGrace is how old a pending_commit row must be before a
	// periodic pass reconciles it; the producer's Exec may still be running.
	messageCommitGrace = 5 * time.Minute
	// messageLostAfter is how long a handed message may stay unconfirmed
	// after the target went idle again before it is redelivered.
	messageLostAfter = 10 * time.Second
	// messageHandedCap redelivers a handed message that could not be
	// confirmed at all for this long (no transcript, no idle event).
	messageHandedCap = 15 * time.Minute
	// messageRetryBackoff caps the retry delay after a failed attempt.
	messageRetryBackoff = 5 * time.Minute
	// messageAckWindow is how long after appearing a message is still checked
	// for the agent's reply (appeared → acknowledged).
	messageAckWindow = 10 * time.Minute
	// messageMaxHandoffs bounds redelivery: a message handed over this many
	// times without a confirmed receipt is failed with the reason instead of
	// being sent again. Redelivery recovers a genuinely lost message (a
	// Claude process killed before running it, a Codex queue row gone); a
	// message that keeps vanishing from every record the daemon can read is
	// a receipt-lookup problem, and every further send would reach the
	// agent as a new copy.
	messageMaxHandoffs = 3
)

// messageEndpoint is the per-session OpenCode server reachability minted at
// launch (in memory: a local relaunch mints a new one).
type messageEndpoint struct {
	port     int
	password string
}

type messageRuntime struct {
	repo    domain.AgentMessageRepository
	mailbox *ingest.Mailbox

	mu sync.Mutex
	// idle holds the gate per session: true after the primary agent's turn
	// ended (Stop / session.idle) or a Claude waiter armed by a resumed or
	// cleared SessionStart, false once a turn starts. idleAt is when it last
	// turned true.
	idle      map[string]bool
	idleAt    map[string]time.Time
	endpoints map[string]messageEndpoint
	// failures counts consecutive transport failures per message for the
	// retry backoff; Attempts on the row counts hand-offs only.
	failures map[string]int
	// commitChecks reconcile pending_commit rows per kind.
	commitChecks map[domain.AgentMessageKind]messageCommitCheck

	kick chan struct{}
	stop chan struct{}
	once sync.Once
	done chan struct{}
	now  func() time.Time
	tick time.Duration
}

// messageCommitCheck decides what a pending_commit row has become: committed
// (the side effect is durable), failed (it demonstrably is not and never will
// be, reason), or still unresolved (condition, kept as is).
type messageCommitCheck func(ctx context.Context, m domain.AgentMessage) messageCommitVerdict

type messageCommitVerdict struct {
	committed bool
	failed    string
	condition string
	// body and payload, when set on a committed verdict, replace the text
	// the producer wrote at begin time: the announced change is read back
	// from its durable record, so the notice says what actually happened.
	body    string
	payload map[string]any
}

func newMessageRuntime() *messageRuntime {
	return &messageRuntime{
		mailbox:      ingest.NewMailbox(),
		idle:         map[string]bool{},
		idleAt:       map[string]time.Time{},
		endpoints:    map[string]messageEndpoint{},
		failures:     map[string]int{},
		commitChecks: map[domain.AgentMessageKind]messageCommitCheck{},
		kick:         make(chan struct{}, 1),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		now:          func() time.Time { return time.Now().UTC() },
		tick:         messageTick,
	}
}

// SetAgentMessageRepository wires the durable outbox; without it producers
// fail loudly rather than dropping a message.
func (s *Service) SetAgentMessageRepository(repo domain.AgentMessageRepository) {
	if s.messages == nil {
		s.messages = newMessageRuntime()
		s.messages.mailbox.SetObservers(s.waiterArmed, s.waiterGone)
	}
	s.messages.repo = repo
	s.messages.commitChecks[domain.AgentMessageTicketConcluded] = s.checkTicketConcludedCommit
	s.messages.commitChecks[domain.AgentMessageActionEnded] = s.checkActionEndedCommit
}

// errAgentMessagesNotConfigured: the Service was built without a message
// repository (tests, or a misconfigured daemon); production wires one in app.
var errAgentMessagesNotConfigured = errors.New("agent message repository not configured")

func (s *Service) messageRepo() (domain.AgentMessageRepository, error) {
	if s.messages == nil || s.messages.repo == nil {
		return nil, errAgentMessagesNotConfigured
	}
	return s.messages.repo, nil
}

// AgentMessageParams is what a producer supplies.
type AgentMessageParams struct {
	Kind         domain.AgentMessageKind
	Recipient    string
	ArchitectKey string
	DedupeKey    string
	Body         string
	Payload      map[string]any
}

func (p AgentMessageParams) validate() error {
	switch {
	case p.Kind == "":
		return errors.New("agent message kind is required")
	case !strings.HasPrefix(p.Recipient, domain.AgentMessageRecipientArchitect) && !strings.HasPrefix(p.Recipient, domain.AgentMessageRecipientSession):
		return fmt.Errorf("agent message recipient %q must be architect:<key> or session:<id>", p.Recipient)
	case strings.TrimSpace(p.DedupeKey) == "":
		return errors.New("agent message dedupe key is required")
	case strings.TrimSpace(p.Body) == "":
		return errors.New("agent message body is required")
	}
	return nil
}

// BeginAgentMessage records the message as pending_commit: it exists, carries
// its identity, and is not delivered until CommitAgentMessage. A second call
// with the same dedupe key returns the existing message. Call it before the
// side effect the message announces, so a crash in between leaves a row to
// reconcile instead of a lost announcement.
func (s *Service) BeginAgentMessage(ctx context.Context, params AgentMessageParams) (domain.AgentMessage, error) {
	repo, err := s.messageRepo()
	if err != nil {
		return domain.AgentMessage{}, err
	}
	if err := params.validate(); err != nil {
		return domain.AgentMessage{}, &domain.ValidationError{Field: "message", Message: err.Error()}
	}
	now := s.messages.now()
	m := domain.AgentMessage{
		ID:           uuid.NewString(),
		Kind:         params.Kind,
		Recipient:    params.Recipient,
		ArchitectKey: params.ArchitectKey,
		DedupeKey:    params.DedupeKey,
		Body:         params.Body,
		Payload:      cloneAnyMap(params.Payload),
		State:        domain.AgentMessagePendingCommit,
		Condition:    "waiting for the announced change to be persisted",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := repo.CreateAgentMessage(ctx, m); err != nil {
		var conflict *domain.ConflictError
		if errors.As(err, &conflict) {
			existing, getErr := repo.GetAgentMessageByDedupeKey(ctx, params.DedupeKey)
			if getErr != nil {
				return domain.AgentMessage{}, fmt.Errorf("agent message %s exists but cannot be read: %w", params.DedupeKey, getErr)
			}
			return existing, nil
		}
		return domain.AgentMessage{}, err
	}
	s.emitArchitectEvent(m.ArchitectKey, domain.ArchitectEventAgentMessageChanged, "", "")
	return m, nil
}

// CommitAgentMessage makes a pending_commit message deliverable. It is
// idempotent for a message already past pending_commit.
func (s *Service) CommitAgentMessage(ctx context.Context, id string) error {
	repo, err := s.messageRepo()
	if err != nil {
		return err
	}
	m, err := repo.GetAgentMessage(ctx, id)
	if err != nil {
		return err
	}
	if m.State != domain.AgentMessagePendingCommit {
		return nil
	}
	s.markCommitted(&m)
	if err := repo.UpdateAgentMessage(ctx, m); err != nil {
		return err
	}
	s.emitArchitectEvent(m.ArchitectKey, domain.ArchitectEventAgentMessageChanged, "", "")
	s.kickMessages()
	return nil
}

func (s *Service) markCommitted(m *domain.AgentMessage) {
	now := s.messages.now()
	m.State = domain.AgentMessagePending
	m.Condition = ""
	m.CommittedAt = &now
	m.UpdatedAt = now
}

// settleAgentMessage resolves one pending_commit message against its kind's
// reconciliation check, as the periodic pass would, right after the producer
// wrote the side effect it announces. Producers whose final text depends on
// the written record use it instead of CommitAgentMessage. It is idempotent
// for a message already past pending_commit.
func (s *Service) settleAgentMessage(ctx context.Context, id string) error {
	repo, err := s.messageRepo()
	if err != nil {
		return err
	}
	m, err := repo.GetAgentMessage(ctx, id)
	if err != nil {
		return err
	}
	if m.State != domain.AgentMessagePendingCommit {
		return nil
	}
	check := s.messages.commitChecks[m.Kind]
	if check == nil {
		return fmt.Errorf("no reconciliation for kind %q", m.Kind)
	}
	s.applyCommitVerdict(&m, check(ctx, m))
	if err := repo.UpdateAgentMessage(ctx, m); err != nil {
		return err
	}
	s.emitArchitectEvent(m.ArchitectKey, domain.ArchitectEventAgentMessageChanged, "", "")
	if m.State == domain.AgentMessagePending {
		s.kickMessages()
	}
	return nil
}

// applyCommitVerdict moves a pending_commit message to what its check found.
func (s *Service) applyCommitVerdict(m *domain.AgentMessage, verdict messageCommitVerdict) {
	switch {
	case verdict.committed:
		if verdict.body != "" {
			m.Body = verdict.body
			m.Payload = cloneAnyMap(verdict.payload)
		}
		s.markCommitted(m)
		s.logger.Info("agent message committed", "message_id", m.ID, "kind", m.Kind, "recipient", m.Recipient)
	case verdict.failed != "":
		s.markMessageFailed(m, verdict.failed)
		s.logger.Warn("agent message failed by its check", "message_id", m.ID, "kind", m.Kind, "reason", verdict.failed)
	default:
		m.Condition = verdict.condition
		m.UpdatedAt = s.messages.now()
	}
}

// EnqueueAgentMessage records a deliverable message in one step.
func (s *Service) EnqueueAgentMessage(ctx context.Context, params AgentMessageParams) (domain.AgentMessage, error) {
	m, err := s.BeginAgentMessage(ctx, params)
	if err != nil {
		return domain.AgentMessage{}, err
	}
	if err := s.CommitAgentMessage(ctx, m.ID); err != nil {
		return domain.AgentMessage{}, err
	}
	return s.messages.repo.GetAgentMessage(ctx, m.ID)
}

// ListAgentMessages returns a project's messages, newest first.
func (s *Service) ListAgentMessages(ctx context.Context, architectKey string) ([]domain.AgentMessage, error) {
	repo, err := s.messageRepo()
	if err != nil {
		return nil, err
	}
	return repo.ListAgentMessages(ctx, architectKey, 0)
}

// messageDeliveryFor returns the launch plumbing for a local session: Claude
// and Codex need none beyond the flag; OpenCode gets a fresh loopback port and
// password remembered for this process's lifetime.
func (s *Service) messageDeliveryFor(sessionID string, agentKind agentruntime.AgentKind) (*agentruntime.MessageDelivery, error) {
	if s.messages == nil {
		return nil, nil
	}
	delivery := &agentruntime.MessageDelivery{}
	if agentKind != agentruntime.AgentOpenCode {
		return delivery, nil
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("reserve opencode server port: %w", err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("mint opencode server password: %w", err)
	}
	delivery.ServerPort = port
	delivery.ServerPassword = hex.EncodeToString(secret)
	s.messages.mu.Lock()
	s.messages.endpoints[sessionID] = messageEndpoint{port: port, password: delivery.ServerPassword}
	s.messages.mu.Unlock()
	return delivery, nil
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func (s *Service) messageEndpoint(sessionID string) (messageEndpoint, bool) {
	if s.messages == nil {
		return messageEndpoint{}, false
	}
	s.messages.mu.Lock()
	defer s.messages.mu.Unlock()
	e, ok := s.messages.endpoints[sessionID]
	return e, ok
}

// observeAgentStatusForMessages feeds the idle gate from the primary agent's
// hook events; subagent events never count.
func (s *Service) observeAgentStatusForMessages(event agentruntime.Event) {
	if s.messages == nil || event.NativeSessionRole == agentruntime.NativeSessionRoleSubsession {
		return
	}
	switch event.Status {
	case agentruntime.StatusIdle:
		s.setMessageIdle(event.ID, true)
	case agentruntime.StatusWorking, agentruntime.StatusAwaitingInput, agentruntime.StatusEnded, agentruntime.StatusError:
		s.setMessageIdle(event.ID, false)
	default:
		return
	}
	s.kickMessages()
}

func (s *Service) setMessageIdle(sessionID string, idle bool) {
	if s.messages == nil {
		return
	}
	s.messages.mu.Lock()
	defer s.messages.mu.Unlock()
	s.messages.idle[sessionID] = idle
	if idle {
		s.messages.idleAt[sessionID] = s.messages.now()
	}
}

func (s *Service) messageIdle(sessionID string) (bool, time.Time) {
	if s.messages == nil {
		return false, time.Time{}
	}
	s.messages.mu.Lock()
	defer s.messages.mu.Unlock()
	return s.messages.idle[sessionID], s.messages.idleAt[sessionID]
}

// forgetMessageSession drops a session's gate and endpoint when it ends.
func (s *Service) forgetMessageSession(sessionID string) {
	if s.messages == nil {
		return
	}
	s.messages.mu.Lock()
	delete(s.messages.idle, sessionID)
	delete(s.messages.idleAt, sessionID)
	delete(s.messages.endpoints, sessionID)
	s.messages.mu.Unlock()
	s.kickMessages()
}

// waiterArmed is the mailbox observer: a waiter armed by Stop, or by a
// SessionStart that resumes or clears a conversation, means the agent sits at
// its prompt. A startup SessionStart does not: the kickoff turn follows.
func (s *Service) waiterArmed(w ingest.Waiter) {
	switch {
	case w.HookEvent == "Stop":
		s.setMessageIdle(w.SessionID, true)
	case w.HookEvent == "SessionStart" && (w.Source == "resume" || w.Source == "clear"):
		s.setMessageIdle(w.SessionID, true)
	}
	s.kickMessages()
}

func (s *Service) waiterGone(ingest.Waiter) { s.kickMessages() }

func (s *Service) kickMessages() {
	if s.messages == nil {
		return
	}
	select {
	case s.messages.kick <- struct{}{}:
	default:
	}
}

// StartMessageDelivery reconciles the outbox once and starts the dispatcher.
// Call it after sessions are restored and before serving requests.
func (s *Service) StartMessageDelivery(ctx context.Context) error {
	if _, err := s.messageRepo(); err != nil {
		return err
	}
	if err := s.reconcileAgentMessages(ctx, true); err != nil {
		return err
	}
	if n, err := s.messages.repo.PruneAgentMessages(ctx, s.messages.now().Add(-messageRetention)); err != nil {
		s.logger.Error("prune agent messages", "error", err)
	} else if n > 0 {
		s.logger.Info("pruned agent messages", "count", n)
	}
	go s.runMessageDispatcher(context.WithoutCancel(ctx))
	s.kickMessages()
	return nil
}

func (s *Service) stopMessageDelivery() {
	if s.messages == nil {
		return
	}
	s.messages.once.Do(func() {
		close(s.messages.stop)
		s.messages.mailbox.Close()
	})
}

func (s *Service) runMessageDispatcher(ctx context.Context) {
	defer close(s.messages.done)
	ticker := time.NewTicker(s.messages.tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.messages.stop:
			return
		case <-s.messages.kick:
		case <-ticker.C:
			if err := s.reconcileAgentMessages(ctx, false); err != nil {
				s.logger.Error("reconcile agent messages", "error", err)
			}
		}
		if err := s.dispatchAgentMessages(ctx); err != nil {
			s.logger.Error("dispatch agent messages", "error", err)
		}
	}
}

// reconcileAgentMessages settles pending_commit rows: all of them at startup,
// only those older than messageCommitGrace afterwards.
func (s *Service) reconcileAgentMessages(ctx context.Context, startup bool) error {
	repo := s.messages.repo
	open, err := repo.ListOpenAgentMessages(ctx)
	if err != nil {
		return err
	}
	now := s.messages.now()
	for _, m := range open {
		if m.State != domain.AgentMessagePendingCommit {
			continue
		}
		if !startup && now.Sub(m.CreatedAt) < messageCommitGrace {
			continue
		}
		check := s.messages.commitChecks[m.Kind]
		if check == nil {
			s.setMessageCondition(ctx, &m, fmt.Sprintf("no reconciliation for kind %q; left unresolved", m.Kind))
			continue
		}
		s.applyCommitVerdict(&m, check(ctx, m))
		if err := repo.UpdateAgentMessage(ctx, m); err != nil {
			return err
		}
		s.emitArchitectEvent(m.ArchitectKey, domain.ArchitectEventAgentMessageChanged, "", "")
	}
	return nil
}

func (s *Service) markMessageFailed(m *domain.AgentMessage, reason string) {
	now := s.messages.now()
	m.State = domain.AgentMessageFailed
	m.LastError = reason
	m.Condition = ""
	m.NextAttemptAt = nil
	m.EndedAt = &now
	m.UpdatedAt = now
}

func (s *Service) setMessageCondition(ctx context.Context, m *domain.AgentMessage, condition string) {
	if m.Condition == condition {
		return
	}
	m.Condition = condition
	m.UpdatedAt = s.messages.now()
	if err := s.messages.repo.UpdateAgentMessage(ctx, *m); err != nil {
		s.logger.Error("record agent message condition", "message_id", m.ID, "error", err)
		return
	}
	s.emitArchitectEvent(m.ArchitectKey, domain.ArchitectEventAgentMessageChanged, "", "")
}
