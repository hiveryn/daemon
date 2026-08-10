package gitdiff

import (
	"context"
	"fmt"
	"strings"
)

// StatusEntry is one changed path from `git status --porcelain`. Index and
// Worktree carry the two porcelain columns verbatim (" " when unchanged on
// that side; "?" both sides for untracked), so consumers can distinguish
// staged from unstaged changes without re-deriving git semantics.
type StatusEntry struct {
	// Path is repo-relative, "/"-separated.
	Path     string `json:"path"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
	// OrigPath is set for renames/copies: the source path.
	OrigPath string `json:"orig_path,omitempty"`
}

// RepoRoot resolves the work-tree root ("git rev-parse --show-toplevel")
// for any directory inside a work tree.
func RepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Status lists every changed path in the repo containing dir, via one
// `git status --porcelain=v1 -z` invocation. Untracked files are expanded
// individually (--untracked-files=all) so a new directory reports its files,
// not just the directory. Paths are relative to the repo root.
func Status(ctx context.Context, dir string) ([]StatusEntry, error) {
	out, err := runGit(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimRight(out, "\x00")
	if trimmed == "" {
		return nil, nil
	}
	tokens := strings.Split(trimmed, "\x00")

	var entries []StatusEntry
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		// Each record starts "XY <path>"; -z uses NUL separators, so a token
		// shorter than the "XY " prefix is a malformed record.
		if len(token) < 4 || token[2] != ' ' {
			return nil, fmt.Errorf("gitdiff: malformed porcelain status record %q in %q", token, dir)
		}
		entry := StatusEntry{
			Index:    token[0:1],
			Worktree: token[1:2],
			Path:     token[3:],
		}
		// Renames/copies emit the source path as the following NUL token.
		if entry.Index == "R" || entry.Index == "C" || entry.Worktree == "R" || entry.Worktree == "C" {
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("gitdiff: porcelain rename record %q missing source path in %q", token, dir)
			}
			entry.OrigPath = tokens[i]
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
