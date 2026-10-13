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

// gitCommandArgs prefixes args with the options every diff command shares,
// locally and in a remote batch.
func gitCommandArgs(args []string) []string {
	return append([]string{"-c", "core.quotepath=false"}, args...)
}

// runGitCommand runs git locally; remote diffs run as one batch (batch.go).
func runGitCommand(ctx context.Context, repoPath string, allowExitCodeOne bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitCommandArgs(args)...)
	cmd.Dir = repoPath

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

// WithSSH routes Git operations to the configured account without changing paths.
func WithSSH(ctx context.Context, alias string) context.Context {
	return context.WithValue(ctx, sshKey{}, alias)
}

type sshKey struct{}

func SSHFromContext(ctx context.Context) string { s, _ := ctx.Value(sshKey{}).(string); return s }
