package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

// CompanionStore implements domain.CompanionRepository.
type CompanionStore struct {
	db *sql.DB
}

func NewCompanionStore(db *sql.DB) *CompanionStore {
	return &CompanionStore{db: db}
}

const companionColumns = `architect_session_id, name, architect_key, repo, workdir, variant, agent, machine, session_id, status, error, created_at, updated_at`

func (s *CompanionStore) CreateCompanion(ctx context.Context, c domain.Companion) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO companions (`+companionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ArchitectSessionID, c.Name, c.ArchitectKey, c.Repo, c.Workdir, c.Variant, c.Agent, c.Machine, c.SessionID, string(c.Status), c.Error,
		formatPreciseTime(c.CreatedAt), formatPreciseTime(c.UpdatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return &domain.ConflictError{Resource: "companion", Field: "name", Message: fmt.Sprintf("architect session %s already has companion %s", c.ArchitectSessionID, c.Name)}
		}
		return fmt.Errorf("insert companion %s of architect session %s: %w", c.Name, c.ArchitectSessionID, err)
	}
	return nil
}

func (s *CompanionStore) GetCompanion(ctx context.Context, architectSessionID, name string) (domain.Companion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+companionColumns+` FROM companions WHERE architect_session_id = ? AND name = ?`, architectSessionID, name)
	c, err := scanCompanion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Companion{}, &domain.NotFoundError{Resource: "companion", ID: name}
	}
	if err != nil {
		return domain.Companion{}, fmt.Errorf("get companion %s of architect session %s: %w", name, architectSessionID, err)
	}
	return c, nil
}

func (s *CompanionStore) GetCompanionBySession(ctx context.Context, sessionID string) (domain.Companion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+companionColumns+` FROM companions WHERE session_id = ? AND session_id <> ''`, sessionID)
	c, err := scanCompanion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Companion{}, &domain.NotFoundError{Resource: "companion session", ID: sessionID}
	}
	if err != nil {
		return domain.Companion{}, fmt.Errorf("get companion of session %s: %w", sessionID, err)
	}
	return c, nil
}

func (s *CompanionStore) ListCompanions(ctx context.Context, architectSessionID string) ([]domain.Companion, error) {
	return s.list(ctx, `WHERE architect_session_id = ? ORDER BY name`, architectSessionID)
}

func (s *CompanionStore) ListAllCompanions(ctx context.Context) ([]domain.Companion, error) {
	return s.list(ctx, `ORDER BY architect_session_id, name`)
}

func (s *CompanionStore) list(ctx context.Context, suffix string, args ...any) ([]domain.Companion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+companionColumns+` FROM companions `+suffix, args...)
	if err != nil {
		return nil, fmt.Errorf("list companions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.Companion{}
	for rows.Next() {
		c, err := scanCompanion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan companion: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate companions: %w", err)
	}
	return out, nil
}

func (s *CompanionStore) UpdateCompanion(ctx context.Context, c domain.Companion) error {
	res, err := s.db.ExecContext(ctx, `UPDATE companions SET session_id = ?, agent = ?, status = ?, error = ?, updated_at = ? WHERE architect_session_id = ? AND name = ?`,
		c.SessionID, c.Agent, string(c.Status), c.Error, formatPreciseTime(c.UpdatedAt), c.ArchitectSessionID, c.Name)
	if err != nil {
		return fmt.Errorf("update companion %s of architect session %s: %w", c.Name, c.ArchitectSessionID, err)
	}
	return ensureRowsAffected(res, "companion", c.Name)
}

const shutdownColumns = `request_id, architect_key, architect_session_id, companion_session_id, companion, status, handoff, error, requested_at,
	COALESCE(completed_at, ''), COALESCE(stopped_at, '')`

// CreateShutdown records the request and moves the running companion to
// shutdown_requested atomically, so a request is never recorded for a
// companion that was not running, nor a companion marked without its request.
func (s *CompanionStore) CreateShutdown(ctx context.Context, sd domain.CompanionShutdown) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin shutdown tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE companions SET status = ?, updated_at = ? WHERE architect_session_id = ? AND name = ? AND session_id = ? AND status = ?`,
		string(domain.CompanionShutdownRequested), formatPreciseTime(sd.RequestedAt), sd.ArchitectSessionID, sd.Companion, sd.CompanionSessionID, string(domain.CompanionRunning))
	if err != nil {
		return fmt.Errorf("mark companion %s shutting down: %w", sd.Companion, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("mark companion %s shutting down: %w", sd.Companion, err)
	} else if n == 0 {
		return &domain.ConflictError{Resource: "companion", Field: "status", Message: "companion " + sd.Companion + " is not running in this architect session"}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO companion_shutdowns (request_id, architect_key, architect_session_id, companion_session_id, companion, status, handoff, error, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sd.RequestID, sd.ArchitectKey, sd.ArchitectSessionID, sd.CompanionSessionID, sd.Companion, string(sd.Status), sd.Handoff, sd.Error, formatPreciseTime(sd.RequestedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return &domain.ConflictError{Resource: "companion_shutdown", Field: "request_id", Message: "a shutdown request with this request_id or for this companion session already exists"}
		}
		return fmt.Errorf("insert shutdown request %s: %w", sd.RequestID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit shutdown request %s: %w", sd.RequestID, err)
	}
	return nil
}

func (s *CompanionStore) GetShutdown(ctx context.Context, requestID string) (domain.CompanionShutdown, error) {
	return s.getShutdown(ctx, `request_id = ?`, requestID)
}

func (s *CompanionStore) GetShutdownBySession(ctx context.Context, companionSessionID string) (domain.CompanionShutdown, error) {
	return s.getShutdown(ctx, `companion_session_id = ?`, companionSessionID)
}

func (s *CompanionStore) getShutdown(ctx context.Context, where string, arg string) (domain.CompanionShutdown, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+shutdownColumns+` FROM companion_shutdowns WHERE `+where, arg)
	var sd domain.CompanionShutdown
	var status, requestedAt, completedAt, stoppedAt string
	err := row.Scan(&sd.RequestID, &sd.ArchitectKey, &sd.ArchitectSessionID, &sd.CompanionSessionID, &sd.Companion, &status, &sd.Handoff, &sd.Error,
		&requestedAt, &completedAt, &stoppedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CompanionShutdown{}, &domain.NotFoundError{Resource: "companion_shutdown", ID: arg}
	}
	if err != nil {
		return domain.CompanionShutdown{}, fmt.Errorf("get shutdown request %s: %w", arg, err)
	}
	sd.Status = domain.CompanionShutdownStatus(status)
	if sd.RequestedAt, err = parseSQLiteTime(requestedAt); err != nil {
		return domain.CompanionShutdown{}, fmt.Errorf("parse shutdown requested_at: %w", err)
	}
	if sd.CompletedAt, err = parseSQLiteMaybeTime(completedAt); err != nil {
		return domain.CompanionShutdown{}, fmt.Errorf("parse shutdown completed_at: %w", err)
	}
	if sd.StoppedAt, err = parseSQLiteMaybeTime(stoppedAt); err != nil {
		return domain.CompanionShutdown{}, fmt.Errorf("parse shutdown stopped_at: %w", err)
	}
	return sd, nil
}

func (s *CompanionStore) UpdateShutdown(ctx context.Context, sd domain.CompanionShutdown) error {
	res, err := s.db.ExecContext(ctx, `UPDATE companion_shutdowns SET status = ?, handoff = ?, error = ?, completed_at = ?, stopped_at = ? WHERE request_id = ?`,
		string(sd.Status), sd.Handoff, sd.Error, nullableTime(sd.CompletedAt), nullableTime(sd.StoppedAt), sd.RequestID)
	if err != nil {
		return fmt.Errorf("update shutdown request %s: %w", sd.RequestID, err)
	}
	return ensureRowsAffected(res, "companion_shutdown", sd.RequestID)
}

func scanCompanion(scanner interface{ Scan(...any) error }) (domain.Companion, error) {
	var c domain.Companion
	var status, createdAt, updatedAt string
	if err := scanner.Scan(&c.ArchitectSessionID, &c.Name, &c.ArchitectKey, &c.Repo, &c.Workdir, &c.Variant, &c.Agent, &c.Machine,
		&c.SessionID, &status, &c.Error, &createdAt, &updatedAt); err != nil {
		return domain.Companion{}, err
	}
	c.Status = domain.CompanionStatus(status)
	var err error
	if c.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return domain.Companion{}, fmt.Errorf("parse companion created_at: %w", err)
	}
	if c.UpdatedAt, err = parseSQLiteTime(updatedAt); err != nil {
		return domain.Companion{}, fmt.Errorf("parse companion updated_at: %w", err)
	}
	c.PendingActions = []domain.ActionRequest{}
	return c, nil
}
