package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/domain"
)

func (s *SessionStore) CreateRun(ctx context.Context, params domain.CreateSessionRunParams) (domain.SessionRun, error) {
	if params.ID == "" {
		params.ID = uuid.NewString()
	}
	if params.AdditionalRepos == nil {
		params.AdditionalRepos = []string{}
	}
	if params.AdditionalWorkdirs == nil {
		params.AdditionalWorkdirs = []string{}
	}
	if params.StartedAt.IsZero() {
		params.StartedAt = time.Now().UTC()
	}
	createdAt := time.Now().UTC()

	profileSnapshotJSON, err := marshalJSONText(params.ProfileSnapshot)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("marshal profile snapshot: %w", err)
	}
	additionalReposJSON, err := marshalJSONText(params.AdditionalRepos)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("marshal run additional repos: %w", err)
	}
	additionalWorkdirsJSON, err := marshalJSONText(params.AdditionalWorkdirs)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("marshal run additional workdirs: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("begin create run tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := ensureSessionExistsTx(ctx, tx, params.SessionID); err != nil {
		return domain.SessionRun{}, err
	}
	if err := ensureNoActiveSiblingSessionTx(ctx, tx, params.SessionID); err != nil {
		return domain.SessionRun{}, err
	}
	if err := ensureNoRunningRunTx(ctx, tx, params.SessionID); err != nil {
		return domain.SessionRun{}, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO session_runs (id, session_id, status, agent_status, profile_name, profile_snapshot, workdir, additional_repos, additional_workdirs, native_id, started_at, created_at, updated_at)
		VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, params.ID, params.SessionID, string(domain.SessionRunStatusRunning), params.ProfileName, profileSnapshotJSON, params.Workdir, additionalReposJSON, additionalWorkdirsJSON, nullIfEmpty(params.NativeID), formatPreciseTime(params.StartedAt), formatPreciseTime(createdAt), formatPreciseTime(createdAt))
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("insert session run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.SessionRun{}, fmt.Errorf("commit create run tx: %w", err)
	}

	return s.GetRun(ctx, params.ID)
}

func (s *SessionStore) GetRun(ctx context.Context, id string) (domain.SessionRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, status, COALESCE(agent_status, ''), profile_name, COALESCE(profile_snapshot, ''), workdir, additional_repos, additional_workdirs, COALESCE(native_id, ''),
		       COALESCE(failure_reason, ''), COALESCE(started_at, ''), COALESCE(ended_at, ''), created_at, updated_at
		FROM session_runs
		WHERE id = ?
	`, id)
	run, err := scanSessionRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.SessionRun{}, &domain.NotFoundError{Resource: "session_run", ID: id}
		}
		return domain.SessionRun{}, fmt.Errorf("get session run %s: %w", id, err)
	}
	return run, nil
}

func (s *SessionStore) GetCurrentRun(ctx context.Context, sessionID string) (*domain.SessionRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, status, COALESCE(agent_status, ''), profile_name, COALESCE(profile_snapshot, ''), workdir, additional_repos, additional_workdirs, COALESCE(native_id, ''),
		       COALESCE(failure_reason, ''), COALESCE(started_at, ''), COALESCE(ended_at, ''), created_at, updated_at
		FROM session_runs
		WHERE session_id = ?
		ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, created_at DESC, id DESC
		LIMIT 1
	`, sessionID)
	run, err := scanSessionRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get current session run for %s: %w", sessionID, err)
	}
	return &run, nil
}

func (s *SessionStore) DeleteRun(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM session_runs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session run %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session_run", id)
}

func (s *SessionStore) MarkRunCompleted(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_runs
		SET status = ?, agent_status = ?, failure_reason = NULL, ended_at = ?, updated_at = ?
		WHERE id = ?
	`, domain.SessionRunStatusCompleted, domain.AgentStatusStopped, formatPreciseTime(now), formatPreciseTime(now), id)
	if err != nil {
		return fmt.Errorf("mark session run completed %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session_run", id)
}

func (s *SessionStore) MarkRunFailed(ctx context.Context, id string, reason domain.SessionRunFailureReason) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_runs
		SET status = ?, agent_status = ?, failure_reason = ?, ended_at = ?, updated_at = ?
		WHERE id = ?
	`, domain.SessionRunStatusFailed, domain.AgentStatusStopped, nullIfEmpty(string(reason)), formatPreciseTime(now), formatPreciseTime(now), id)
	if err != nil {
		return fmt.Errorf("mark session run failed %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session_run", id)
}

func (s *SessionStore) UpdateRunNativeID(ctx context.Context, id, nativeID string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_runs
		SET native_id = ?, updated_at = ?
		WHERE id = ?
	`, nullIfEmpty(nativeID), formatPreciseTime(now), id)
	if err != nil {
		return fmt.Errorf("update session run native id %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session_run", id)
}

func (s *SessionStore) UpdateRunAgentStatus(ctx context.Context, id, agentStatus string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_runs
		SET agent_status = ?, updated_at = ?
		WHERE id = ?
	`, agentStatus, formatPreciseTime(now), id)
	if err != nil {
		return fmt.Errorf("update session run agent status %s: %w", id, err)
	}
	return ensureRowsAffected(result, "session_run", id)
}

func (s *SessionStore) ListSessionEvents(ctx context.Context, sessionID string) ([]domain.SessionEvent, error) {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, COALESCE(run_id, ''), seq, type, COALESCE(status, ''), COALESCE(tool, ''), COALESCE(message, ''),
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
	if params.RunID != "" {
		if err := ensureRunBelongsToSessionTx(ctx, tx, params.RunID, params.SessionID); err != nil {
			return domain.SessionEvent{}, err
		}
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
		RunID:             params.RunID,
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
			id, session_id, run_id, seq, type, status, tool, message, native_id, primary_native_id, native_session_role, metadata, raw, at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ID, event.SessionID, nullIfEmpty(event.RunID), event.Seq, event.Type, nullIfEmpty(event.Status), nullIfEmpty(event.Tool),
		nullIfEmpty(event.Message), nullIfEmpty(event.NativeID), nullIfEmpty(event.PrimaryNativeID), nullIfEmpty(event.NativeSessionRole), metadataJSON, rawJSON, event.At.Format(time.RFC3339Nano))
	if err != nil {
		return domain.SessionEvent{}, fmt.Errorf("insert session event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.SessionEvent{}, fmt.Errorf("commit session event tx: %w", err)
	}
	return event, nil
}

func ensureNoRunningRunTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM session_runs WHERE session_id = ? AND status = ? LIMIT 1`, sessionID, string(domain.SessionRunStatusRunning)).Scan(&existingID)
	if err == nil {
		return &domain.ConflictError{
			Resource: "session_run",
			Field:    "session_id",
			Message:  fmt.Sprintf("session %s already has a running run", sessionID),
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("check running session run: %w", err)
}

func ensureRunBelongsToSessionTx(ctx context.Context, tx *sql.Tx, runID, sessionID string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM session_runs WHERE id = ? AND session_id = ?`, runID, sessionID).Scan(&exists)
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.NotFoundError{Resource: "session_run", ID: runID}
	}
	return fmt.Errorf("check session run %s on session %s: %w", runID, sessionID, err)
}

func scanSessionEvent(scanner interface{ Scan(...any) error }) (domain.SessionEvent, error) {
	var event domain.SessionEvent
	var metadataJSON string
	var rawJSON string
	var at string
	if err := scanner.Scan(
		&event.ID,
		&event.SessionID,
		&event.RunID,
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

func scanSessionRun(scanner interface{ Scan(...any) error }) (domain.SessionRun, error) {
	var run domain.SessionRun
	var status string
	var agentStatus string
	var profileSnapshotJSON string
	var additionalReposJSON string
	var additionalWorkdirsJSON string
	var failureReason string
	var startedAt string
	var endedAt string
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&run.ID,
		&run.SessionID,
		&status,
		&agentStatus,
		&run.ProfileName,
		&profileSnapshotJSON,
		&run.Workdir,
		&additionalReposJSON,
		&additionalWorkdirsJSON,
		&run.NativeID,
		&failureReason,
		&startedAt,
		&endedAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		return domain.SessionRun{}, err
	}

	run.Status = domain.SessionRunStatus(status)
	if err := json.Unmarshal([]byte(additionalReposJSON), &run.AdditionalRepos); err != nil {
		return domain.SessionRun{}, fmt.Errorf("decode run additional repos: %w", err)
	}
	if err := json.Unmarshal([]byte(additionalWorkdirsJSON), &run.AdditionalWorkdirs); err != nil {
		return domain.SessionRun{}, fmt.Errorf("decode run additional workdirs: %w", err)
	}
	run.AgentStatus = agentStatus
	run.FailureReason = domain.SessionRunFailureReason(failureReason)
	if profileSnapshotJSON != "" {
		var snapshot domain.AgentProfileSnapshot
		if err := json.Unmarshal([]byte(profileSnapshotJSON), &snapshot); err != nil {
			return domain.SessionRun{}, fmt.Errorf("decode profile snapshot: %w", err)
		}
		run.ProfileSnapshot = &snapshot
	}
	parsedStartedAt, err := parseSQLiteMaybeTime(startedAt)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("parse started_at: %w", err)
	}
	run.StartedAt = parsedStartedAt
	parsedEndedAt, err := parseSQLiteMaybeTime(endedAt)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("parse ended_at: %w", err)
	}
	run.EndedAt = parsedEndedAt
	run.CreatedAt, err = parseSQLiteTime(createdAt)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("parse created_at: %w", err)
	}
	run.UpdatedAt, err = parseSQLiteTime(updatedAt)
	if err != nil {
		return domain.SessionRun{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return run, nil
}
