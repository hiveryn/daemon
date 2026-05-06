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

const timestampLayout = "2006-01-02 15:04:05"

type ProfileStore struct {
	db *sql.DB
}

func NewProfileStore(db *sql.DB) *ProfileStore {
	return &ProfileStore{db: db}
}

func (s *ProfileStore) List(ctx context.Context) ([]domain.AgentProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, agent_kind, args_json, env_json, created_at, updated_at
		FROM agent_profiles
		ORDER BY name, id
	`)
	if err != nil {
		return nil, fmt.Errorf("query agent profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	profiles := []domain.AgentProfile{}
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent profiles: %w", err)
	}

	return profiles, nil
}

func (s *ProfileStore) Create(ctx context.Context, input domain.AgentProfileInput) (domain.AgentProfile, error) {
	normalized, err := normalizeInput(input)
	if err != nil {
		return domain.AgentProfile{}, err
	}

	argsJSON, envJSON, err := marshalProfileJSON(normalized.Args, normalized.Env)
	if err != nil {
		return domain.AgentProfile{}, err
	}

	id, err := newProfileID()
	if err != nil {
		return domain.AgentProfile{}, fmt.Errorf("generate profile id: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_profiles(id, name, agent_kind, args_json, env_json)
		VALUES (?, ?, ?, ?, ?)
	`, id, normalized.Name, string(normalized.AgentKind), argsJSON, envJSON)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: agent_profiles.name") {
			return domain.AgentProfile{}, &domain.ConflictError{
				Resource: "agent_profile",
				Field:    "name",
				Message:  "name already exists",
			}
		}
		return domain.AgentProfile{}, fmt.Errorf("insert agent profile: %w", err)
	}

	return s.Get(ctx, id)
}

func (s *ProfileStore) Get(ctx context.Context, id string) (domain.AgentProfile, error) {
	profile, err := scanProfile(s.db.QueryRowContext(ctx, `
		SELECT id, name, agent_kind, args_json, env_json, created_at, updated_at
		FROM agent_profiles
		WHERE id = ?
	`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentProfile{}, domain.ErrNotFound
		}
		return domain.AgentProfile{}, err
	}
	return profile, nil
}

func (s *ProfileStore) Update(ctx context.Context, id string, input domain.AgentProfileInput) (domain.AgentProfile, error) {
	normalized, err := normalizeInput(input)
	if err != nil {
		return domain.AgentProfile{}, err
	}

	argsJSON, envJSON, err := marshalProfileJSON(normalized.Args, normalized.Env)
	if err != nil {
		return domain.AgentProfile{}, err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE agent_profiles
		SET name = ?, agent_kind = ?, args_json = ?, env_json = ?, updated_at = datetime('now')
		WHERE id = ?
	`, normalized.Name, string(normalized.AgentKind), argsJSON, envJSON, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: agent_profiles.name") {
			return domain.AgentProfile{}, &domain.ConflictError{
				Resource: "agent_profile",
				Field:    "name",
				Message:  "name already exists",
			}
		}
		return domain.AgentProfile{}, fmt.Errorf("update agent profile: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return domain.AgentProfile{}, fmt.Errorf("check updated rows: %w", err)
	}
	if rowsAffected == 0 {
		return domain.AgentProfile{}, domain.ErrNotFound
	}

	return s.Get(ctx, id)
}

func (s *ProfileStore) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM agent_profiles WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete agent profile: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted rows: %w", err)
	}
	if rowsAffected == 0 {
		return domain.ErrNotFound
	}

	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProfile(scanner rowScanner) (domain.AgentProfile, error) {
	var (
		profile      domain.AgentProfile
		agentKind    string
		argsJSON     string
		envJSON      string
		createdAtRaw string
		updatedAtRaw string
	)

	if err := scanner.Scan(&profile.ID, &profile.Name, &agentKind, &argsJSON, &envJSON, &createdAtRaw, &updatedAtRaw); err != nil {
		return domain.AgentProfile{}, err
	}

	profile.AgentKind = domain.AgentKind(agentKind)
	if !profile.AgentKind.Valid() {
		return domain.AgentProfile{}, fmt.Errorf("invalid agent_kind %q stored in database", agentKind)
	}

	if err := json.Unmarshal([]byte(argsJSON), &profile.Args); err != nil {
		return domain.AgentProfile{}, fmt.Errorf("decode args_json for profile %q: %w", profile.ID, err)
	}
	if err := json.Unmarshal([]byte(envJSON), &profile.Env); err != nil {
		return domain.AgentProfile{}, fmt.Errorf("decode env_json for profile %q: %w", profile.ID, err)
	}

	var err error
	profile.CreatedAt, err = time.ParseInLocation(timestampLayout, createdAtRaw, time.UTC)
	if err != nil {
		return domain.AgentProfile{}, fmt.Errorf("parse created_at for profile %q: %w", profile.ID, err)
	}
	profile.UpdatedAt, err = time.ParseInLocation(timestampLayout, updatedAtRaw, time.UTC)
	if err != nil {
		return domain.AgentProfile{}, fmt.Errorf("parse updated_at for profile %q: %w", profile.ID, err)
	}

	return profile, nil
}

func normalizeInput(input domain.AgentProfileInput) (domain.AgentProfileInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return domain.AgentProfileInput{}, &domain.ValidationError{Field: "name", Message: "is required"}
	}
	if !input.AgentKind.Valid() {
		return domain.AgentProfileInput{}, &domain.ValidationError{Field: "agent_kind", Message: "must be one of claude, codex, opencode"}
	}
	if input.Args == nil {
		input.Args = []string{}
	}
	if input.Env == nil {
		input.Env = map[string]string{}
	}

	for i, arg := range input.Args {
		if strings.TrimSpace(arg) == "" {
			return domain.AgentProfileInput{}, &domain.ValidationError{Field: fmt.Sprintf("args[%d]", i), Message: "must not be blank"}
		}
	}

	for key := range input.Env {
		if strings.TrimSpace(key) == "" {
			return domain.AgentProfileInput{}, &domain.ValidationError{Field: "env", Message: "keys must not be blank"}
		}
	}

	return input, nil
}

func marshalProfileJSON(args []string, env map[string]string) (string, string, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", "", fmt.Errorf("marshal args_json: %w", err)
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", "", fmt.Errorf("marshal env_json: %w", err)
	}

	return string(argsJSON), string(envJSON), nil
}

func newProfileID() (string, error) {
	return newResourceID("ap")
}
