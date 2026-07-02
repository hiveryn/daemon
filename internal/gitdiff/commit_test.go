package gitdiff

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestLoadCommitDiff_RegularCommit(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "root.txt"), "root\n")
	runGitTest(t, repoPath, "add", "root.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root commit")
	rootSHA := commitSHA(t, repoPath)

	writeFile(t, filepath.Join(repoPath, "second.txt"), "second\n")
	runGitTest(t, repoPath, "add", "second.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "second commit")
	secondSHA := commitSHA(t, repoPath)

	result, err := LoadCommitDiff(context.Background(), repoPath, secondSHA)
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA != secondSHA || result.ParentSHA != rootSHA || result.IsMerge {
		t.Fatalf("unexpected commit metadata: %#v", result)
	}
	file := findFile(t, result.Files, "second.txt")
	if file.Status != "new" || len(file.Sections) != 0 {
		t.Fatalf("unexpected file: %#v", file)
	}
	if result.Summary.Files != 1 || result.Summary.Additions != 1 {
		t.Fatalf("unexpected summary: %#v", result.Summary)
	}
}

func TestLoadCommitDiff_RootCommit(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "a.txt"), "a\n")
	writeFile(t, filepath.Join(repoPath, "b.txt"), "b\n")
	runGitTest(t, repoPath, "add", "a.txt", "b.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root commit")
	rootSHA := commitSHA(t, repoPath)

	result, err := LoadCommitDiff(context.Background(), repoPath, rootSHA)
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA != rootSHA || result.ParentSHA != "" || result.IsMerge {
		t.Fatalf("unexpected commit metadata: %#v", result)
	}
	if result.Summary.Files != 2 {
		t.Fatalf("expected 2 files against empty tree, got %#v", result.Summary)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		file := findFile(t, result.Files, name)
		if file.Status != "new" {
			t.Fatalf("expected %q to be new against empty tree, got %#v", name, file)
		}
	}
}

func TestLoadCommitDiff_MergeCommit(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "root.txt"), "root\n")
	runGitTest(t, repoPath, "add", "root.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root commit")
	rootSHA := commitSHA(t, repoPath)

	writeFile(t, filepath.Join(repoPath, "second.txt"), "second\n")
	runGitTest(t, repoPath, "add", "second.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "second commit")
	secondSHA := commitSHA(t, repoPath)

	baseBranch := runGitTest(t, repoPath, "branch", "--show-current")

	runGitTest(t, repoPath, "checkout", "-q", "-b", "feature", rootSHA)
	writeFile(t, filepath.Join(repoPath, "b.txt"), "feature\n")
	runGitTest(t, repoPath, "add", "b.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "feature commit")

	runGitTest(t, repoPath, "checkout", "-q", baseBranch)
	runGitTest(t, repoPath, "merge", "-q", "--no-ff", "feature", "-m", "merge feature")
	mergeSHA := commitSHA(t, repoPath)

	result, err := LoadCommitDiff(context.Background(), repoPath, mergeSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsMerge || result.ParentSHA != secondSHA {
		t.Fatalf("expected merge diffed against first parent %q, got %#v", secondSHA, result)
	}
	file := findFile(t, result.Files, "b.txt")
	if file.Status != "new" {
		t.Fatalf("expected feature file introduced by merge, got %#v", file)
	}
}

func TestLoadCommitDiff_UnknownSHA(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "root.txt"), "root\n")
	runGitTest(t, repoPath, "add", "root.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root commit")

	_, err := LoadCommitDiff(context.Background(), repoPath, strings.Repeat("a", 40))
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestLoadCommitDiff_NonCommitObject(t *testing.T) {
	repoPath := initRepo(t)
	filePath := filepath.Join(repoPath, "root.txt")
	writeFile(t, filePath, "root\n")
	runGitTest(t, repoPath, "add", "root.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root commit")

	blobSHA := runGitTest(t, repoPath, "hash-object", filePath)

	_, err := LoadCommitDiff(context.Background(), repoPath, blobSHA)
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}
