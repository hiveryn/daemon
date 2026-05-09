package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

const sqliteTimeLayout = "2006-01-02 15:04:05"

type SessionStore struct {
	db *sql.DB
}

func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

func (s *SessionStore) CreateSession(ctx context.Context, params domain.CreateSessionParams) (domain.Session, error) {
	if params.ID == "" {
		params.ID = uuid.NewString()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, profile_name, architect_key, prompt, instructions, status, native_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, params.ID, params.ProfileName, params.ArchitectKey, params.Prompt, params.Instructions, params.Status, nullIfEmpty(params.NativeID))
	if err != nil {
		if params.Status == domain.SessionStatusRunning && isRunningSessionUniqueConstraint(err) {
			return domain.Session{}, &domain.ConflictError{
				Resource: "session",
				Field:    "architect_key",
				Message:  fmt.Sprintf("architect %s already has an active session", params.ArchitectKey),
			}
		}
		return domain.Session{}, fmt.Errorf("insert session: %w", err)
	}

	return s.GetSession(ctx, params.ID)
}

func (s *SessionStore) GetSession(ctx context.Context, id string) (domain.Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, profile_name, architect_key, prompt, instructions, status, COALESCE(native_id, ''), created_at, updated_at
		FROM sessions
		WHERE id = ?
	`, id)

	session, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Session{}, &domain.NotFoundError{Resource: "session", ID: id}
		}
		return domain.Session{}, fmt.Errorf("get session %s: %w", id, err)
	}
	return session, nil
}

func (s *SessionStore) ListSessions(ctx context.Context, filter domain.SessionListFilter) ([]domain.Session, error) {
	query := `
		SELECT id, profile_name, architect_key, prompt, instructions, status, COALESCE(native_id, ''), created_at, updated_at
		FROM sessions
	`
	args := []any{}
	if filter.Status != "" {
		query += ` WHERE status = ?`
		args = append(args, filter.Status)
	}
	query += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sessions := []domain.Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return sessions, nil
}

func (s *SessionStore) UpdateSessionStatus(ctx context.Context, id string, status domain.SessionStatus) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET status = ?, updated_at = datetime('now')
		WHERE id = ?
	`, status, id)
	if err != nil {
		return fmt.Errorf("update session status %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session", id)
}

func (s *SessionStore) UpdateSessionNativeID(ctx context.Context, id, nativeID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET native_id = ?, updated_at = datetime('now')
		WHERE id = ? AND COALESCE(native_id, '') = ''
	`, nullIfEmpty(nativeID), id)
	if err != nil {
		return fmt.Errorf("update session native id %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for session native id %s: %w", id, err)
	}
	if rows == 0 {
		if _, err := s.GetSession(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *SessionStore) DeleteSession(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session", id)
}

func (s *SessionStore) ListSessionEvents(ctx context.Context, sessionID string) ([]domain.SessionEvent, error) {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, seq, type, COALESCE(status, ''), COALESCE(tool, ''), COALESCE(message, ''),
		       COALESCE(native_id, ''), COALESCE(primary_native_id, ''), COALESCE(native_session_role, ''),
		       COALESCE(metadata, ''), COALESCE(raw, ''), at
		FROM session_events
		WHERE session_id = ?
		ORDER BY seq ASC
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session events %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	events := []domain.SessionEvent{}
	for rows.Next() {
		event, err := scanSessionEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session events %s: %w", sessionID, err)
	}
	return events, nil
}

func (s *SessionStore) AppendSessionEvent(ctx context.Context, params domain.AppendSessionEventParams) (domain.SessionEvent, error) {
	if params.SessionID == "" {
		return domain.SessionEvent{}, fmt.Errorf("missing session ID")
	}
	if params.Type == "" {
		params.Type = "status"
	}
	if params.At.IsZero() {
		params.At = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("begin session event tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := ensureSessionExistsTx(ctx, tx, params.SessionID); err != nil {
		return domain.SessionEvent{}, err
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events WHERE session_id = ?`, params.SessionID).Scan(&count); err != nil {
		return domain.SessionEvent{}, fmt.Errorf("count session events: %w", err)
	}
	if count >= 100 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM session_events
			WHERE id = (
				SELECT id FROM session_events
				WHERE session_id = ?
				ORDER BY seq ASC
				LIMIT 1
			)
		`, params.SessionID); err != nil {
			return domain.SessionEvent{}, fmt.Errorf("trim session events: %w", err)
		}
	}

	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM session_events WHERE session_id = ?`, params.SessionID).Scan(&seq); err != nil {
		return domain.SessionEvent{}, fmt.Errorf("next session event seq: %w", err)
	}

	event := domain.SessionEvent{
		ID:                uuid.NewString(),
		SessionID:         params.SessionID,
		Seq:               seq,
		Type:              params.Type,
		Status:            params.Status,
		Tool:              params.Tool,
		Message:           params.Message,
		NativeID:          params.NativeID,
		PrimaryNativeID:   params.PrimaryNativeID,
		NativeSessionRole: params.NativeSessionRole,
		Metadata:          cloneStringMap(params.Metadata),
		Raw:               cloneAnyMap(params.Raw),
		At:                params.At.UTC(),
	}

	metadataJSON, err := marshalJSONText(event.Metadata)
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("marshal session event metadata: %w", err)
	}
	rawJSON, err := marshalJSONText(event.Raw)
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("marshal session event raw: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO session_events (
			id, session_id, seq, type, status, tool, message, native_id, primary_native_id, native_session_role, metadata, raw, at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ID, event.SessionID, event.Seq, event.Type, nullIfEmpty(event.Status), nullIfEmpty(event.Tool),
		nullIfEmpty(event.Message), nullIfEmpty(event.NativeID), nullIfEmpty(event.PrimaryNativeID),
		nullIfEmpty(event.NativeSessionRole), metadataJSON, rawJSON, event.At.Format(time.RFC3339Nano))
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("insert session event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.SessionEvent{}, fmt.Errorf("commit session event tx: %w", err)
	}
	return event, nil
}

func (s *SessionStore) FailRunningSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET status = ?, updated_at = datetime('now')
		WHERE status = ?
	`, domain.SessionStatusFailed, domain.SessionStatusRunning)
	if err != nil {
		return fmt.Errorf("fail running sessions: %w", err)
	}
	return nil
}

func scanSession(scanner interface{ Scan(...any) error }) (domain.Session, error) {
	var session domain.Session
	var status string
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&session.ID,
		&session.ProfileName,
		&session.ArchitectKey,
		&session.Prompt,
		&session.Instructions,
		&status,
		&session.NativeID,
		&createdAt,
		&updatedAt,
	); err != nil {
		return domain.Session{}, err
	}

	parsedCreatedAt, err := parseSQLiteTime(createdAt)
	if err != nil {
		return domain.Session{}, fmt.Errorf("parse created_at: %w", err)
	}
	parsedUpdatedAt, err := parseSQLiteTime(updatedAt)
	if err != nil {
		return domain.Session{}, fmt.Errorf("parse updated_at: %w", err)
	}

	session.Status = domain.SessionStatus(status)
	session.CreatedAt = parsedCreatedAt
	session.UpdatedAt = parsedUpdatedAt
	return session, nil
}

func scanSessionEvent(scanner interface{ Scan(...any) error }) (domain.SessionEvent, error) {
	var event domain.SessionEvent
	var metadataJSON string
	var rawJSON string
	var at string
	if err := scanner.Scan(
		&event.ID,
		&event.SessionID,
		&event.Seq,
		&event.Type,
		&event.Status,
		&event.Tool,
		&event.Message,
		&event.NativeID,
		&event.PrimaryNativeID,
		&event.NativeSessionRole,
		&metadataJSON,
		&rawJSON,
		&at,
	); err != nil {
		return domain.SessionEvent{}, err
	}

	parsedAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("parse event at: %w", err)
	}
	event.At = parsedAt.UTC()
	event.Metadata, err = unmarshalStringMap(metadataJSON)
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("decode event metadata: %w", err)
	}
	event.Raw, err = unmarshalAnyMap(rawJSON)
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("decode event raw: %w", err)
	}
	return event, nil
}

func ensureSessionExistsTx(ctx context.Context, tx *sql.Tx, id string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists)
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.NotFoundError{Resource: "session", ID: id}
	}
	return fmt.Errorf("check session %s: %w", id, err)
}

func ensureRowsAffected(result sql.Result, resource, id string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for %s %s: %w", resource, id, err)
	}
	if rows == 0 {
		return &domain.NotFoundError{Resource: resource, ID: id}
	}
	return nil
}

func parseSQLiteTime(value string) (time.Time, error) {
	parsed, err := time.ParseInLocation(sqliteTimeLayout, value, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func marshalJSONText(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case map[string]string:
		if len(typed) == 0 {
			return nil, nil
		}
	case map[string]any:
		if len(typed) == 0 {
			return nil, nil
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func unmarshalStringMap(value string) (map[string]string, error) {
	if value == "" {
		return map[string]string{}, nil
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, err
	}
	if decoded == nil {
		decoded = map[string]string{}
	}
	return decoded, nil
}

func unmarshalAnyMap(value string) (map[string]any, error) {
	if value == "" {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, err
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	return decoded, nil
}

func cloneAnyMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func isRunningSessionUniqueConstraint(err error) bool {
	message := err.Error()
	return strings.Contains(message, "UNIQUE constraint failed") &&
		strings.Contains(message, "sessions.architect_key")
}
