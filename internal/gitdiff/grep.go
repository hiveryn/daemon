package gitdiff

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// maxGrepLineBytes caps how much of a matched line is kept — minified
// bundles can have megabyte lines that would bloat the response.
const maxGrepLineBytes = 500

// ContentMatch is one matched line from a content search.
type ContentMatch struct {
	// Path is relative to the searched dir, "/"-separated.
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepContent runs a case-insensitive fixed-string content search over the
// tracked + untracked (unignored) files under dir via one `git grep`
// subprocess, streaming stdout so it can stop the process as soon as
// maxMatches lines are collected (truncated=true). dir may be any directory
// inside a work tree; paths come back relative to it.
func GrepContent(ctx context.Context, dir, query string, maxMatches int) ([]ContentMatch, bool, error) {
	grepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// -z NUL-terminates the path field, making "path\0line:text" records
	// unambiguous for any filename. -I skips binaries. --fixed-strings keeps
	// user input from being parsed as a regex. --relative scopes paths to dir
	// when it is a subdirectory of the repo.
	cmd := exec.CommandContext(grepCtx, "git",
		"-c", "core.quotepath=false",
		"grep", "-I", "-i", "-n", "-z", "--no-color", "--untracked",
		"--fixed-strings", "-e", query, "--", ".",
	)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("gitdiff: git grep stdout pipe in %q: %w", dir, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("gitdiff: start git grep in %q: %w", dir, err)
	}

	var matches []ContentMatch
	truncated := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if len(matches) >= maxMatches {
			truncated = true
			cancel() // kill git; we have enough
			break
		}
		match, err := parseGrepLine(scanner.Text())
		if err != nil {
			cancel()
			_ = cmd.Wait()
			return nil, false, fmt.Errorf("%w (in %q)", err, dir)
		}
		matches = append(matches, match)
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()
	if truncated {
		// We killed the process deliberately — its exit status is meaningless.
		return matches, true, nil
	}
	if scanErr != nil {
		return nil, false, fmt.Errorf("gitdiff: read git grep output in %q: %w", dir, scanErr)
	}
	if waitErr != nil {
		// Exit code 1 with no stderr is git grep's "no matches" — not an error.
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 && stderr.Len() == 0 {
			return matches, false, nil
		}
		return nil, false, fmt.Errorf("gitdiff: git grep failed in %q: %w (stderr: %s)", dir, waitErr, stderr.String())
	}
	return matches, false, nil
}

// parseGrepLine parses one git grep -n -z record. Depending on git version
// -z NUL-separates either just the path ("path\0line:text") or the line
// number too ("path\0line\0text") — accept both.
func parseGrepLine(raw string) (ContentMatch, error) {
	pathEnd := strings.IndexByte(raw, 0)
	if pathEnd == -1 {
		return ContentMatch{}, fmt.Errorf("gitdiff: malformed git grep record (no NUL): %q", truncateForError(raw))
	}
	rest := raw[pathEnd+1:]
	lineEnd := strings.IndexAny(rest, "\x00:")
	if lineEnd == -1 {
		return ContentMatch{}, fmt.Errorf("gitdiff: malformed git grep record (no line separator): %q", truncateForError(raw))
	}
	lineNo, err := strconv.Atoi(rest[:lineEnd])
	if err != nil {
		return ContentMatch{}, fmt.Errorf("gitdiff: malformed git grep line number: %q", truncateForError(raw))
	}
	return ContentMatch{
		Path: raw[:pathEnd],
		Line: lineNo,
		Text: truncateLine(rest[lineEnd+1:]),
	}, nil
}

func truncateLine(text string) string {
	if len(text) <= maxGrepLineBytes {
		return text
	}
	return text[:maxGrepLineBytes]
}

func truncateForError(raw string) string {
	if len(raw) > 120 {
		return raw[:120] + "…"
	}
	return raw
}
