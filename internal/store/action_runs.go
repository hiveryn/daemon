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
	COALESCE(architect_key, ''), COALESCE(requester_session_id, ''), COALESCE(reason, ''),
	COALESCE(requester_ticket_id, ''), machine, COALESCE(delivery_dir, ''), artifact_contract, artifact_mode`

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
		                         architect_key, requester_session_id, reason, requester_ticket_id, machine, artifact_contract, artifact_mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, run.Action, string(run.Trigger), string(run.Status), run.Prompt, run.ProfileName, run.RepoPath, run.OutputDir,
		nullIfEmpty(run.SessionID), nullIfEmpty(run.Summary), nullIfEmpty(run.Error),
		formatPreciseTime(run.CreatedAt), nullableTime(run.StartedAt), nullableTime(run.EndedAt),
		nullIfEmpty(run.ArchitectKey), nullIfEmpty(run.RequesterSessionID), nullIfEmpty(run.Reason), nullIfEmpty(run.RequesterTicketID),
		run.Machine, run.ArtifactContract, domain.EffectiveArtifactMode(run.ArtifactMode, run.ArtifactContract))
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
		UPDATE action_runs SET notification_pending = CASE WHEN started_at IS NOT NULL AND COALESCE(architect_key, '') != '' AND trigger IN ('architect', 'worker') THEN 1 ELSE 0 END, status = ?, summary = ?, error = ?, ended_at = ?
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

func (s *ActionRunStore) StartActionRun(ctx context.Context, id string, start domain.ActionRunStart) error {
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
		UPDATE action_runs SET status = ?, profile_name = ?, started_at = ?, machine = ?, repo_path = ?, output_dir = ?, artifact_contract = ?, artifact_mode = ?
		WHERE id = ? AND status = ?
	`, string(domain.ActionRunRunning), start.ProfileName, formatPreciseTime(start.At), start.Machine, start.RepoPath, start.OutputDir, start.ArtifactContract, domain.EffectiveArtifactMode(start.ArtifactMode, start.ArtifactContract),
		id, string(domain.ActionRunPendingApproval)); err != nil {
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
		&run.ArchitectKey, &run.RequesterSessionID, &run.Reason, &run.RequesterTicketID, &run.Machine, &run.DeliveryDir, &run.ArtifactContract, &run.ArtifactMode); err != nil {
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

func (s *ActionRunStore) PublishActionConclusion(ctx context.Context, id string, status domain.ActionRunStatus, summary string, pkg *domain.ActionArtifactRecord, at time.Time) error {
	if status != domain.ActionRunCompleted && status != domain.ActionRunFailed {
		return fmt.Errorf("publish action conclusion: invalid outcome %q", status)
	}
	if pkg != nil && (pkg.ExecutionID != id || pkg.Kind != domain.ActionArtifactPackage || pkg.State != domain.ActionArtifactsReady) {
		return fmt.Errorf("complete action run %s: package record must be the execution's ready package", id)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete action run tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE action_runs SET notification_pending = CASE WHEN started_at IS NOT NULL AND COALESCE(architect_key, '') != '' AND trigger IN ('architect', 'worker') THEN 1 ELSE 0 END, status = ?, summary = ?, ended_at = ?
		WHERE id = ? AND status = ?
	`, string(status), nullIfEmpty(summary), formatPreciseTime(at), id, string(domain.ActionRunRunning))
	if err != nil {
		return fmt.Errorf("complete action run %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete action run %s: rows affected: %w", id, err)
	}
	if n != 1 {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM action_runs WHERE id = ?`, id).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &domain.NotFoundError{Resource: "action_run", ID: id}
			}
			return fmt.Errorf("read action run %s: %w", id, err)
		}
		return &domain.ConflictError{Resource: "action_run", Field: "status", Message: fmt.Sprintf("action execution %s is %s, not running", id, status)}
	}
	if pkg != nil {
		if err := putActionArtifact(ctx, tx, *pkg); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit complete action run tx: %w", err)
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *ActionRunStore) PutActionArtifact(ctx context.Context, rec domain.ActionArtifactRecord) error {
	return putActionArtifact(ctx, s.db, rec)
}

func putActionArtifact(ctx context.Context, db execer, rec domain.ActionArtifactRecord) error {
	if rec.ExecutionID == "" || rec.Path == "" || (rec.Kind != domain.ActionArtifactPackage && rec.Kind != domain.ActionArtifactDelivery && !strings.HasPrefix(string(rec.Kind), "staging:")) {
		return fmt.Errorf("put action artifact: incomplete record %+v", rec)
	}
	_, err := db.ExecContext(ctx, `
		INSERT OR REPLACE INTO action_artifacts (execution_id, kind, machine, ssh, path, managed_root, state, error, cleanup_error, files, bytes, created_at, ready_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.ExecutionID, string(rec.Kind), rec.Machine, rec.SSH, rec.Path, rec.ManagedRoot, string(rec.State),
		nullIfEmpty(rec.Error), nullIfEmpty(rec.CleanupError), rec.Files, rec.Bytes,
		formatPreciseTime(rec.CreatedAt), nullableTime(rec.ReadyAt), nullableTime(rec.ExpiresAt))
	if err != nil {
		return fmt.Errorf("put %s artifact of action run %s on %q: %w", rec.Kind, rec.ExecutionID, rec.Machine, err)
	}
	return nil
}

const actionArtifactColumns = `execution_id, kind, machine, ssh, path, managed_root, state, COALESCE(error, ''), COALESCE(cleanup_error, ''),
	files, bytes, created_at, COALESCE(ready_at, ''), COALESCE(expires_at, '')`

func (s *ActionRunStore) ActionArtifacts(ctx context.Context, executionID string) ([]domain.ActionArtifactRecord, error) {
	return s.queryActionArtifacts(ctx, `SELECT `+actionArtifactColumns+` FROM action_artifacts WHERE execution_id = ? ORDER BY kind DESC, machine`, executionID)
}

func (s *ActionRunStore) ActionArtifactsToClean(ctx context.Context, now time.Time) ([]domain.ActionArtifactRecord, error) {
	// expires_at is stored in formatPreciseTime's fixed-width UTC form, so the
	// string comparison is chronological.
	return s.queryActionArtifacts(ctx, `SELECT `+actionArtifactColumns+` FROM action_artifacts
		WHERE managed_root != '' AND (
			state = ? OR (cleanup_error IS NOT NULL AND cleanup_error != '' AND state != ?)
			OR (state IN (?, ?) AND expires_at IS NOT NULL AND expires_at <= ?)
			OR (state = 'prepared' AND execution_id IN (SELECT id FROM action_runs WHERE status != 'running')))
		ORDER BY machine, execution_id, kind`,
		string(domain.ActionArtifactsCopying), string(domain.ActionArtifactsExpired),
		string(domain.ActionArtifactsReady), string(domain.ActionArtifactsFailed), formatPreciseTime(now.UTC()))
}

func (s *ActionRunStore) queryActionArtifacts(ctx context.Context, query string, args ...any) ([]domain.ActionArtifactRecord, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query action artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.ActionArtifactRecord{}
	for rows.Next() {
		var (
			rec                     domain.ActionArtifactRecord
			kind, state             string
			created, ready, expires string
		)
		if err := rows.Scan(&rec.ExecutionID, &kind, &rec.Machine, &rec.SSH, &rec.Path, &rec.ManagedRoot, &state, &rec.Error, &rec.CleanupError,
			&rec.Files, &rec.Bytes, &created, &ready, &expires); err != nil {
			return nil, fmt.Errorf("scan action artifact: %w", err)
		}
		rec.Kind, rec.State = domain.ActionArtifactKind(kind), domain.ActionArtifactState(state)
		if rec.CreatedAt, err = parseSQLiteTime(created); err != nil {
			return nil, fmt.Errorf("parse action artifact %s created_at: %w", rec.ExecutionID, err)
		}
		if rec.ReadyAt, err = parseSQLiteMaybeTime(ready); err != nil {
			return nil, fmt.Errorf("parse action artifact %s ready_at: %w", rec.ExecutionID, err)
		}
		if rec.ExpiresAt, err = parseSQLiteMaybeTime(expires); err != nil {
			return nil, fmt.Errorf("parse action artifact %s expires_at: %w", rec.ExecutionID, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate action artifacts: %w", err)
	}
	return out, nil
}

// PrepareActionPackage atomically swaps the current prepared package. The candidate
// and replaced package retain ownership records for bounded startup cleanup.
func (s *ActionRunStore) PrepareActionPackage(ctx context.Context, rec domain.ActionArtifactRecord) error {
	if !strings.HasPrefix(string(rec.Kind), "staging:") || rec.State != domain.ActionArtifactsReady || rec.Files < 1 {
		return fmt.Errorf("prepare package: candidate must be a ready nonempty staging snapshot")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin prepare package: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM action_runs WHERE id = ?", rec.ExecutionID).Scan(&status); err != nil {
		return fmt.Errorf("read preparing execution: %w", err)
	}
	if status != string(domain.ActionRunRunning) {
		return &domain.ConflictError{Resource: "action_run", Message: "execution ended during preparation"}
	}
	// The old package assumes the candidate's unique cleanup key; the new
	// package becomes current. All three changes commit together.
	if _, err := tx.ExecContext(ctx, "DELETE FROM action_artifacts WHERE execution_id = ? AND kind = ? AND machine = ?", rec.ExecutionID, string(rec.Kind), rec.Machine); err != nil {
		return fmt.Errorf("remove candidate record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE action_artifacts SET kind = ?, state = 'copying' WHERE execution_id = ? AND kind = 'package'", string(rec.Kind), rec.ExecutionID); err != nil {
		return fmt.Errorf("retire previous package: %w", err)
	}
	rec.Kind = domain.ActionArtifactPackage
	rec.State = domain.ActionArtifactsPrepared
	rec.ExpiresAt = nil
	if err := putActionArtifact(ctx, tx, rec); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit prepared package: %w", err)
	}
	return nil
}

// PendingActionNotifications lists only explicit obligations from terminal writes,
// never historical executions or requests that did not run.
func (s *ActionRunStore) PendingActionNotifications(ctx context.Context) ([]domain.ActionRun, error) {
	return s.queryActionRuns(ctx, `SELECT `+actionRunColumns+` FROM action_runs WHERE notification_pending = 1`)
}

func (s *ActionRunStore) ClearActionNotification(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE action_runs SET notification_pending = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear notification obligation for %s: %w", id, err)
	}
	return nil
}
