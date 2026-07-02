package gitdiff

import (
	"context"
	"fmt"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

// LoadCommitDiff computes the diff introduced by a single commit. Root
// commits are diffed against the empty tree; merge commits are diffed
// against their first parent (git's `diff-tree -p` prints nothing for
// merge commits by default, mirroring `git log -p`).
func LoadCommitDiff(ctx context.Context, repoPath, sha string) (CommitDiff, error) {
	if err := checkRepoPath(repoPath); err != nil {
		return CommitDiff{}, err
	}

	resolvedSHA, parents, err := commitParents(ctx, repoPath, sha)
	if err != nil {
		return CommitDiff{}, &domain.NotFoundError{Resource: "commit", ID: sha}
	}

	var raw string
	var parentSHA string
	isMerge := len(parents) >= 2

	switch {
	case isMerge:
		parentSHA = parents[0]
		raw, err = runGit(ctx, repoPath, "diff", "--no-color", "--no-ext-diff", "--find-renames", "--no-prefix", parents[0], resolvedSHA)
	default:
		if len(parents) == 1 {
			parentSHA = parents[0]
		}
		raw, err = runGit(ctx, repoPath, "diff-tree", "--no-color", "--no-ext-diff", "--find-renames", "--no-prefix", "--no-commit-id", "-r", "-p", "--root", resolvedSHA)
	}
	if err != nil {
		return CommitDiff{}, err
	}

	parsed, err := parseDiffOutput(raw)
	if err != nil {
		return CommitDiff{}, err
	}

	files := make([]File, 0, len(parsed))
	for _, p := range parsed {
		added, deleted := countAddedDeleted(p.Raw)
		text, size, truncated := capRawDiff(p.Raw)
		files = append(files, File{
			Path:           p.Path,
			OldPath:        p.OldPath,
			Status:         p.Status,
			IsBinary:       p.IsBinary,
			Additions:      added,
			Deletions:      deleted,
			RawDiffBytes:   size,
			Truncated:      truncated,
			RawUnifiedDiff: text,
		})
	}

	summary := Summary{Files: len(files)}
	for _, file := range files {
		summary.Additions += file.Additions
		summary.Deletions += file.Deletions
	}

	return CommitDiff{
		SHA:       resolvedSHA,
		ParentSHA: parentSHA,
		IsMerge:   isMerge,
		RepoPath:  repoPath,
		Files:     files,
		Summary:   summary,
	}, nil
}

// commitParents resolves sha to its full SHA and parent list via
// `git rev-list --parents -n 1 <sha>`. This single command classifies root
// commits (0 parents), ordinary commits (1 parent), and merges (2+
// parents) uniformly. A bad ref makes the git command itself fail; a valid
// but non-commit object (e.g. a blob SHA) makes git succeed with empty
// output, which is treated as "not a commit" here too. Both cases are
// reported as a plain error — the caller maps any failure to a
// domain.NotFoundError, since from the API's perspective an unresolvable
// or non-commit SHA is the same "commit not found" condition.
func commitParents(ctx context.Context, repoPath, sha string) (resolvedSHA string, parents []string, err error) {
	output, err := runGit(ctx, repoPath, "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return "", nil, err
	}
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("gitdiff: %q did not resolve to a commit", sha)
	}
	return fields[0], fields[1:], nil
}
