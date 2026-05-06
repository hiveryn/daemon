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

type ArchitectGroupStore struct {
	db *sql.DB
}

func NewArchitectGroupStore(db *sql.DB) *ArchitectGroupStore {
	return &ArchitectGroupStore{db: db}
}

func (s *ArchitectGroupStore) List(ctx context.Context) ([]domain.ArchitectGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, updated_at
		FROM architect_groups
		ORDER BY name, id
	`)
	if err != nil {
		return nil, fmt.Errorf("query architect groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	groups := []domain.ArchitectGroup{}
	for rows.Next() {
		group, err := scanArchitectGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate architect groups: %w", err)
	}

	return groups, nil
}

func (s *ArchitectGroupStore) Create(ctx context.Context, input domain.ArchitectGroupInput) (domain.ArchitectGroup, error) {
	name, err := normalizeArchitectGroupName(input.Name)
	if err != nil {
		return domain.ArchitectGroup{}, err
	}

	id, err := newResourceID("ag")
	if err != nil {
		return domain.ArchitectGroup{}, fmt.Errorf("generate architect group id: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO architect_groups(id, name)
		VALUES (?, ?)
	`, id, name)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: architect_groups.name") {
			return domain.ArchitectGroup{}, &domain.ConflictError{Resource: "architect_group", Field: "name", Message: "name already exists"}
		}
		return domain.ArchitectGroup{}, fmt.Errorf("insert architect group: %w", err)
	}

	return s.Get(ctx, id)
}

func (s *ArchitectGroupStore) Get(ctx context.Context, id string) (domain.ArchitectGroup, error) {
	group, err := scanArchitectGroup(s.db.QueryRowContext(ctx, `
		SELECT id, name, created_at, updated_at
		FROM architect_groups
		WHERE id = ?
	`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ArchitectGroup{}, &domain.NotFoundError{Resource: "architect_group", ID: id}
		}
		return domain.ArchitectGroup{}, err
	}
	return group, nil
}

func (s *ArchitectGroupStore) Update(ctx context.Context, id string, input domain.ArchitectGroupUpdate) (domain.ArchitectGroup, error) {
	if !input.NameSet {
		return s.Get(ctx, id)
	}
	if input.Name == nil {
		return domain.ArchitectGroup{}, &domain.ValidationError{Field: "name", Message: "is required"}
	}

	name, err := normalizeArchitectGroupName(*input.Name)
	if err != nil {
		return domain.ArchitectGroup{}, err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE architect_groups
		SET name = ?, updated_at = datetime('now')
		WHERE id = ?
	`, name, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: architect_groups.name") {
			return domain.ArchitectGroup{}, &domain.ConflictError{Resource: "architect_group", Field: "name", Message: "name already exists"}
		}
		return domain.ArchitectGroup{}, fmt.Errorf("update architect group: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return domain.ArchitectGroup{}, fmt.Errorf("check updated architect groups: %w", err)
	}
	if rowsAffected == 0 {
		return domain.ArchitectGroup{}, &domain.NotFoundError{Resource: "architect_group", ID: id}
	}

	return s.Get(ctx, id)
}

func (s *ArchitectGroupStore) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM architect_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete architect group: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted architect groups: %w", err)
	}
	if rowsAffected == 0 {
		return &domain.NotFoundError{Resource: "architect_group", ID: id}
	}

	return nil
}

func scanArchitectGroup(scanner rowScanner) (domain.ArchitectGroup, error) {
	var (
		group        domain.ArchitectGroup
		createdAtRaw string
		updatedAtRaw string
	)

	if err := scanner.Scan(&group.ID, &group.Name, &createdAtRaw, &updatedAtRaw); err != nil {
		return domain.ArchitectGroup{}, err
	}

	var err error
	group.CreatedAt, err = time.ParseInLocation(timestampLayout, createdAtRaw, time.UTC)
	if err != nil {
		return domain.ArchitectGroup{}, fmt.Errorf("parse created_at for architect group %q: %w", group.ID, err)
	}
	group.UpdatedAt, err = time.ParseInLocation(timestampLayout, updatedAtRaw, time.UTC)
	if err != nil {
		return domain.ArchitectGroup{}, fmt.Errorf("parse updated_at for architect group %q: %w", group.ID, err)
	}

	return group, nil
}

func normalizeArchitectGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", &domain.ValidationError{Field: "name", Message: "is required"}
	}
	return name, nil
}
