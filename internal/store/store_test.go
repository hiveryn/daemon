package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
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

func TestSessionStoreAllowsOneRunningSessionPerArchitect(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ProfileName:  "claude",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusRunning,
	}); err != nil {
		t.Fatalf("create first running session: %v", err)
	}

	_, err = store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-2",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusRunning,
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second running session, got %v", err)
	}

	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-3",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusCompleted,
	}); err != nil {
		t.Fatalf("create completed session for same architect: %v", err)
	}
}

func TestSessionStoreAllowsOneRunningWorkSessionPerTicket(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "work-1",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeWork),
		Status:       domain.SessionStatusRunning,
		TicketID:     "ticket-abc",
	}); err != nil {
		t.Fatalf("create first running work session: %v", err)
	}

	_, err = store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "work-2",
		ProfileName:  "claude",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeWork),
		Status:       domain.SessionStatusRunning,
		TicketID:     "ticket-abc",
	})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict for second work session on same ticket, got %v", err)
	}

	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "work-3",
		ProfileName:  "codex",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeWork),
		Status:       domain.SessionStatusCompleted,
		TicketID:     "ticket-abc",
	}); err != nil {
		t.Fatalf("create completed work session for same ticket: %v", err)
	}
}

func TestSessionStoreUpdatesNativeID(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewSessionStore(db)
	if _, err := store.CreateSession(context.Background(), domain.CreateSessionParams{
		ID:           "session-1",
		ProfileName:  "opencode",
		ArchitectKey: "hiveryn",
		SessionType:  string(domain.SessionTypeArchitect),
		Status:       domain.SessionStatusRunning,
		NativeID:     "first-native-id",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := store.UpdateSessionNativeID(context.Background(), "session-1", "primary-native-id"); err != nil {
		t.Fatalf("update native id: %v", err)
	}

	session, err := store.GetSession(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.NativeID != "primary-native-id" {
		t.Fatalf("expected updated native id, got %q", session.NativeID)
	}
}
