package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

const sqliteTimeLayout = "2006-01-02 15:04:05"

const preciseTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func scanSessionWithCurrentRun(scanner interface{ Scan(...any) error }) (domain.Session, error) {
	var session domain.Session
	var sessionType string
	var createdBy string
	var createdAt string
	var updatedAt string
	var additionalReposJSON string
	var additionalWorkdirsJSON string

	var runID sql.NullString
	var runSessionID sql.NullString
	var runStatus sql.NullString
	var runAgentStatus sql.NullString
	var runProfileName sql.NullString
	var runProfileSnapshot sql.NullString
	var runWorkdir sql.NullString
	var runAdditionalRepos sql.NullString
	var runAdditionalWorkdirs sql.NullString
	var runNativeID sql.NullString
	var runFailureReason sql.NullString
	var runStartedAt sql.NullString
	var runEndedAt sql.NullString
	var runCreatedAt sql.NullString
	var runUpdatedAt sql.NullString

	if err := scanner.Scan(
		&session.ID,
		&session.ArchitectKey,
		&sessionType,
		&session.ContextID,
		&session.Prompt,
		&session.Workdir,
		&additionalReposJSON,
		&additionalWorkdirsJSON,
		&session.Instructions,
		&createdBy,
		&createdAt,
		&updatedAt,
		&runID,
		&runSessionID,
		&runStatus,
		&runAgentStatus,
		&runProfileName,
		&runProfileSnapshot,
		&runWorkdir,
		&runAdditionalRepos,
		&runAdditionalWorkdirs,
		&runNativeID,
		&runFailureReason,
		&runStartedAt,
		&runEndedAt,
		&runCreatedAt,
		&runUpdatedAt,
	); err != nil {
		return domain.Session{}, err
	}

	session.SessionType = domain.SessionType(sessionType)
	if err := json.Unmarshal([]byte(additionalReposJSON), &session.AdditionalRepos); err != nil {
		return domain.Session{}, fmt.Errorf("decode session additional repos: %w", err)
	}
	if err := json.Unmarshal([]byte(additionalWorkdirsJSON), &session.AdditionalWorkdirs); err != nil {
		return domain.Session{}, fmt.Errorf("decode session additional workdirs: %w", err)
	}
	session.CreatedBy = domain.SessionCreatedBy(createdBy)
	var err error
	session.CreatedAt, err = parseSQLiteTime(createdAt)
	if err != nil {
		return domain.Session{}, fmt.Errorf("parse created_at: %w", err)
	}
	session.UpdatedAt, err = parseSQLiteTime(updatedAt)
	if err != nil {
		return domain.Session{}, fmt.Errorf("parse updated_at: %w", err)
	}

	if runID.Valid && runID.String != "" {
		run, err := scanSessionRunValues(
			runID.String,
			runSessionID.String,
			runStatus.String,
			runAgentStatus.String,
			runProfileName.String,
			runProfileSnapshot.String,
			runWorkdir.String,
			runAdditionalRepos.String,
			runAdditionalWorkdirs.String,
			runNativeID.String,
			runFailureReason.String,
			runStartedAt.String,
			runEndedAt.String,
			runCreatedAt.String,
			runUpdatedAt.String,
		)
		if err != nil {
			return domain.Session{}, err
		}
		session.CurrentRun = &run
	}

	return session, nil
}

func scanSessionRunValues(id, sessionID, status, agentStatus, profileName, profileSnapshotJSON, workdir, additionalReposJSON, additionalWorkdirsJSON, nativeID, failureReason, startedAt, endedAt, createdAt, updatedAt string) (domain.SessionRun, error) {
	run := domain.SessionRun{
		ID:            id,
		SessionID:     sessionID,
		Status:        domain.SessionRunStatus(status),
		AgentStatus:   agentStatus,
		ProfileName:   profileName,
		Workdir:       workdir,
		NativeID:      nativeID,
		FailureReason: domain.SessionRunFailureReason(failureReason),
	}
	if err := json.Unmarshal([]byte(additionalReposJSON), &run.AdditionalRepos); err != nil {
		return domain.SessionRun{}, fmt.Errorf("decode run additional repos: %w", err)
	}
	if err := json.Unmarshal([]byte(additionalWorkdirsJSON), &run.AdditionalWorkdirs); err != nil {
		return domain.SessionRun{}, fmt.Errorf("decode run additional workdirs: %w", err)
	}
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
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.ParseInLocation(sqliteTimeLayout, value, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func formatPreciseTime(value time.Time) string {
	return value.UTC().Format(preciseTimeLayout)
}

func parseSQLiteMaybeTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		parsed = parsed.UTC()
		return &parsed, nil
	}
	parsedSQLite, err := parseSQLiteTime(value)
	if err != nil {
		return nil, err
	}
	return &parsedSQLite, nil
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
	case domain.AgentProfileSnapshot:
		if typed.Agent == "" && len(typed.Args) == 0 && len(typed.Env) == 0 {
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
