package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

func (s *SessionStore) CreateSession(ctx context.Context, params domain.CreateSessionParams) (domain.Session, error) {
	if params.ID == "" {
		params.ID = uuid.NewString()
	}
	if params.AdditionalRepos == nil {
		params.AdditionalRepos = []string{}
	}
	if params.AdditionalWorkdirs == nil {
		params.AdditionalWorkdirs = []string{}
	}
	additionalRepos, err := json.Marshal(params.AdditionalRepos)
	if err != nil {
		return domain.Session{}, fmt.Errorf("marshal session additional repos: %w", err)
	}
	additionalWorkdirs, err := json.Marshal(params.AdditionalWorkdirs)
	if err != nil {
		return domain.Session{}, fmt.Errorf("marshal session additional workdirs: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Session{}, fmt.Errorf("begin create session tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := ensureNoActiveSessionTx(ctx, tx, params); err != nil {
		return domain.Session{}, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions (id, architect_key, session_type, context_id, prompt, workdir, additional_repos, additional_workdirs, instructions, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, params.ID, params.ArchitectKey, string(params.SessionType), params.ContextID, params.Prompt, params.Workdir, string(additionalRepos), string(additionalWorkdirs), nullIfEmpty(params.Instructions), nullIfEmpty(string(params.CreatedBy)))
	if err != nil {
		return domain.Session{}, fmt.Errorf("insert session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.Session{}, fmt.Errorf("commit create session tx: %w", err)
	}

	return s.GetSession(ctx, params.ID)
}

func (s *SessionStore) GetSession(ctx context.Context, id string) (domain.Session, error) {
	row := s.db.QueryRowContext(ctx, intentWithCurrentRunQuery(`WHERE i.id = ?`), id)
	session, err := scanSessionWithCurrentRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Session{}, &domain.NotFoundError{Resource: "session", ID: id}
		}
		return domain.Session{}, fmt.Errorf("get session %s: %w", id, err)
	}
	return session, nil
}

func (s *SessionStore) ListSessions(ctx context.Context) ([]domain.Session, error) {
	rows, err := s.db.QueryContext(ctx, intentWithCurrentRunQuery(`ORDER BY i.created_at DESC, i.id DESC`))
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sessions := []domain.Session{}
	for rows.Next() {
		session, err := scanSessionWithCurrentRun(rows)
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

func (s *SessionStore) DeleteSession(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session", id)
}

func intentWithCurrentRunQuery(suffix string) string {
	return `
		SELECT i.id, i.architect_key, i.session_type, i.context_id, i.prompt, i.workdir, i.additional_repos, i.additional_workdirs, COALESCE(i.instructions, ''),
		       COALESCE(i.created_by, ''), i.created_at, i.updated_at,
		       r.id, r.session_id, r.status, COALESCE(r.agent_status, ''), r.profile_name, COALESCE(r.profile_snapshot, ''), COALESCE(r.workdir, ''), COALESCE(r.additional_repos, '[]'), COALESCE(r.additional_workdirs, '[]'),
		       COALESCE(r.native_id, ''), COALESCE(r.failure_reason, ''), COALESCE(r.started_at, ''), COALESCE(r.ended_at, ''),
		       COALESCE(r.created_at, ''), COALESCE(r.updated_at, '')
		FROM sessions i
		LEFT JOIN session_runs r ON r.id = (
			SELECT sr.id
			FROM session_runs sr
			WHERE sr.session_id = i.id
			ORDER BY CASE WHEN sr.status = 'running' THEN 0 ELSE 1 END, sr.created_at DESC, sr.id DESC
			LIMIT 1
		)
	` + suffix
}

const activeSessionSQL = `(
	NOT EXISTS (
		SELECT 1
		FROM session_runs sr
		WHERE sr.session_id = i.id
	)
	OR EXISTS (
		SELECT 1
		FROM session_runs sr
		WHERE sr.session_id = i.id
		  AND sr.status = ?
	)
)`

func ensureNoActiveSessionTx(ctx context.Context, tx *sql.Tx, params domain.CreateSessionParams) error {
	switch params.SessionType {
	case domain.SessionTypeArchitect:
		var existingID string
		err := tx.QueryRowContext(ctx, `
			SELECT i.id
			FROM sessions i
			WHERE i.architect_key = ?
			  AND i.session_type = ?
			  AND `+activeSessionSQL+`
			LIMIT 1
		`, params.ArchitectKey, string(domain.SessionTypeArchitect), string(domain.SessionRunStatusRunning)).Scan(&existingID)
		if err == nil {
			return &domain.ConflictError{
				Resource: "session",
				Field:    "architect_key",
				Message:  fmt.Sprintf("architect %s already has an active architect session", params.ArchitectKey),
			}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check active architect session: %w", err)
		}
	case domain.SessionTypeTicket:
		var existingID string
		err := tx.QueryRowContext(ctx, `
			SELECT i.id
			FROM sessions i
			WHERE i.context_id = ?
			  AND i.session_type = ?
			  AND `+activeSessionSQL+`
			LIMIT 1
		`, params.ContextID, string(domain.SessionTypeTicket), string(domain.SessionRunStatusRunning)).Scan(&existingID)
		if err == nil {
			return &domain.ConflictError{
				Resource: "session",
				Field:    "ticket_id",
				Message:  fmt.Sprintf("ticket %s already has an active ticket session", params.ContextID),
			}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check active ticket session: %w", err)
		}
	}
	return nil
}

func ensureNoActiveSiblingSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var architectKey, sessionType, contextID string
	if err := tx.QueryRowContext(ctx, `
		SELECT architect_key, session_type, context_id
		FROM sessions
		WHERE id = ?
	`, sessionID).Scan(&architectKey, &sessionType, &contextID); err != nil {
		return fmt.Errorf("get session %s for run creation: %w", sessionID, err)
	}

	var (
		existingID string
		err        error
	)
	switch domain.SessionType(sessionType) {
	case domain.SessionTypeArchitect:
		err = tx.QueryRowContext(ctx, `
			SELECT i.id
			FROM sessions i
			WHERE i.id <> ?
			  AND i.architect_key = ?
			  AND i.session_type = ?
			  AND `+activeSessionSQL+`
			LIMIT 1
		`, sessionID, architectKey, string(domain.SessionTypeArchitect), string(domain.SessionRunStatusRunning)).Scan(&existingID)
	case domain.SessionTypeTicket:
		err = tx.QueryRowContext(ctx, `
			SELECT i.id
			FROM sessions i
			WHERE i.id <> ?
			  AND i.context_id = ?
			  AND i.session_type = ?
			  AND `+activeSessionSQL+`
			LIMIT 1
		`, sessionID, contextID, string(domain.SessionTypeTicket), string(domain.SessionRunStatusRunning)).Scan(&existingID)
	default:
		return nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check active sibling session for %s: %w", sessionID, err)
	}

	field := "architect_key"
	message := fmt.Sprintf("architect %s already has an active architect session", architectKey)
	if domain.SessionType(sessionType) == domain.SessionTypeTicket {
		field = "ticket_id"
		message = fmt.Sprintf("ticket %s already has an active ticket session", contextID)
	}
	return &domain.ConflictError{
		Resource: "session",
		Field:    field,
		Message:  message,
	}
}
