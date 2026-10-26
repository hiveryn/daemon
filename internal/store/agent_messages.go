package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// AgentMessageStore implements domain.AgentMessageRepository.
type AgentMessageStore struct {
	db *sql.DB
}

func NewAgentMessageStore(db *sql.DB) *AgentMessageStore {
	return &AgentMessageStore{db: db}
}

const agentMessageColumns = `id, kind, recipient, architect_key, dedupe_key, body, COALESCE(payload, ''), state, condition,
	target_session_id, target_agent, target_native_id, target_config_root, target_workdir, mechanism, provider_ref, attempts, last_error,
	COALESCE(next_attempt_at, ''), created_at, COALESCE(committed_at, ''), COALESCE(handed_at, ''), COALESCE(appeared_at, ''),
	COALESCE(acknowledged_at, ''), COALESCE(ended_at, ''), updated_at`

const defaultAgentMessageListLimit = 100

func (s *AgentMessageStore) CreateAgentMessage(ctx context.Context, m domain.AgentMessage) error {
	payload, err := marshalNullableJSON(m.Payload, len(m.Payload) == 0)
	if err != nil {
		return fmt.Errorf("marshal agent message payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_messages (id, kind, recipient, architect_key, dedupe_key, body, payload, state, condition,
			target_session_id, target_agent, target_native_id, target_config_root, target_workdir, mechanism, provider_ref, attempts, last_error,
			next_attempt_at, created_at, committed_at, handed_at, appeared_at, acknowledged_at, ended_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, string(m.Kind), m.Recipient, m.ArchitectKey, m.DedupeKey, m.Body, payload, string(m.State), m.Condition,
		m.TargetSessionID, m.TargetAgent, m.TargetNativeID, m.TargetConfigRoot, m.TargetWorkdir, m.Mechanism, m.ProviderRef, m.Attempts, m.LastError,
		nullableTime(m.NextAttemptAt), formatPreciseTime(m.CreatedAt), nullableTime(m.CommittedAt), nullableTime(m.HandedAt),
		nullableTime(m.AppearedAt), nullableTime(m.AcknowledgedAt), nullableTime(m.EndedAt), formatPreciseTime(m.UpdatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: agent_messages.dedupe_key") {
			return &domain.ConflictError{Resource: "agent_message", Field: "dedupe_key", Message: "a message with dedupe key " + m.DedupeKey + " already exists"}
		}
		return fmt.Errorf("insert agent message %s: %w", m.ID, err)
	}
	return nil
}

func (s *AgentMessageStore) GetAgentMessage(ctx context.Context, id string) (domain.AgentMessage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+agentMessageColumns+` FROM agent_messages WHERE id = ?`, id)
	m, err := scanAgentMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentMessage{}, &domain.NotFoundError{Resource: "agent_message", ID: id}
	}
	return m, err
}

func (s *AgentMessageStore) GetAgentMessageByDedupeKey(ctx context.Context, key string) (domain.AgentMessage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+agentMessageColumns+` FROM agent_messages WHERE dedupe_key = ?`, key)
	m, err := scanAgentMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentMessage{}, &domain.NotFoundError{Resource: "agent_message", ID: key}
	}
	return m, err
}

func (s *AgentMessageStore) UpdateAgentMessage(ctx context.Context, m domain.AgentMessage) error {
	payload, err := marshalNullableJSON(m.Payload, len(m.Payload) == 0)
	if err != nil {
		return fmt.Errorf("marshal agent message payload: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_messages SET kind = ?, recipient = ?, architect_key = ?, body = ?, payload = ?, state = ?, condition = ?,
			target_session_id = ?, target_agent = ?, target_native_id = ?, target_config_root = ?, target_workdir = ?, mechanism = ?, provider_ref = ?, attempts = ?, last_error = ?,
			next_attempt_at = ?, committed_at = ?, handed_at = ?, appeared_at = ?, acknowledged_at = ?, ended_at = ?, updated_at = ?
		WHERE id = ?
	`, string(m.Kind), m.Recipient, m.ArchitectKey, m.Body, payload, string(m.State), m.Condition,
		m.TargetSessionID, m.TargetAgent, m.TargetNativeID, m.TargetConfigRoot, m.TargetWorkdir, m.Mechanism, m.ProviderRef, m.Attempts, m.LastError,
		nullableTime(m.NextAttemptAt), nullableTime(m.CommittedAt), nullableTime(m.HandedAt), nullableTime(m.AppearedAt),
		nullableTime(m.AcknowledgedAt), nullableTime(m.EndedAt), formatPreciseTime(m.UpdatedAt), m.ID)
	if err != nil {
		return fmt.Errorf("update agent message %s: %w", m.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update agent message %s: rows affected: %w", m.ID, err)
	}
	if n == 0 {
		return &domain.NotFoundError{Resource: "agent_message", ID: m.ID}
	}
	return nil
}

func (s *AgentMessageStore) ListOpenAgentMessages(ctx context.Context) ([]domain.AgentMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentMessageColumns+` FROM agent_messages
		WHERE state IN (?, ?, ?) ORDER BY created_at ASC, id ASC`,
		string(domain.AgentMessagePendingCommit), string(domain.AgentMessagePending), string(domain.AgentMessageHanded))
	if err != nil {
		return nil, fmt.Errorf("list open agent messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectAgentMessages(rows)
}

func (s *AgentMessageStore) ListAppearedAgentMessages(ctx context.Context, since time.Time) ([]domain.AgentMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentMessageColumns+` FROM agent_messages
		WHERE state = ? AND appeared_at >= ? ORDER BY created_at ASC, id ASC`, string(domain.AgentMessageAppeared), formatPreciseTime(since))
	if err != nil {
		return nil, fmt.Errorf("list appeared agent messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectAgentMessages(rows)
}

func (s *AgentMessageStore) ListAgentMessages(ctx context.Context, architectKey string, limit int) ([]domain.AgentMessage, error) {
	if limit <= 0 {
		limit = defaultAgentMessageListLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentMessageColumns+` FROM agent_messages
		WHERE architect_key = ? ORDER BY created_at DESC, id DESC LIMIT ?`, architectKey, limit)
	if err != nil {
		return nil, fmt.Errorf("list agent messages of %s: %w", architectKey, err)
	}
	defer func() { _ = rows.Close() }()
	return collectAgentMessages(rows)
}

func (s *AgentMessageStore) PruneAgentMessages(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_messages WHERE ended_at IS NOT NULL AND ended_at < ?
		AND state IN (?, ?, ?)`, formatPreciseTime(cutoff),
		string(domain.AgentMessageAppeared), string(domain.AgentMessageAcknowledged), string(domain.AgentMessageFailed))
	if err != nil {
		return 0, fmt.Errorf("prune agent messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune agent messages: rows affected: %w", err)
	}
	return int(n), nil
}

func collectAgentMessages(rows *sql.Rows) ([]domain.AgentMessage, error) {
	out := []domain.AgentMessage{}
	for rows.Next() {
		m, err := scanAgentMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent messages: %w", err)
	}
	return out, nil
}

func scanAgentMessage(scanner interface{ Scan(...any) error }) (domain.AgentMessage, error) {
	var (
		m                                                                                      domain.AgentMessage
		kind, state, payload                                                                   string
		nextAttempt, createdAt, committedAt, handedAt, appearedAt, ackedAt, endedAt, updatedAt string
	)
	if err := scanner.Scan(&m.ID, &kind, &m.Recipient, &m.ArchitectKey, &m.DedupeKey, &m.Body, &payload, &state, &m.Condition,
		&m.TargetSessionID, &m.TargetAgent, &m.TargetNativeID, &m.TargetConfigRoot, &m.TargetWorkdir, &m.Mechanism, &m.ProviderRef, &m.Attempts, &m.LastError,
		&nextAttempt, &createdAt, &committedAt, &handedAt, &appearedAt, &ackedAt, &endedAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentMessage{}, err
		}
		return domain.AgentMessage{}, fmt.Errorf("scan agent message: %w", err)
	}
	m.Kind = domain.AgentMessageKind(kind)
	m.State = domain.AgentMessageState(state)
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &m.Payload); err != nil {
			return domain.AgentMessage{}, fmt.Errorf("decode agent message %s payload: %w", m.ID, err)
		}
	}
	var err error
	if m.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return domain.AgentMessage{}, fmt.Errorf("parse agent message %s created_at: %w", m.ID, err)
	}
	if m.UpdatedAt, err = parseSQLiteTime(updatedAt); err != nil {
		return domain.AgentMessage{}, fmt.Errorf("parse agent message %s updated_at: %w", m.ID, err)
	}
	for _, field := range []struct {
		raw string
		dst **time.Time
	}{{nextAttempt, &m.NextAttemptAt}, {committedAt, &m.CommittedAt}, {handedAt, &m.HandedAt}, {appearedAt, &m.AppearedAt}, {ackedAt, &m.AcknowledgedAt}, {endedAt, &m.EndedAt}} {
		if *field.dst, err = parseSQLiteMaybeTime(field.raw); err != nil {
			return domain.AgentMessage{}, fmt.Errorf("parse agent message %s time: %w", m.ID, err)
		}
	}
	return m, nil
}

func marshalNullableJSON(v any, empty bool) (any, error) {
	if empty {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatPreciseTime(*t)
}
