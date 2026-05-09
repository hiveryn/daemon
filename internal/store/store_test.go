package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenRunsSessionMigrations(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, table := range []string{"schema_migrations", "sessions", "session_events"} {
		var name string
		if err := db.QueryRowContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
		if name != table {
			t.Fatalf("expected table %q, got %q", table, name)
		}
	}
}
