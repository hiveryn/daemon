package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestArchitectRegistrationStores(t *testing.T) {
	t.Parallel()

	db := newTestDB(t)
	defer func() { _ = db.Close() }()

	groups := NewArchitectGroupStore(db)
	architects := NewArchitectStore(db)
	repos := NewRepoStore(db)

	group, err := groups.Create(context.Background(), domain.ArchitectGroupInput{Name: "Core"})
	if err != nil {
		t.Fatalf("create architect group: %v", err)
	}

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	architect, err := architects.Create(context.Background(), domain.ArchitectInput{
		Path:    architectPath,
		Title:   "Hiveryn",
		GroupID: &group.ID,
	})
	if err != nil {
		t.Fatalf("create architect: %v", err)
	}
	if !architect.Exists {
		t.Fatal("expected created architect to exist on disk")
	}

	repo, err := repos.Create(context.Background(), architect.ID, domain.RepoInput{Key: "daemon", Path: &repoPath})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	listed, err := architects.List(context.Background())
	if err != nil {
		t.Fatalf("list architects: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 architect, got %d", len(listed))
	}
	if listed[0].RepoCount != 1 {
		t.Fatalf("expected repo_count 1, got %d", listed[0].RepoCount)
	}
	if listed[0].Group == nil || listed[0].Group.ID != group.ID {
		t.Fatalf("expected architect group %q, got %#v", group.ID, listed[0].Group)
	}

	fetched, err := architects.Get(context.Background(), architect.ID)
	if err != nil {
		t.Fatalf("get architect: %v", err)
	}
	if len(fetched.Repos) != 1 || fetched.Repos[0].ID != repo.ID {
		t.Fatalf("expected architect repos to include %q, got %#v", repo.ID, fetched.Repos)
	}

	openedAt := time.Now().UTC().Truncate(time.Second)
	if err := architects.Touch(context.Background(), architect.ID, openedAt); err != nil {
		t.Fatalf("touch architect: %v", err)
	}

	touched, err := architects.Get(context.Background(), architect.ID)
	if err != nil {
		t.Fatalf("get touched architect: %v", err)
	}
	if touched.LastOpenedAt == nil || !touched.LastOpenedAt.Equal(openedAt) {
		t.Fatalf("expected last_opened_at %v, got %#v", openedAt, touched.LastOpenedAt)
	}

	if err := groups.Delete(context.Background(), group.ID); err != nil {
		t.Fatalf("delete architect group: %v", err)
	}

	updatedArchitect, err := architects.Get(context.Background(), architect.ID)
	if err != nil {
		t.Fatalf("get architect after group delete: %v", err)
	}
	if updatedArchitect.Group != nil {
		t.Fatalf("expected architect group to be cleared, got %#v", updatedArchitect.Group)
	}

	if err := architects.Delete(context.Background(), architect.ID); err != nil {
		t.Fatalf("delete architect: %v", err)
	}

	_, err = repos.Get(context.Background(), architect.ID, repo.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after architect delete cascade, got %v", err)
	}
}

func TestArchitectRegistrationValidationAndConflicts(t *testing.T) {
	t.Parallel()

	db := newTestDB(t)
	defer func() { _ = db.Close() }()

	architects := NewArchitectStore(db)
	repos := NewRepoStore(db)

	_, err := architects.Create(context.Background(), domain.ArchitectInput{Path: "relative/path"})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected validation error for relative architect path, got %v", err)
	}

	architectPath := t.TempDir()
	architect, err := architects.Create(context.Background(), domain.ArchitectInput{Path: architectPath})
	if err != nil {
		t.Fatalf("create architect: %v", err)
	}

	_, err = architects.Create(context.Background(), domain.ArchitectInput{Path: architectPath})
	var conflictErr *domain.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected conflict error for duplicate architect path, got %v", err)
	}

	_, err = repos.Create(context.Background(), architect.ID, domain.RepoInput{Key: " "})
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected validation error for blank repo key, got %v", err)
	}

	_, err = repos.Create(context.Background(), architect.ID, domain.RepoInput{Key: "daemon"})
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected validation error for missing repo path, got %v", err)
	}

	repoPath := t.TempDir()

	_, err = repos.Create(context.Background(), architect.ID, domain.RepoInput{Key: "daemon", Path: &repoPath})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	repo, err := repos.List(context.Background(), architect.ID)
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repo) != 1 || repo[0].Path == nil || *repo[0].Path != repoPath {
		t.Fatalf("expected repo path %q, got %#v", repoPath, repo)
	}

	_, err = repos.Create(context.Background(), architect.ID, domain.RepoInput{Key: "daemon", Path: &repoPath})
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected conflict error for duplicate repo key, got %v", err)
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return db
}
