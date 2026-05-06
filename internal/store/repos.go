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

type RepoStore struct {
	db *sql.DB
}

func NewRepoStore(db *sql.DB) *RepoStore {
	return &RepoStore{db: db}
}

func (s *RepoStore) List(ctx context.Context, architectID string) ([]domain.Repo, error) {
	if err := ensureArchitectExists(ctx, s.db, architectID); err != nil {
		return nil, err
	}
	return listReposByArchitect(ctx, s.db, architectID)
}

func (s *RepoStore) Create(ctx context.Context, architectID string, input domain.RepoInput) (domain.Repo, error) {
	if err := ensureArchitectExists(ctx, s.db, architectID); err != nil {
		return domain.Repo{}, err
	}

	normalized, err := normalizeRepoInput(input)
	if err != nil {
		return domain.Repo{}, err
	}

	id, err := newResourceID("repo")
	if err != nil {
		return domain.Repo{}, fmt.Errorf("generate repo id: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO repos(id, architect_id, key, path)
		VALUES (?, ?, ?, ?)
	`, id, architectID, normalized.Key, *normalized.Path)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: repos.architect_id, repos.key") {
			return domain.Repo{}, &domain.ConflictError{Resource: "repo", Field: "key", Message: "key already exists for architect"}
		}
		return domain.Repo{}, fmt.Errorf("insert repo: %w", err)
	}

	return s.Get(ctx, architectID, id)
}

func (s *RepoStore) Get(ctx context.Context, architectID, id string) (domain.Repo, error) {
	repo, err := scanRepo(s.db.QueryRowContext(ctx, `
		SELECT id, key, path, architect_id, created_at, updated_at
		FROM repos
		WHERE architect_id = ? AND id = ?
	`, architectID, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if err := ensureArchitectExists(ctx, s.db, architectID); err != nil {
				return domain.Repo{}, err
			}
			return domain.Repo{}, &domain.NotFoundError{Resource: "repo", ID: id}
		}
		return domain.Repo{}, err
	}
	return repo, nil
}

func (s *RepoStore) Update(ctx context.Context, architectID, id string, input domain.RepoUpdate) (domain.Repo, error) {
	current, err := s.Get(ctx, architectID, id)
	if err != nil {
		return domain.Repo{}, err
	}

	key := current.Key
	if input.KeySet {
		if input.Key == nil {
			return domain.Repo{}, &domain.ValidationError{Field: "key", Message: "is required"}
		}
		key, err = normalizeRepoKey(*input.Key)
		if err != nil {
			return domain.Repo{}, err
		}
	}

	path := current.Path
	if input.PathSet {
		if input.Path == nil {
			return domain.Repo{}, &domain.ValidationError{Field: "path", Message: "is required"}
		} else {
			pathValue, err := normalizeExistingAbsolutePath("path", *input.Path)
			if err != nil {
				return domain.Repo{}, err
			}
			path = &pathValue
		}
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE repos
		SET key = ?, path = ?, updated_at = datetime('now')
		WHERE architect_id = ? AND id = ?
	`, key, *path, architectID, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: repos.architect_id, repos.key") {
			return domain.Repo{}, &domain.ConflictError{Resource: "repo", Field: "key", Message: "key already exists for architect"}
		}
		return domain.Repo{}, fmt.Errorf("update repo: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return domain.Repo{}, fmt.Errorf("check updated repos: %w", err)
	}
	if rowsAffected == 0 {
		return domain.Repo{}, &domain.NotFoundError{Resource: "repo", ID: id}
	}

	return s.Get(ctx, architectID, id)
}

func (s *RepoStore) Delete(ctx context.Context, architectID, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE architect_id = ? AND id = ?`, architectID, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted repos: %w", err)
	}
	if rowsAffected == 0 {
		if err := ensureArchitectExists(ctx, s.db, architectID); err != nil {
			return err
		}
		return &domain.NotFoundError{Resource: "repo", ID: id}
	}

	return nil
}

func scanRepo(scanner rowScanner) (domain.Repo, error) {
	var (
		repo         domain.Repo
		path         string
		createdAtRaw string
		updatedAtRaw string
	)

	if err := scanner.Scan(&repo.ID, &repo.Key, &path, &repo.ArchitectID, &createdAtRaw, &updatedAtRaw); err != nil {
		return domain.Repo{}, err
	}
	repo.Path = &path

	var err error
	repo.CreatedAt, err = time.ParseInLocation(timestampLayout, createdAtRaw, time.UTC)
	if err != nil {
		return domain.Repo{}, fmt.Errorf("parse created_at for repo %q: %w", repo.ID, err)
	}
	repo.UpdatedAt, err = time.ParseInLocation(timestampLayout, updatedAtRaw, time.UTC)
	if err != nil {
		return domain.Repo{}, fmt.Errorf("parse updated_at for repo %q: %w", repo.ID, err)
	}

	return repo, nil
}

func normalizeRepoInput(input domain.RepoInput) (domain.RepoInput, error) {
	key, err := normalizeRepoKey(input.Key)
	if err != nil {
		return domain.RepoInput{}, err
	}
	if input.Path == nil {
		return domain.RepoInput{}, &domain.ValidationError{Field: "path", Message: "is required"}
	}

	path, err := normalizeOptionalRepoPath(input.Path)
	if err != nil {
		return domain.RepoInput{}, err
	}

	return domain.RepoInput{Key: key, Path: path}, nil
}

func normalizeRepoKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", &domain.ValidationError{Field: "key", Message: "is required"}
	}
	return key, nil
}

func normalizeOptionalRepoPath(path *string) (*string, error) {
	if path == nil {
		return nil, nil
	}
	value, err := normalizeExistingAbsolutePath("path", *path)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
