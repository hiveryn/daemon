// Package gitdiff computes working-tree and single-commit git diffs for a
// repo path, returning file-level metadata plus raw unified diff text.
// Hunk/line-level parsing is deliberately not performed here — API
// consumers parse the raw diff text themselves.
package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// runGit runs a git command, treating any non-zero exit as failure.
func runGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	return runGitCommand(ctx, repoPath, false, args...)
}

// runGitAllowChanges runs a git command, tolerating exit code 1 — used only
// for `git diff --no-index`, which exits 1 when the compared files differ
// (the expected outcome for every untracked file).
func runGitAllowChanges(ctx context.Context, repoPath string, args ...string) (string, error) {
	return runGitCommand(ctx, repoPath, true, args...)
}

func runGitCommand(ctx context.Context, repoPath string, allowExitCodeOne bool, args ...string) (string, error) {
	return runGitCommandStdin(ctx, repoPath, "", allowExitCodeOne, args...)
}

// runGitCommandStdin behaves like runGitCommand but additionally feeds stdin
// to the git process when non-empty (used by check-ignore's --stdin mode).
func runGitCommandStdin(ctx context.Context, repoPath, stdin string, allowExitCodeOne bool, args ...string) (string, error) {
	commandArgs := append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Dir = repoPath
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	var exitErr *exec.ExitError
	if allowExitCodeOne && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return stdout.String(), nil
	}

	return "", fmt.Errorf("gitdiff: git %s failed in %q: %w (stderr: %s)", strings.Join(args, " "), repoPath, err, stderr.String())
}
