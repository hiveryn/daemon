package gitdiff

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGrepContentFindsTrackedAndUntracked(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "tracked.txt"), "alpha\nneedle here\nomega\n")
	runGitTest(t, repoPath, "add", "tracked.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")
	writeFile(t, filepath.Join(repoPath, "untracked.txt"), "NEEDLE uppercase\n")

	matches, truncated, err := GrepContent(context.Background(), repoPath, "needle", 100)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("expected no truncation")
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (case-insensitive, incl. untracked), got %#v", matches)
	}
	byPath := map[string]ContentMatch{}
	for _, m := range matches {
		byPath[m.Path] = m
	}
	if m := byPath["tracked.txt"]; m.Line != 2 || m.Text != "needle here" {
		t.Fatalf("unexpected tracked match: %#v", m)
	}
	if m := byPath["untracked.txt"]; m.Line != 1 {
		t.Fatalf("unexpected untracked match: %#v", m)
	}
}

func TestGrepContentNoMatches(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "a.txt"), "nothing to see\n")
	runGitTest(t, repoPath, "add", "a.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	matches, truncated, err := GrepContent(context.Background(), repoPath, "zzz-absent", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 || truncated {
		t.Fatalf("expected empty result, got matches=%#v truncated=%v", matches, truncated)
	}
}

func TestGrepContentTruncatesAtCap(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "many.txt"), "hit\nhit\nhit\nhit\n")
	runGitTest(t, repoPath, "add", "many.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	matches, truncated, err := GrepContent(context.Background(), repoPath, "hit", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || !truncated {
		t.Fatalf("expected 2 matches with truncated=true, got %d truncated=%v", len(matches), truncated)
	}
}

func TestGrepContentQueryIsNotARegex(t *testing.T) {
	t.Parallel()

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "a.txt"), "literal a.b here\naXb should not match\n")
	runGitTest(t, repoPath, "add", "a.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	matches, _, err := GrepContent(context.Background(), repoPath, "a.b", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Line != 1 {
		t.Fatalf("expected only the literal a.b line, got %#v", matches)
	}
}
