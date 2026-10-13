package gitdiff

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

// A remote diff runs as one SSH invocation: every SSH connection costs a full
// handshake, which dominates a refresh when operations each open their own.
//
// The batch script runs its steps in order and reports each as a
// length-prefixed record, so any filename or diff content round-trips
// exactly:
//
//	hiveryn-step <index> <exit status> <stdout bytes> <stderr bytes>\n<stdout><stderr>
//
// It stops after the first failed step and exits 0, so the daemon learns which
// operation failed and its stderr. A non-zero exit is therefore a transport or
// setup failure (ssh, mktemp), reported with the SSH error. Output is staged in
// a private temporary directory outside the repository, removed on exit.
const batchPrelude = `hiveryn_t=$(mktemp -d) || exit 1
trap 'rm -rf "$hiveryn_t"' EXIT
trap 'exit 129' HUP INT TERM
step() {
  { eval "$2"; } >"$hiveryn_t/$1.out" 2>"$hiveryn_t/$1.err"
  hiveryn_s=$?
  printf 'hiveryn-step %s %d %d %d\n' "$1" "$hiveryn_s" $(wc -c <"$hiveryn_t/$1.out") $(wc -c <"$hiveryn_t/$1.err") &&
    cat "$hiveryn_t/$1.out" "$hiveryn_t/$1.err" || exit 1
  return "$hiveryn_s"
}
`

const batchRecordPrefix = "hiveryn-step "

// batchStep is one operation of a remote batch: name identifies it in errors,
// script is the shell command run (in the repository after the cd step).
type batchStep struct {
	name   string
	script string
}

type batchResult struct {
	status int
	stdout string
	stderr string
}

// remoteBatch is the outcome of one batch: the records of the steps that ran.
type remoteBatch struct {
	alias    string
	repoPath string
	steps    []batchStep
	results  []batchResult
}

// The first two steps of every batch locate the repository; their indices
// are fixed so callers can tell a missing directory from a Git failure.
const (
	stepRepoDir = 0
	stepRepoCd  = 1
)

func runRemoteBatch(ctx context.Context, alias, repoPath string, steps ...batchStep) (*remoteBatch, error) {
	steps = append([]batchStep{
		{name: "repository directory check", script: "test -d " + remoteexec.Quote(repoPath)},
		{name: "cd to repository", script: "cd " + remoteexec.Quote(repoPath)},
	}, steps...)
	var script strings.Builder
	script.WriteString(batchPrelude)
	for i, step := range steps {
		fmt.Fprintf(&script, "step %d %s || exit 0\n", i, remoteexec.Quote(step.script))
	}
	out, err := remoteexec.Run(ctx, alias, script.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("gitdiff: remote diff of %q failed: %w", repoPath, err)
	}
	results, err := parseBatchOutput(out, len(steps))
	if err != nil {
		return nil, fmt.Errorf("gitdiff: remote diff of %q on SSH %s returned malformed output: %w", repoPath, alias, err)
	}
	batch := &remoteBatch{alias: alias, repoPath: repoPath, steps: steps, results: results}
	if r, ok := batch.result(stepRepoDir); ok && r.status == 1 {
		return nil, &domain.NotFoundError{Resource: "repo_path", ID: repoPath}
	}
	if _, err := batch.output(stepRepoDir); err != nil {
		return nil, err
	}
	if _, err := batch.output(stepRepoCd); err != nil {
		return nil, err
	}
	return batch, nil
}

// parseBatchOutput reads the records in order; each must carry the next index.
func parseBatchOutput(out []byte, maxSteps int) ([]batchResult, error) {
	var results []batchResult
	for len(out) > 0 {
		line, rest, ok := bytes.Cut(out, []byte("\n"))
		if !ok || !bytes.HasPrefix(line, []byte(batchRecordPrefix)) {
			return nil, fmt.Errorf("expected step record %d, got %q", len(results), truncateForError(line))
		}
		fields := strings.Fields(string(line[len(batchRecordPrefix):]))
		if len(fields) != 4 {
			return nil, fmt.Errorf("malformed step record %q", line)
		}
		nums := make([]int, 4)
		for i, field := range fields {
			n, err := strconv.Atoi(field)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("malformed step record %q", line)
			}
			nums[i] = n
		}
		if nums[0] != len(results) || nums[0] >= maxSteps {
			return nil, fmt.Errorf("step record %d out of order (expected %d)", nums[0], len(results))
		}
		if nums[2]+nums[3] > len(rest) {
			return nil, fmt.Errorf("step %d output truncated: want %d bytes, have %d", nums[0], nums[2]+nums[3], len(rest))
		}
		results = append(results, batchResult{
			status: nums[1],
			stdout: string(rest[:nums[2]]),
			stderr: string(rest[nums[2] : nums[2]+nums[3]]),
		})
		out = rest[nums[2]+nums[3]:]
	}
	return results, nil
}

func truncateForError(b []byte) []byte {
	const limit = 200
	if len(b) > limit {
		return b[:limit]
	}
	return b
}

func (b *remoteBatch) result(i int) (batchResult, bool) {
	if i >= len(b.results) {
		return batchResult{}, false
	}
	return b.results[i], true
}

// output returns step i's stdout, or an error naming the operation: its exit
// status and stderr if it failed, or that the batch ended before reaching it.
// A step that did not succeed is never read as empty output.
func (b *remoteBatch) output(i int) (string, error) {
	r, ok := b.result(i)
	if !ok {
		return "", fmt.Errorf("gitdiff: %s did not run in %q on SSH %s: remote batch ended after %d of %d steps", b.steps[i].name, b.repoPath, b.alias, len(b.results), len(b.steps))
	}
	if r.status != 0 {
		return "", fmt.Errorf("gitdiff: %s failed in %q on SSH %s: exit status %d (stderr: %s)", b.steps[i].name, b.repoPath, b.alias, r.status, r.stderr)
	}
	return r.stdout, nil
}

// gitStep is a batch step running git with the same arguments a local diff uses.
func gitStep(args ...string) batchStep {
	return batchStep{name: "git " + strings.Join(args, " "), script: gitScript(args...)}
}

// gitScript is the shell command for git with args, each quoted.
func gitScript(args ...string) string {
	return remoteexec.Args(append([]string{"git"}, gitCommandArgs(args)...)...)
}
