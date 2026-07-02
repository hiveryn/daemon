package gitdiff

import (
	"context"
	"strings"
)

// CheckIgnore batch-checks names (entries of dir, not full paths) against
// gitignore rules. It never errors: if dir isn't inside a git repo, or git
// fails for any other reason, it returns an empty map so callers can treat
// "ignored" as silently unknown rather than failing the request.
func CheckIgnore(ctx context.Context, dir string, names []string) map[string]bool {
	ignored := make(map[string]bool, len(names))
	if len(names) == 0 {
		return ignored
	}

	stdin := strings.Join(names, "\x00") + "\x00"
	out, err := runGitCommandStdin(ctx, dir, stdin, true, "check-ignore", "--stdin", "-z")
	if err != nil {
		return ignored
	}

	for name := range strings.SplitSeq(strings.TrimRight(out, "\x00"), "\x00") {
		if name != "" {
			ignored[name] = true
		}
	}
	return ignored
}
