package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// ActionRunStore implements domain.ActionRunRepository.
type ActionRunStore struct {
	db *sql.DB
}

func NewActionRunStore(db *sql.DB) *ActionRunStore {
	return &ActionRunStore{db: db}
}

const actionRunColumns = `id, action, trigger, status, prompt, profile_name, repo_path, output_dir,
	COALESCE(session_id, ''), COALESCE(summary, ''), COALESCE(error, ''),
	created_at, COALESCE(started_at, ''), COALESCE(ended_at, ''),
	COALESCE(architect_key, ''), COALESCE(requester_session_id, ''), COALESCE(reason, '')`

func (s *ActionRunStore) CreateActionRun(ctx context.Context, run domain.ActionRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create action run tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if run.Status == domain.ActionRunRunning {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM action_runs WHERE action = ? AND status = ? LIMIT 1`,
			run.Action, string(domain.ActionRunRunning)).Scan(&existing)
		if err == nil {
			return busyActionError(run.Action, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check running execution of action %s: %w", run.Action, err)
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO action_runs (id, action, trigger, status, prompt, profile_name, repo_path, output_dir, session_id, summary, error, created_at, started_at, ended_at,
		                         architect_key, requester_session_id, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, run.Action, string(run.Trigger), string(run.Status), run.Prompt, run.ProfileName, run.RepoPath, run.OutputDir,
		nullIfEmpty(run.SessionID), nullIfEmpty(run.Summary), nullIfEmpty(run.Error),
		formatPreciseTime(run.CreatedAt), nullableTime(run.StartedAt), nullableTime(run.EndedAt),
		nullIfEmpty(run.ArchitectKey), nullIfEmpty(run.RequesterSessionID), nullIfEmpty(run.Reason))
	if err != nil {
		// The partial unique index is the backstop for the check above.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: action_runs.action") {
			return busyActionError(run.Action, "")
		}
		return fmt.Errorf("insert action run %s: %w", run.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create action run tx: %w", err)
	}
	return nil
}

func busyActionError(action, runningID string) error {
	msg := fmt.Sprintf("%s is already running", action)
	if runningID != "" {
		msg += " (execution " + runningID + ")"
	}
	return &domain.ConflictError{Resource: "action", Field: "name", Message: msg + "; only one execution of an action may run at a time"}
}

func (s *ActionRunStore) GetActionRun(ctx context.Context, id string) (domain.ActionRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+actionRunColumns+` FROM action_runs WHERE id = ?`, id)
	run, err := scanActionRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ActionRun{}, &domain.NotFoundError{Resource: "action_run", ID: id}
		}
		return domain.ActionRun{}, fmt.Errorf("get action run %s: %w", id, err)
	}
	return run, nil
}

func (s *ActionRunStore) ListActionRuns(ctx context.Context, action string, limit int) ([]domain.ActionRun, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT ` + actionRunColumns + ` FROM action_runs`
	args := []any{}
	if action != "" {
		query += ` WHERE action = ?`
		args = append(args, action)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryActionRuns(ctx, query, args...)
}

func (s *ActionRunStore) RunningActionRuns(ctx context.Context) (map[string]domain.ActionRun, error) {
	runs, err := s.queryActionRuns(ctx, `SELECT `+actionRunColumns+` FROM action_runs WHERE status = ?`, string(domain.ActionRunRunning))
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.ActionRun, len(runs))
	for _, run := range runs {
		out[run.Action] = run
	}
	return out, nil
}

func (s *ActionRunStore) SetActionRunSession(ctx context.Context, id, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE action_runs SET session_id = ? WHERE id = ?`, nullIfEmpty(sessionID), id)
	if err != nil {
		return fmt.Errorf("set action run %s session: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set action run %s session: rows affected: %w", id, err)
	}
	if n == 0 {
		return &domain.NotFoundError{Resource: "action_run", ID: id}
	}
	return nil
}

func (s *ActionRunStore) FinishActionRun(ctx context.Context, id string, status domain.ActionRunStatus, summary, errText string, at time.Time) error {
	if status != domain.ActionRunCompleted && status != domain.ActionRunFailed {
		return fmt.Errorf("finish action run %s: %s is not a finishing status", id, status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_runs SET status = ?, summary = ?, error = ?, ended_at = ?
		WHERE id = ? AND status = ?
	`, string(status), nullIfEmpty(summary), nullIfEmpty(errText), formatPreciseTime(at), id, string(domain.ActionRunRunning))
	if err != nil {
		return fmt.Errorf("finish action run %s as %s: %w", id, status, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish action run %s: rows affected: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	current, err := s.GetActionRun(ctx, id)
	if err != nil {
		return err
	}
	return &domain.ConflictError{
		Resource: "action_run",
		Field:    "status",
		Message:  fmt.Sprintf("action execution %s is %s, not running", id, current.Status),
	}
}

func (s *ActionRunStore) StartActionRun(ctx context.Context, id, profileName string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin start action run tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var action, status string
	if err := tx.QueryRowContext(ctx, `SELECT action, status FROM action_runs WHERE id = ?`, id).Scan(&action, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &domain.NotFoundError{Resource: "action_run", ID: id}
		}
		return fmt.Errorf("read action run %s: %w", id, err)
	}
	if status != string(domain.ActionRunPendingApproval) {
		return notPendingError(id, status)
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM action_runs WHERE action = ? AND status = ? LIMIT 1`,
		action, string(domain.ActionRunRunning)).Scan(&existing)
	if err == nil {
		return busyActionError(action, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check running execution of action %s: %w", action, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE action_runs SET status = ?, profile_name = ?, started_at = ?
		WHERE id = ? AND status = ?
	`, string(domain.ActionRunRunning), profileName, formatPreciseTime(at), id, string(domain.ActionRunPendingApproval)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: action_runs.action") {
			return busyActionError(action, "")
		}
		return fmt.Errorf("start action run %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit start action run tx: %w", err)
	}
	return nil
}

func (s *ActionRunStore) EndPendingActionRun(ctx context.Context, id string, status domain.ActionRunStatus, reason, errText string, at time.Time) error {
	if status != domain.ActionRunDenied && status != domain.ActionRunFailed {
		return fmt.Errorf("end pending action run %s: %s is not an ending status for a request", id, status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_runs SET status = ?, reason = ?, error = ?, ended_at = ?
		WHERE id = ? AND status = ?
	`, string(status), nullIfEmpty(reason), nullIfEmpty(errText), formatPreciseTime(at), id, string(domain.ActionRunPendingApproval))
	if err != nil {
		return fmt.Errorf("end pending action run %s as %s: %w", id, status, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("end pending action run %s: rows affected: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	current, err := s.GetActionRun(ctx, id)
	if err != nil {
		return err
	}
	return notPendingError(id, string(current.Status))
}

func (s *ActionRunStore) FailPendingActionRuns(ctx context.Context, reason string, at time.Time) ([]domain.ActionRun, error) {
	pending, err := s.queryActionRuns(ctx, `SELECT `+actionRunColumns+` FROM action_runs WHERE status = ?`, string(domain.ActionRunPendingApproval))
	if err != nil {
		return nil, err
	}
	failed := make([]domain.ActionRun, 0, len(pending))
	for _, run := range pending {
		err := s.EndPendingActionRun(ctx, run.ID, domain.ActionRunFailed, "", reason, at)
		if err != nil {
			if errors.As(err, new(*domain.ConflictError)) {
				continue
			}
			return failed, err
		}
		failed = append(failed, run)
	}
	return failed, nil
}

func notPendingError(id, status string) error {
	return &domain.ConflictError{
		Resource: "action_run",
		Field:    "status",
		Message:  fmt.Sprintf("action execution %s is %s, not pending_approval", id, status),
	}
}

func (s *ActionRunStore) RecentActionConclusions(ctx context.Context, action string, limit int) ([]domain.ActionConclusion, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status, summary, COALESCE(ended_at, '')
		FROM action_runs
		WHERE action = ? AND summary IS NOT NULL AND summary != '' AND status IN (?, ?)
		ORDER BY ended_at DESC, id DESC
		LIMIT ?
	`, action, string(domain.ActionRunCompleted), string(domain.ActionRunFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("list recent conclusions of action %s: %w", action, err)
	}
	defer func() { _ = rows.Close() }()

	out := []domain.ActionConclusion{}
	for rows.Next() {
		var (
			c             domain.ActionConclusion
			status, ended string
		)
		if err := rows.Scan(&c.ExecutionID, &status, &c.Summary, &ended); err != nil {
			return nil, fmt.Errorf("scan action conclusion: %w", err)
		}
		c.Status = domain.ActionRunStatus(status)
		if c.EndedAt, err = parseSQLiteMaybeTime(ended); err != nil {
			return nil, fmt.Errorf("parse action run %s ended_at: %w", c.ExecutionID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate action conclusions: %w", err)
	}
	return out, nil
}

func (s *ActionRunStore) queryActionRuns(ctx context.Context, query string, args ...any) ([]domain.ActionRun, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query action runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []domain.ActionRun{}
	for rows.Next() {
		run, err := scanActionRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan action run: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate action runs: %w", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanActionRun(row rowScanner) (domain.ActionRun, error) {
	var (
		run                           domain.ActionRun
		trigger, status               string
		createdAt, startedAt, endedAt string
	)
	if err := row.Scan(&run.ID, &run.Action, &trigger, &status, &run.Prompt, &run.ProfileName, &run.RepoPath, &run.OutputDir,
		&run.SessionID, &run.Summary, &run.Error, &createdAt, &startedAt, &endedAt,
		&run.ArchitectKey, &run.RequesterSessionID, &run.Reason); err != nil {
		return domain.ActionRun{}, err
	}
	run.Trigger = domain.ActionRunTrigger(trigger)
	run.Status = domain.ActionRunStatus(status)
	var err error
	if run.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return domain.ActionRun{}, fmt.Errorf("parse action run %s created_at: %w", run.ID, err)
	}
	if run.StartedAt, err = parseSQLiteMaybeTime(startedAt); err != nil {
		return domain.ActionRun{}, fmt.Errorf("parse action run %s started_at: %w", run.ID, err)
	}
	if run.EndedAt, err = parseSQLiteMaybeTime(endedAt); err != nil {
		return domain.ActionRun{}, fmt.Errorf("parse action run %s ended_at: %w", run.ID, err)
	}
	return run, nil
}
