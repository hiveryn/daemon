package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/hiveryn/daemon/internal/config"
	_ "modernc.org/sqlite"
)

func DefaultDBPath() (string, error) {
	runtime, err := config.ResolveRuntime("", "")
	if err != nil {
		return "", err
	}
	return runtime.DBPath, nil
}

func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		var err error
		path, err = DefaultDBPath()
		if err != nil {
			return nil, err
		}
	}

	// Worker tunnel credentials are persisted in sessions. Create/restrict the
	// database before SQLite opens it, so new journal files inherit private mode.
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("open private database: %w", err)
		}
		err = file.Chmod(0600)
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := initialize(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func initialize(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("enable sqlite foreign keys: %w", err)
	}

	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("set sqlite busy timeout: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	if err := runMigrations(ctx, db); err != nil {
		return err
	}

	return nil
}
