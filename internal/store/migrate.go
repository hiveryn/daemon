package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationFiles = []string{
	"migrations/0001_agent_profiles.sql",
	"migrations/0002_agent_profiles_unique_name.sql",
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	for idx, name := range migrationFiles {
		version := idx + 1

		var exists int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %d: %w", version, err)
		}

		script, err := migrationsFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %d: %w", version, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			return fmt.Errorf("record migration %d: %w", version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}
