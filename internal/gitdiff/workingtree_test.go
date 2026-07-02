package gitdiff

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestLoadWorkingTreeDiff_MixedChanges(t *testing.T) {
	repoPath := createRepoWithMixedChanges(t)

	result, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.RepoPath != repoPath {
		t.Fatalf("unexpected repo path: %#v", result)
	}
	if result.Summary.Files != 2 || result.Summary.StagedFiles != 1 || result.Summary.UnstagedFiles != 2 {
		t.Fatalf("unexpected summary counts: %#v", result.Summary)
	}
	if result.Summary.Additions != 4 || result.Summary.Deletions != 2 {
		t.Fatalf("unexpected line counts: %#v", result.Summary)
	}

	tracked := findFile(t, result.Files, "file.txt")
	if tracked.Status != "modified" || len(tracked.Sections) != 2 {
		t.Fatalf("tracked file = %#v", tracked)
	}
	if tracked.Sections[0].Kind != sectionKindStaged || tracked.Sections[1].Kind != sectionKindUnstaged {
		t.Fatalf("tracked sections = %#v", tracked.Sections)
	}
	if !strings.Contains(tracked.RawUnifiedDiff, "diff --git file.txt file.txt") {
		t.Fatalf("tracked raw diff = %q", tracked.RawUnifiedDiff)
	}
	if tracked.Sections[0].Additions != 1 || tracked.Sections[0].Deletions != 1 {
		t.Fatalf("tracked staged section counts = %#v", tracked.Sections[0])
	}

	untracked := findFile(t, result.Files, "untracked.txt")
	if untracked.Status != "untracked" || len(untracked.Sections) != 1 || untracked.Sections[0].Kind != sectionKindUnstaged {
		t.Fatalf("untracked file = %#v", untracked)
	}
	if untracked.Sections[0].Additions != 2 {
		t.Fatalf("untracked section = %#v", untracked.Sections[0])
	}
}

func TestLoadWorkingTreeDiff_CleanRepo(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\n")
	runGitTest(t, repoPath, "add", "file.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	result, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected no files, got %#v", result.Files)
	}
	if result.Summary.Files != 0 || result.Summary.Additions != 0 || result.Summary.Deletions != 0 {
		t.Fatalf("expected zeroed summary, got %#v", result.Summary)
	}
}

func TestLoadWorkingTreeDiff_BinaryFile(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "keep.txt"), "keep\n")
	runGitTest(t, repoPath, "add", "keep.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	binaryPath := filepath.Join(repoPath, "image.bin")
	if err := writeBinary(binaryPath); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoPath, "add", "image.bin")

	result, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	file := findFile(t, result.Files, "image.bin")
	if !file.IsBinary || file.Status != "new" {
		t.Fatalf("expected binary new file, got %#v", file)
	}
	if file.Additions != 0 || file.Deletions != 0 {
		t.Fatalf("expected zero line counts for binary file, got %#v", file)
	}
}

func TestLoadWorkingTreeDiff_RenamedFile(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "old.txt"), "one\ntwo\nthree\nfour\nfive\n")
	runGitTest(t, repoPath, "add", "old.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	runGitTest(t, repoPath, "mv", "old.txt", "new.txt")

	result, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	file := findFile(t, result.Files, "new.txt")
	if file.Status != "renamed" || file.OldPath != "old.txt" {
		t.Fatalf("expected renamed file, got %#v", file)
	}
}

func TestLoadWorkingTreeDiff_RepoPathNotFound(t *testing.T) {
	_, err := LoadWorkingTreeDiff(context.Background(), filepath.Join(t.TempDir(), "missing"))
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestLoadWorkingTreeDiff_RepoPathNotADirectory(t *testing.T) {
	repoPath := t.TempDir()
	filePath := filepath.Join(repoPath, "file")
	writeFile(t, filePath, "not a directory")

	_, err := LoadWorkingTreeDiff(context.Background(), filePath)
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestLoadWorkingTreeDiff_LargeFileTruncation(t *testing.T) {
	original := MaxRawDiffBytes
	MaxRawDiffBytes = 10
	t.Cleanup(func() { MaxRawDiffBytes = original })

	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "big.txt"), strings.Repeat("line\n", 200))
	runGitTest(t, repoPath, "add", "big.txt")

	result, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	file := findFile(t, result.Files, "big.txt")
	if !file.Truncated || file.RawUnifiedDiff != "" {
		t.Fatalf("expected truncated empty diff, got %#v", file)
	}
	if file.RawDiffBytes <= MaxRawDiffBytes {
		t.Fatalf("expected true size to exceed cap, got %d", file.RawDiffBytes)
	}
}

func writeBinary(path string) error {
	return writeBinaryContent(path, []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x01, 0x02, 0xff, 0xfe})
}
