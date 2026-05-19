package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

type SessionStore struct {
	db *sql.DB
}

func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

func (s *SessionStore) CreateIntent(ctx context.Context, params domain.CreateSessionIntentParams) (domain.SessionIntent, error) {
	if params.ID == "" {
		params.ID = uuid.NewString()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SessionIntent{}, fmt.Errorf("begin create intent tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := ensureNoActiveIntentTx(ctx, tx, params); err != nil {
		return domain.SessionIntent{}, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO session_intents (id, architect_key, session_type, ticket_id, prompt, instructions, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, params.ID, params.ArchitectKey, string(params.SessionType), nullIfEmpty(params.TicketID), nullIfEmpty(params.Prompt), nullIfEmpty(params.Instructions), nullIfEmpty(string(params.CreatedBy)))
	if err != nil {
		return domain.SessionIntent{}, fmt.Errorf("insert session intent: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.SessionIntent{}, fmt.Errorf("commit create intent tx: %w", err)
	}

	return s.GetIntent(ctx, params.ID)
}

func (s *SessionStore) GetIntent(ctx context.Context, id string) (domain.SessionIntent, error) {
	row := s.db.QueryRowContext(ctx, intentWithCurrentRunQuery(`WHERE i.id = ?`), id)
	intent, err := scanSessionIntentWithCurrentRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.SessionIntent{}, &domain.NotFoundError{Resource: "session_intent", ID: id}
		}
		return domain.SessionIntent{}, fmt.Errorf("get session intent %s: %w", id, err)
	}
	return intent, nil
}

func (s *SessionStore) ListIntents(ctx context.Context) ([]domain.SessionIntent, error) {
	rows, err := s.db.QueryContext(ctx, intentWithCurrentRunQuery(`ORDER BY i.created_at DESC, i.id DESC`))
	if err != nil {
		return nil, fmt.Errorf("list session intents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	intents := []domain.SessionIntent{}
	for rows.Next() {
		intent, err := scanSessionIntentWithCurrentRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session intent: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session intents: %w", err)
	}
	return intents, nil
}

func (s *SessionStore) DeleteIntent(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM session_intents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session intent %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session_intent", id)
}

func intentWithCurrentRunQuery(suffix string) string {
	return `
		SELECT i.id, i.architect_key, i.session_type, COALESCE(i.ticket_id, ''), COALESCE(i.prompt, ''), COALESCE(i.instructions, ''),
		       COALESCE(i.created_by, ''), i.created_at, i.updated_at,
		       r.id, r.session_intent_id, r.status, r.profile_name, COALESCE(r.profile_snapshot, ''), COALESCE(r.workdir, ''),
		       COALESCE(r.native_id, ''), COALESCE(r.failure_reason, ''), COALESCE(r.started_at, ''), COALESCE(r.ended_at, ''),
		       COALESCE(r.created_at, ''), COALESCE(r.updated_at, '')
		FROM session_intents i
		LEFT JOIN session_runs r ON r.id = (
			SELECT sr.id
			FROM session_runs sr
			WHERE sr.session_intent_id = i.id
			ORDER BY CASE WHEN sr.status = 'running' THEN 0 ELSE 1 END, sr.created_at DESC, sr.id DESC
			LIMIT 1
		)
	` + suffix
}

func ensureNoActiveIntentTx(ctx context.Context, tx *sql.Tx, params domain.CreateSessionIntentParams) error {
	switch params.SessionType {
	case domain.SessionTypeArchitect:
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM session_intents WHERE architect_key = ? AND session_type = ? LIMIT 1`, params.ArchitectKey, string(domain.SessionTypeArchitect)).Scan(&existingID)
		if err == nil {
			return &domain.ConflictError{
				Resource: "session_intent",
				Field:    "architect_key",
				Message:  fmt.Sprintf("architect %s already has an active architect intent", params.ArchitectKey),
			}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check active architect intent: %w", err)
		}
	case domain.SessionTypeWork:
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM session_intents WHERE ticket_id = ? AND session_type = ? LIMIT 1`, params.TicketID, string(domain.SessionTypeWork)).Scan(&existingID)
		if err == nil {
			return &domain.ConflictError{
				Resource: "session_intent",
				Field:    "ticket_id",
				Message:  fmt.Sprintf("ticket %s already has an active work intent", params.TicketID),
			}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check active work intent: %w", err)
		}
	}
	return nil
}
