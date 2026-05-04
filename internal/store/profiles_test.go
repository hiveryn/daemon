package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestProfileStoreCRUD(t *testing.T) {
	t.Parallel()

	s := newTestProfileStore(t)
	defer func() { _ = s.db.Close() }()

	created, err := s.Create(context.Background(), domain.AgentProfileInput{
		Name:      "OpenCode Review",
		AgentKind: domain.AgentKindOpenCode,
		Args:      []string{"--model", "gpt-5"},
		Env: map[string]string{
			"OPENCODE_ENV": "dev",
		},
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}

	fetched, err := s.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if fetched.Name != created.Name {
		t.Fatalf("expected name %q, got %q", created.Name, fetched.Name)
	}

	profiles, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(profiles))
	}

	updated, err := s.Update(context.Background(), created.ID, domain.AgentProfileInput{
		Name:      "Claude Review",
		AgentKind: domain.AgentKindClaude,
		Args:      []string{"--permission-mode", "plan"},
		Env: map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
		},
	})
	if err != nil {
		t.Fatalf("update profile: %v", err)
	}
	if updated.AgentKind != domain.AgentKindClaude {
		t.Fatalf("expected updated kind %q, got %q", domain.AgentKindClaude, updated.AgentKind)
	}

	if err := s.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete profile: %v", err)
	}

	_, err = s.Get(context.Background(), created.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestCreateValidatesAgentKind(t *testing.T) {
	t.Parallel()

	s := newTestProfileStore(t)
	defer func() { _ = s.db.Close() }()

	_, err := s.Create(context.Background(), domain.AgentProfileInput{
		Name:      "Bad",
		AgentKind: "nope",
	})

	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func newTestProfileStore(t *testing.T) *ProfileStore {
	t.Helper()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return NewProfileStore(db)
}
