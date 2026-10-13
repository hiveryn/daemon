package gitdiff

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

// LoadCommitDiff computes the diff introduced by a single commit. Root
// commits are diffed against the empty tree; merge commits are diffed
// against their first parent (git's `diff-tree -p` prints nothing for
// merge commits by default, mirroring `git log -p`). A remote repository is
// read in one SSH invocation.
func LoadCommitDiff(ctx context.Context, repoPath, sha string) (CommitDiff, error) {
	if alias := SSHFromContext(ctx); alias != "" {
		return loadRemoteCommitDiff(ctx, alias, repoPath, sha)
	}
	if err := checkRepoPath(repoPath); err != nil {
		return CommitDiff{}, err
	}

	output, err := runGit(ctx, repoPath, commitParentsArgs(sha)...)
	if err != nil {
		return CommitDiff{}, &domain.NotFoundError{Resource: "commit", ID: sha}
	}
	resolvedSHA, parents, err := parseCommitParents(sha, output)
	if err != nil {
		return CommitDiff{}, &domain.NotFoundError{Resource: "commit", ID: sha}
	}

	var args []string
	if len(parents) >= 2 {
		args = mergeDiffArgs(parents[0], resolvedSHA)
	} else {
		args = commitDiffArgs(resolvedSHA)
	}
	raw, err := runGit(ctx, repoPath, args...)
	if err != nil {
		return CommitDiff{}, err
	}
	return buildCommitDiff(repoPath, resolvedSHA, parents, raw)
}

// loadRemoteCommitDiff resolves the commit and chooses its diff in the remote
// shell, from the rev-list record, by the same rule the local path applies:
// two or more parents diff against the first parent, otherwise diff-tree
// --root. The daemon reclassifies the same record, so both always agree.
func loadRemoteCommitDiff(ctx context.Context, alias, repoPath, sha string) (CommitDiff, error) {
	const (
		stepParents = stepRepoCd + 1
		stepDiff    = stepRepoCd + 2
	)
	parentsOut := `"$hiveryn_t/` + strconv.Itoa(stepParents) + `.out"`
	diffScript := `read -r c p1 p2 rest <` + parentsOut + `; ` +
		`if [ -n "$p2" ]; then ` + gitScript(mergeDiffFlags...) + ` "$p1" "$c"; ` +
		`elif [ -n "$c" ]; then ` + gitScript(commitDiffFlags...) + ` "$c"; fi`

	batch, err := runRemoteBatch(ctx, alias, repoPath,
		gitStep(commitParentsArgs(sha)...),
		batchStep{name: "commit diff", script: diffScript},
	)
	if err != nil {
		return CommitDiff{}, err
	}
	output, err := batch.output(stepParents)
	if err != nil {
		if r, ran := batch.result(stepParents); ran && r.status != 0 {
			return CommitDiff{}, &domain.NotFoundError{Resource: "commit", ID: sha}
		}
		return CommitDiff{}, err
	}
	resolvedSHA, parents, err := parseCommitParents(sha, output)
	if err != nil {
		return CommitDiff{}, &domain.NotFoundError{Resource: "commit", ID: sha}
	}
	raw, err := batch.output(stepDiff)
	if err != nil {
		return CommitDiff{}, err
	}
	return buildCommitDiff(repoPath, resolvedSHA, parents, raw)
}

func commitParentsArgs(sha string) []string {
	return []string{"rev-list", "--parents", "-n", "1", sha}
}

var (
	mergeDiffFlags  = []string{"diff", "--no-color", "--no-ext-diff", "--find-renames", "--no-prefix"}
	commitDiffFlags = []string{"diff-tree", "--no-color", "--no-ext-diff", "--find-renames", "--no-prefix", "--no-commit-id", "-r", "-p", "--root"}
)

func mergeDiffArgs(parent, sha string) []string {
	return append(append([]string{}, mergeDiffFlags...), parent, sha)
}

func commitDiffArgs(sha string) []string {
	return append(append([]string{}, commitDiffFlags...), sha)
}

func buildCommitDiff(repoPath, resolvedSHA string, parents []string, raw string) (CommitDiff, error) {
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

	parentSHA := ""
	if len(parents) > 0 {
		parentSHA = parents[0]
	}
	return CommitDiff{
		SHA:       resolvedSHA,
		ParentSHA: parentSHA,
		IsMerge:   len(parents) >= 2,
		RepoPath:  repoPath,
		Files:     files,
		Summary:   summary,
	}, nil
}

// parseCommitParents reads `git rev-list --parents -n 1 <sha>` output: the
// resolved SHA followed by its parents. This single command classifies root
// commits (0 parents), ordinary commits (1 parent), and merges (2+ parents)
// uniformly. A bad ref makes the git command itself fail; a valid but
// non-commit object (e.g. a blob SHA) makes git succeed with empty output,
// which is treated as "not a commit" here. Callers map both to a
// domain.NotFoundError, since from the API's perspective an unresolvable or
// non-commit SHA is the same "commit not found" condition.
func parseCommitParents(sha, output string) (resolvedSHA string, parents []string, err error) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("gitdiff: %q did not resolve to a commit", sha)
	}
	return fields[0], fields[1:], nil
}
