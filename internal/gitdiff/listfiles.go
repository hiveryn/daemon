package gitdiff

import (
	"context"
	"strings"
)

// IsInsideWorkTree reports whether dir is inside a git work tree.
func IsInsideWorkTree(ctx context.Context, dir string) bool {
	out, err := runGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// ListFiles returns the paths of all tracked and untracked-but-not-ignored
// files under dir ("/"-separated, relative to dir), via one `git ls-files`
// invocation. dir may be any directory inside a work tree — not just the
// repo root — in which case only its subtree is listed.
func ListFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := runGit(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(out, "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\x00"), nil
}
