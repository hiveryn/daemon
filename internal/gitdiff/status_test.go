package gitdiff

import (
	"context"
	"path/filepath"
	"testing"
)

func statusByPath(entries []StatusEntry) map[string]StatusEntry {
	out := make(map[string]StatusEntry, len(entries))
	for _, e := range entries {
		out[e.Path] = e
	}
	return out
}

func TestStatusMixedChanges(t *testing.T) {
	t.Parallel()

	repoPath := createRepoWithMixedChanges(t)
	entries, err := Status(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	byPath := statusByPath(entries)

	tracked, ok := byPath["file.txt"]
	if !ok {
		t.Fatalf("expected file.txt in status, got %#v", entries)
	}
	// Staged edit then a further unstaged edit → modified in both columns.
	if tracked.Index != "M" || tracked.Worktree != "M" {
		t.Fatalf("expected file.txt MM, got index=%q worktree=%q", tracked.Index, tracked.Worktree)
	}

	untracked, ok := byPath["untracked.txt"]
	if !ok {
		t.Fatalf("expected untracked.txt in status, got %#v", entries)
	}
	if untracked.Index != "?" || untracked.Worktree != "?" {
		t.Fatalf("expected untracked.txt ??, got index=%q worktree=%q", untracked.Index, untracked.Worktree)
	}
}

func TestStatusCleanRepo(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\n")
	runGitTest(t, repoPath, "add", "file.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	entries, err := Status(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries for a clean repo, got %#v", entries)
	}
}

func TestStatusRenameCarriesOrigPath(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "old.txt"), "content\n")
	runGitTest(t, repoPath, "add", "old.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")
	runGitTest(t, repoPath, "mv", "old.txt", "new.txt")

	entries, err := Status(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	byPath := statusByPath(entries)
	renamed, ok := byPath["new.txt"]
	if !ok {
		t.Fatalf("expected new.txt in status, got %#v", entries)
	}
	if renamed.Index != "R" || renamed.OrigPath != "old.txt" {
		t.Fatalf("expected R with orig_path old.txt, got %#v", renamed)
	}
}

func TestStatusUntrackedDirExpandsFiles(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "tracked.txt"), "x\n")
	runGitTest(t, repoPath, "add", "tracked.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")
	writeFile(t, filepath.Join(repoPath, "newdir", "inner.txt"), "y\n")

	entries, err := Status(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	byPath := statusByPath(entries)
	if _, ok := byPath["newdir/inner.txt"]; !ok {
		t.Fatalf("expected untracked dir expanded to files, got %#v", entries)
	}
}
