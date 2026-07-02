package gitdiff

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCheckIgnore_InsideRepo(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, ".gitignore"), "ignored.txt\n")

	ignored := CheckIgnore(context.Background(), repoPath, []string{"ignored.txt", "tracked.txt"})
	if !ignored["ignored.txt"] {
		t.Fatalf("expected ignored.txt to be ignored, got %#v", ignored)
	}
	if ignored["tracked.txt"] {
		t.Fatalf("expected tracked.txt to not be ignored, got %#v", ignored)
	}
}

func TestCheckIgnore_OutsideRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ignored := CheckIgnore(context.Background(), dir, []string{"anything.txt"})
	if len(ignored) != 0 {
		t.Fatalf("expected empty map outside a repo, got %#v", ignored)
	}
}

func TestCheckIgnore_EmptyNames(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	ignored := CheckIgnore(context.Background(), repoPath, nil)
	if len(ignored) != 0 {
		t.Fatalf("expected empty map for no names, got %#v", ignored)
	}
}
