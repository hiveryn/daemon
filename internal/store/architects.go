package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

type ArchitectStore struct {
	db *sql.DB
}

func NewArchitectStore(db *sql.DB) *ArchitectStore {
	return &ArchitectStore{db: db}
}

func (s *ArchitectStore) List(ctx context.Context) ([]domain.Architect, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			a.id,
			a.path,
			COALESCE(a.title, ''),
			a.last_opened_at,
			a.created_at,
			a.updated_at,
			g.id,
			g.name,
			COUNT(r.id)
		FROM architects a
		LEFT JOIN architect_groups g ON g.id = a.group_id
		LEFT JOIN repos r ON r.architect_id = a.id
		GROUP BY a.id, a.path, a.title, a.last_opened_at, a.created_at, a.updated_at, g.id, g.name
		ORDER BY a.title, a.path, a.id
	`)
	if err != nil {
		return nil, fmt.Errorf("query architects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	architects := []domain.Architect{}
	for rows.Next() {
		architect, err := scanArchitectSummary(rows)
		if err != nil {
			return nil, err
		}
		architect.Exists = pathExists(architect.Path)
		architects = append(architects, architect)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate architects: %w", err)
	}

	return architects, nil
}

func (s *ArchitectStore) Create(ctx context.Context, input domain.ArchitectInput) (domain.Architect, error) {
	normalized, err := normalizeArchitectInput(input)
	if err != nil {
		return domain.Architect{}, err
	}
	if err := ensureArchitectGroupExists(ctx, s.db, normalized.GroupID); err != nil {
		return domain.Architect{}, err
	}

	id, err := newResourceID("arc")
	if err != nil {
		return domain.Architect{}, fmt.Errorf("generate architect id: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO architects(id, path, title, group_id)
		VALUES (?, ?, ?, ?)
	`, id, normalized.Path, nullableString(normalized.Title), nullableStringPtr(normalized.GroupID))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: architects.path") {
			return domain.Architect{}, &domain.ConflictError{Resource: "architect", Field: "path", Message: "path already registered"}
		}
		return domain.Architect{}, fmt.Errorf("insert architect: %w", err)
	}

	return s.Get(ctx, id)
}

func (s *ArchitectStore) Get(ctx context.Context, id string) (domain.Architect, error) {
	architect, err := scanArchitectSummary(s.db.QueryRowContext(ctx, `
		SELECT
			a.id,
			a.path,
			COALESCE(a.title, ''),
			a.last_opened_at,
			a.created_at,
			a.updated_at,
			g.id,
			g.name,
			0
		FROM architects a
		LEFT JOIN architect_groups g ON g.id = a.group_id
		WHERE a.id = ?
	`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Architect{}, &domain.NotFoundError{Resource: "architect", ID: id}
		}
		return domain.Architect{}, err
	}

	repos, err := listReposByArchitect(ctx, s.db, id)
	if err != nil {
		return domain.Architect{}, err
	}

	architect.Repos = repos
	architect.Exists = pathExists(architect.Path)
	architect.RepoCount = 0
	return architect, nil
}

func (s *ArchitectStore) Update(ctx context.Context, id string, input domain.ArchitectUpdate) (domain.Architect, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return domain.Architect{}, err
	}

	path := current.Path
	if input.PathSet {
		if input.Path == nil {
			return domain.Architect{}, &domain.ValidationError{Field: "path", Message: "is required"}
		}
		path, err = normalizeExistingAbsolutePath("path", *input.Path)
		if err != nil {
			return domain.Architect{}, err
		}
	}

	title := current.Title
	if input.TitleSet {
		title = normalizeOptionalTitle(input.Title)
	}

	groupID, err := normalizeArchitectGroupUpdate(input, current.Group)
	if err != nil {
		return domain.Architect{}, err
	}
	if err := ensureArchitectGroupExists(ctx, s.db, groupID); err != nil {
		return domain.Architect{}, err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE architects
		SET path = ?, title = ?, group_id = ?, updated_at = datetime('now')
		WHERE id = ?
	`, path, nullableString(title), nullableStringPtr(groupID), id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: architects.path") {
			return domain.Architect{}, &domain.ConflictError{Resource: "architect", Field: "path", Message: "path already registered"}
		}
		return domain.Architect{}, fmt.Errorf("update architect: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return domain.Architect{}, fmt.Errorf("check updated architects: %w", err)
	}
	if rowsAffected == 0 {
		return domain.Architect{}, &domain.NotFoundError{Resource: "architect", ID: id}
	}

	return s.Get(ctx, id)
}

func (s *ArchitectStore) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM architects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete architect: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted architects: %w", err)
	}
	if rowsAffected == 0 {
		return &domain.NotFoundError{Resource: "architect", ID: id}
	}

	return nil
}

func (s *ArchitectStore) Touch(ctx context.Context, id string, openedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE architects
		SET last_opened_at = ?, updated_at = datetime('now')
		WHERE id = ?
	`, openedAt.UTC().Format(timestampLayout), id)
	if err != nil {
		return fmt.Errorf("touch architect: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check touched architects: %w", err)
	}
	if rowsAffected == 0 {
		return &domain.NotFoundError{Resource: "architect", ID: id}
	}

	return nil
}

func scanArchitectSummary(scanner rowScanner) (domain.Architect, error) {
	var (
		architect       domain.Architect
		lastOpenedAtRaw sql.NullString
		createdAtRaw    string
		updatedAtRaw    string
		groupID         sql.NullString
		groupName       sql.NullString
	)

	if err := scanner.Scan(
		&architect.ID,
		&architect.Path,
		&architect.Title,
		&lastOpenedAtRaw,
		&createdAtRaw,
		&updatedAtRaw,
		&groupID,
		&groupName,
		&architect.RepoCount,
	); err != nil {
		return domain.Architect{}, err
	}

	var err error
	architect.CreatedAt, err = time.ParseInLocation(timestampLayout, createdAtRaw, time.UTC)
	if err != nil {
		return domain.Architect{}, fmt.Errorf("parse created_at for architect %q: %w", architect.ID, err)
	}
	architect.UpdatedAt, err = time.ParseInLocation(timestampLayout, updatedAtRaw, time.UTC)
	if err != nil {
		return domain.Architect{}, fmt.Errorf("parse updated_at for architect %q: %w", architect.ID, err)
	}
	if lastOpenedAtRaw.Valid {
		openedAt, err := time.ParseInLocation(timestampLayout, lastOpenedAtRaw.String, time.UTC)
		if err != nil {
			return domain.Architect{}, fmt.Errorf("parse last_opened_at for architect %q: %w", architect.ID, err)
		}
		architect.LastOpenedAt = &openedAt
	}
	if groupID.Valid {
		architect.Group = &domain.ArchitectGroupRef{ID: groupID.String, Name: groupName.String}
	}

	return architect, nil
}

func listReposByArchitect(ctx context.Context, db *sql.DB, architectID string) ([]domain.Repo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, key, path, architect_id, created_at, updated_at
		FROM repos
		WHERE architect_id = ?
		ORDER BY key, id
	`, architectID)
	if err != nil {
		return nil, fmt.Errorf("query repos for architect %q: %w", architectID, err)
	}
	defer func() { _ = rows.Close() }()

	repos := []domain.Repo{}
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repos for architect %q: %w", architectID, err)
	}

	return repos, nil
}

func normalizeArchitectInput(input domain.ArchitectInput) (domain.ArchitectInput, error) {
	path, err := normalizeExistingAbsolutePath("path", input.Path)
	if err != nil {
		return domain.ArchitectInput{}, err
	}

	groupID, err := normalizeOptionalID("group_id", input.GroupID)
	if err != nil {
		return domain.ArchitectInput{}, err
	}

	return domain.ArchitectInput{
		Path:    path,
		Title:   strings.TrimSpace(input.Title),
		GroupID: groupID,
	}, nil
}

func normalizeArchitectGroupUpdate(input domain.ArchitectUpdate, current *domain.ArchitectGroupRef) (*string, error) {
	if !input.GroupIDSet {
		if current == nil {
			return nil, nil
		}
		return &current.ID, nil
	}
	if input.GroupID == nil {
		return nil, nil
	}
	return normalizeOptionalID("group_id", input.GroupID)
}

func ensureArchitectGroupExists(ctx context.Context, db *sql.DB, id *string) error {
	if id == nil {
		return nil
	}

	var exists int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM architect_groups WHERE id = ?`, *id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.NotFoundError{Resource: "architect_group", ID: *id}
	}
	if err != nil {
		return fmt.Errorf("lookup architect group %q: %w", *id, err)
	}
	return nil
}

func ensureArchitectExists(ctx context.Context, db *sql.DB, id string) error {
	var exists int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM architects WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.NotFoundError{Resource: "architect", ID: id}
	}
	if err != nil {
		return fmt.Errorf("lookup architect %q: %w", id, err)
	}
	return nil
}

func normalizeExistingAbsolutePath(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &domain.ValidationError{Field: field, Message: "is required"}
	}
	if !filepath.IsAbs(value) {
		return "", &domain.ValidationError{Field: field, Message: "must be an absolute path"}
	}
	if _, err := os.Stat(value); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &domain.ValidationError{Field: field, Message: "must exist on disk"}
		}
		return "", fmt.Errorf("stat %s %q: %w", field, value, err)
	}
	return value, nil
}

func normalizeOptionalID(field string, value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, &domain.ValidationError{Field: field, Message: "must not be blank"}
	}
	return &trimmed, nil
}

func normalizeOptionalTitle(title *string) string {
	if title == nil {
		return ""
	}
	return strings.TrimSpace(*title)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableStringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
