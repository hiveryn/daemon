package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

const testAlias = "bk"

// remoteContext routes diffs through a fake ssh that runs each remote script
// locally; repositories keep their absolute paths, so the same repository can
// be read locally and "remotely" and the results compared.
func remoteContext(t *testing.T) (context.Context, *sshtest.Fake) {
	t.Helper()
	fake := sshtest.Install(t)
	return WithSSH(context.Background(), testAlias), fake
}

// slowSSH delays every ssh invocation, standing in for a high-latency
// connection whose handshake dominates each call.
func slowSSH(t *testing.T, delay time.Duration) {
	t.Helper()
	inner, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nsleep %.3f\nexec %s \"$@\"\n", delay.Seconds(), inner)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func assertInvocations(t *testing.T, fake *sshtest.Fake, want int) {
	t.Helper()
	if got := len(fake.Commands(testAlias)); got != want {
		t.Fatalf("ssh invocations = %d, want %d", got, want)
	}
}

// createRichRepo covers every kind of working-tree change the diff reports,
// including filenames and contents that look like batch framing or diff
// headers.
func createRichRepo(t *testing.T) string {
	t.Helper()
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo\nthree\n")
	writeFile(t, filepath.Join(repoPath, "old.txt"), "one\ntwo\nthree\nfour\nfive\n")
	writeFile(t, filepath.Join(repoPath, "gone.txt"), "bye\n")
	writeFile(t, filepath.Join(repoPath, "dir with space/naïve (1).md"), "a\n")
	runGitTest(t, repoPath, "add", ".")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo staged\nthree\n")
	runGitTest(t, repoPath, "add", "file.txt")
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo unstaged\nthree\n")
	runGitTest(t, repoPath, "mv", "old.txt", "new.txt")
	runGitTest(t, repoPath, "rm", "-q", "gone.txt")
	if err := writeBinary(filepath.Join(repoPath, "image.bin")); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoPath, "add", "image.bin")
	writeFile(t, filepath.Join(repoPath, "dir with space/naïve (1).md"), "a\nb\n")
	writeFile(t, filepath.Join(repoPath, "untracked.txt"), "hiveryn-step 9 0 0 0\ndiff --git a b\nno trailing newline")
	writeFile(t, filepath.Join(repoPath, "nested/deeper/'quote' $x.go"), "package x\n")
	if err := os.WriteFile(filepath.Join(repoPath, "blob.bin"), []byte{0, 1, 2, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	return repoPath
}

func TestRemoteWorkingTreeDiffMatchesLocalInOneInvocation(t *testing.T) {
	repoPath := createRichRepo(t)
	local, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Files) != 8 {
		t.Fatalf("fixture produced %d files: %#v", len(local.Files), local.Files)
	}

	ctx, fake := remoteContext(t)
	remote, err := LoadWorkingTreeDiff(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(local, remote) {
		t.Fatalf("remote diff differs from local:\nlocal  %#v\nremote %#v", local, remote)
	}
	assertInvocations(t, fake, 1)
}

func TestRemoteWorkingTreeDiffTruncationMatchesLocal(t *testing.T) {
	original := MaxRawDiffBytes
	MaxRawDiffBytes = 10
	t.Cleanup(func() { MaxRawDiffBytes = original })
	repoPath := createRichRepo(t)
	local, err := LoadWorkingTreeDiff(context.Background(), repoPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := remoteContext(t)
	remote, err := LoadWorkingTreeDiff(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(local, remote) || !remote.Files[0].Truncated {
		t.Fatalf("truncated remote diff differs:\nlocal  %#v\nremote %#v", local, remote)
	}
}

func TestRemoteWorkingTreeDiffClean(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\n")
	runGitTest(t, repoPath, "add", "file.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")
	ctx, fake := remoteContext(t)
	result, err := LoadWorkingTreeDiff(ctx, repoPath)
	if err != nil || len(result.Files) != 0 || result.Files == nil {
		t.Fatalf("clean remote repo: %#v, %v", result, err)
	}
	assertInvocations(t, fake, 1)
}

func TestRemoteCommitDiffMatchesLocalInOneInvocation(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "root.txt"), "root\n")
	writeFile(t, filepath.Join(repoPath, "old.txt"), "one\ntwo\nthree\nfour\nfive\n")
	runGitTest(t, repoPath, "add", ".")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root")
	root := commitSHA(t, repoPath)
	baseBranch := runGitTest(t, repoPath, "branch", "--show-current")

	if err := os.Mkdir(filepath.Join(repoPath, "dir with space"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoPath, "mv", "old.txt", "dir with space/new 'name'.txt")
	if err := writeBinary(filepath.Join(repoPath, "image.bin")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoPath, "root.txt"), "root\nhiveryn-step 3 0 0 0\n")
	runGitTest(t, repoPath, "add", ".")
	runGitTest(t, repoPath, "commit", "-q", "-m", "second")
	second := commitSHA(t, repoPath)

	runGitTest(t, repoPath, "checkout", "-q", "-b", "feature", root)
	writeFile(t, filepath.Join(repoPath, "b.txt"), "feature\n")
	runGitTest(t, repoPath, "add", "b.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "feature")
	runGitTest(t, repoPath, "checkout", "-q", baseBranch)
	runGitTest(t, repoPath, "merge", "-q", "--no-ff", "feature", "-m", "merge")
	merge := commitSHA(t, repoPath)

	ctx, fake := remoteContext(t)
	for _, tc := range []struct {
		name, sha    string
		parent       string
		isMerge      bool
		filesAtLeast int
	}{
		{"root", root, "", false, 2},
		{"ordinary", second[:10], root, false, 3},
		{"merge", merge, second, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, err := LoadCommitDiff(context.Background(), repoPath, tc.sha)
			if err != nil {
				t.Fatal(err)
			}
			before := len(fake.Commands(testAlias))
			remote, err := LoadCommitDiff(ctx, repoPath, tc.sha)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(local, remote) {
				t.Fatalf("remote commit diff differs:\nlocal  %#v\nremote %#v", local, remote)
			}
			if remote.ParentSHA != tc.parent || remote.IsMerge != tc.isMerge || len(remote.Files) < tc.filesAtLeast {
				t.Fatalf("unexpected commit diff: %#v", remote)
			}
			if got := len(fake.Commands(testAlias)) - before; got != 1 {
				t.Fatalf("ssh invocations = %d, want 1", got)
			}
		})
	}
}

func TestRemoteDiffMissingRepositoryIsNotFound(t *testing.T) {
	ctx, _ := remoteContext(t)
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, file, "x")
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		var notFound *domain.NotFoundError
		if _, err := LoadWorkingTreeDiff(ctx, path); !errors.As(err, &notFound) || notFound.Resource != "repo_path" {
			t.Fatalf("working tree %s: %v", path, err)
		}
		if _, err := LoadCommitDiff(ctx, path, "HEAD"); !errors.As(err, &notFound) || notFound.Resource != "repo_path" {
			t.Fatalf("commit %s: %v", path, err)
		}
	}
}

// A Git failure names the operation and keeps git's stderr; it is never an
// empty diff or a not-found. Outside a repository `git diff` rejects --cached.
func TestRemoteDiffGitFailureNamesOperation(t *testing.T) {
	ctx, _ := remoteContext(t)
	_, err := LoadWorkingTreeDiff(ctx, t.TempDir())
	var notFound *domain.NotFoundError
	if err == nil || errors.As(err, &notFound) || !strings.Contains(err.Error(), "git diff --cached --no-ext-diff") ||
		!strings.Contains(err.Error(), "unknown option `cached'") || !strings.Contains(err.Error(), "SSH "+testAlias) {
		t.Fatalf("non-repository: %v", err)
	}
}

func TestRemoteDiffUntrackedFailureNamesOperation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "secret.txt"), "x\n")
	if err := os.Chmod(filepath.Join(repoPath, "secret.txt"), 0); err != nil {
		t.Fatal(err)
	}
	ctx, _ := remoteContext(t)
	_, err := LoadWorkingTreeDiff(ctx, repoPath)
	if err == nil || !strings.Contains(err.Error(), "untracked file diff failed") || !strings.Contains(err.Error(), "secret.txt") {
		t.Fatalf("unreadable untracked file: %v", err)
	}
}

func TestRemoteCommitDiffUnknownCommitIsNotFound(t *testing.T) {
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "root.txt"), "root\n")
	runGitTest(t, repoPath, "add", "root.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "root")
	blob := runGitTest(t, repoPath, "hash-object", filepath.Join(repoPath, "root.txt"))
	ctx, _ := remoteContext(t)
	for _, sha := range []string{strings.Repeat("a", 40), blob} {
		var notFound *domain.NotFoundError
		if _, err := LoadCommitDiff(ctx, repoPath, sha); !errors.As(err, &notFound) || notFound.Resource != "commit" {
			t.Fatalf("%s: %v", sha, err)
		}
	}
}

// An unreachable machine is an SSH error for both diffs — never a clean diff
// and never a missing commit.
func TestRemoteDiffUnreachableMachine(t *testing.T) {
	repoPath := createRepoWithMixedChanges(t)
	ctx, fake := remoteContext(t)
	fake.SetDown(t, testAlias, true)
	var notFound *domain.NotFoundError
	if _, err := LoadWorkingTreeDiff(ctx, repoPath); err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("working tree: %v", err)
	}
	if _, err := LoadCommitDiff(ctx, repoPath, "HEAD"); err == nil || errors.As(err, &notFound) || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("commit: %v", err)
	}
}

func TestRemoteDiffHighLatencyCostsOneRoundTrip(t *testing.T) {
	repoPath := createRichRepo(t)
	ctx, fake := remoteContext(t)
	const delay = time.Second
	slowSSH(t, delay)
	// The first run of a new executable can be slow (macOS scans it); keep
	// that out of the measurement.
	if _, err := LoadWorkingTreeDiff(ctx, repoPath); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := LoadWorkingTreeDiff(ctx, repoPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCommitDiff(ctx, repoPath, "HEAD"); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	assertInvocations(t, fake, 3)
	// Four plus three sequential calls would take at least 7 delays.
	if elapsed >= 4*delay {
		t.Fatalf("two diffs took %s with a %s handshake", elapsed, delay)
	}
	t.Logf("working-tree + commit diff with a %s handshake: %s", delay, elapsed)
}

func TestRemoteDiffHonorsDeadline(t *testing.T) {
	repoPath := createRepoWithMixedChanges(t)
	ctx, _ := remoteContext(t)
	slowSSH(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := LoadWorkingTreeDiff(ctx, repoPath)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("deadline not honored: %s", elapsed)
	}
}

func TestParseBatchOutputRejectsIncompleteOutput(t *testing.T) {
	for name, out := range map[string]string{
		"garbage":       "hello\n",
		"no newline":    "hiveryn-step 0 0 0 0",
		"truncated":     "hiveryn-step 0 0 5 0\nabc",
		"out of order":  "hiveryn-step 1 0 0 0\n",
		"too many":      "hiveryn-step 0 0 0 0\nhiveryn-step 1 0 0 0\n",
		"negative size": "hiveryn-step 0 0 -1 0\n",
	} {
		if _, err := parseBatchOutput([]byte(out), 1); err == nil {
			t.Errorf("%s: accepted %q", name, out)
		}
	}
	results, err := parseBatchOutput([]byte("hiveryn-step 0 0 3 2\na\nbe\n"), 1)
	if err != nil || !reflect.DeepEqual(results, []batchResult{{status: 0, stdout: "a\nb", stderr: "e\n"}}) {
		t.Fatalf("results = %#v, %v", results, err)
	}
}

// A batch that ends early reports the operation that never ran.
func TestBatchOutputMissingStepIsAnError(t *testing.T) {
	batch := &remoteBatch{alias: testAlias, repoPath: "/r", steps: []batchStep{{name: "a"}, {name: "git diff"}}, results: []batchResult{{}}}
	if _, err := batch.output(1); err == nil || !strings.Contains(err.Error(), "git diff did not run") {
		t.Fatalf("missing step: %v", err)
	}
}
