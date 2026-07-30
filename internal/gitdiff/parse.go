package gitdiff

import (
	"fmt"
	"strconv"
	"strings"
)

// parsedFile is the file-level result of parsing one `diff --git` block.
// It carries the raw block text forward so callers can derive line counts,
// binary status, and size-capped output independently.
type parsedFile struct {
	Path     string
	OldPath  string
	Status   string // modified|new|deleted|renamed|copied
	IsBinary bool
	Raw      string
}

// parseDiffOutput splits raw multi-file git diff output into per-file
// parsed blocks. Empty input (no changes) returns an empty, non-nil slice.
func parseDiffOutput(raw string) ([]parsedFile, error) {
	if strings.TrimSpace(raw) == "" {
		return []parsedFile{}, nil
	}

	blocks, err := splitDiffBlocks(raw)
	if err != nil {
		return nil, err
	}
	files := make([]parsedFile, 0, len(blocks))
	for _, block := range blocks {
		file, err := parseDiffBlock(block)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

// splitDiffBlocks splits multi-file diff output into blocks at lines
// starting with "diff --git ".
func splitDiffBlocks(raw string) ([]string, error) {
	lines := strings.SplitAfter(raw, "\n")
	if len(lines) == 0 {
		return []string{}, nil
	}

	blocks := make([]string, 0, 8)
	var current strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") && current.Len() > 0 {
			blocks = append(blocks, current.String())
			current.Reset()
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		blocks = append(blocks, current.String())
	}
	if len(blocks) == 0 || !strings.HasPrefix(blocks[0], "diff --git ") {
		return nil, fmt.Errorf("gitdiff: expected git diff output to start with 'diff --git', got: %q", raw)
	}
	return blocks, nil
}

// parseDiffBlock parses one file's diff block, extracting path/status/binary
// metadata. It never inspects hunk or line content — see countAddedDeleted
// for the lightweight addition/deletion counting used instead.
func parseDiffBlock(block string) (parsedFile, error) {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if len(lines) == 0 {
		return parsedFile{}, fmt.Errorf("gitdiff: encountered empty diff block")
	}

	headerOldPath, headerNewPath, headerErr := parseDiffGitHeader(lines[0])

	file := parsedFile{
		Path:    headerNewPath,
		OldPath: headerOldPath,
		Status:  "modified",
		Raw:     block,
	}

	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "new file mode "):
			file.Status = "new"
		case strings.HasPrefix(line, "deleted file mode "):
			file.Status = "deleted"
		case strings.HasPrefix(line, "rename from "):
			file.Status = "renamed"
			file.OldPath = parseDiffPathValue(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			file.Status = "renamed"
			file.Path = parseDiffPathValue(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "copy from "):
			file.Status = "copied"
			file.OldPath = parseDiffPathValue(strings.TrimPrefix(line, "copy from "))
		case strings.HasPrefix(line, "copy to "):
			file.Status = "copied"
			file.Path = parseDiffPathValue(strings.TrimPrefix(line, "copy to "))
		case strings.HasPrefix(line, "--- "):
			path := parseDiffPathValue(strings.TrimPrefix(line, "--- "))
			if path != "/dev/null" {
				file.OldPath = path
			}
		case strings.HasPrefix(line, "+++ "):
			path := parseDiffPathValue(strings.TrimPrefix(line, "+++ "))
			if path != "/dev/null" {
				file.Path = path
			}
		case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
			file.IsBinary = true
		}
	}

	if file.Path == "" && headerNewPath != "/dev/null" {
		file.Path = headerNewPath
	}
	if file.OldPath == "" && headerOldPath != "/dev/null" {
		file.OldPath = headerOldPath
	}
	if file.Path == "" && file.OldPath != "" {
		file.Path = file.OldPath
	}
	if file.Path == "" {
		if headerErr != nil {
			return parsedFile{}, headerErr
		}
		return parsedFile{}, fmt.Errorf("gitdiff: could not determine diff path from block: %q", block)
	}
	if file.Path == file.OldPath {
		file.OldPath = ""
	}

	return file, nil
}

// countAddedDeleted counts `+`/`-` prefixed content lines inside hunks,
// mirroring the line-classification the old hunk parser performed without
// building line structs. Lines before the first hunk header (the
// `diff --git`/`---`/`+++`/mode-line preamble) are never counted, so this
// naturally skips the `---`/`+++` file-header lines and yields 0/0 for
// binary files (which contain no `@@ ` hunk headers).
func countAddedDeleted(block string) (added, deleted int) {
	inHunk := false
	for line := range strings.SplitSeq(block, "\n") {
		if !inHunk {
			if strings.HasPrefix(line, "@@ ") {
				inHunk = true
			}
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			deleted++
		}
	}
	return added, deleted
}

func parseDiffGitHeader(line string) (string, string, error) {
	const prefix = "diff --git "
	if !strings.HasPrefix(line, prefix) {
		return "", "", fmt.Errorf("gitdiff: invalid diff header %q", line)
	}
	header := strings.TrimPrefix(line, prefix)
	parts, err := parseQuotedFields(header)
	if err != nil {
		return "", "", fmt.Errorf("gitdiff: could not parse diff header %q: %w", line, err)
	}
	if len(parts) != 2 {
		if repeatedPath, ok := splitRepeatedDiffHeaderPath(header); ok {
			return repeatedPath, repeatedPath, nil
		}
		return "", "", fmt.Errorf("gitdiff: expected exactly two paths in diff header %q, got %v", line, parts)
	}
	return parseDiffPathValue(parts[0]), parseDiffPathValue(parts[1]), nil
}

func splitRepeatedDiffHeaderPath(header string) (string, bool) {
	if len(header) < 3 || len(header)%2 == 0 {
		return "", false
	}
	mid := len(header) / 2
	if header[mid] != ' ' {
		return "", false
	}
	if header[:mid] != header[mid+1:] {
		return "", false
	}
	return parseDiffPathValue(header[:mid]), true
}

// parseQuotedFields splits a header value into space-separated fields,
// honoring double-quoted fields (git quotes paths containing spaces or
// special characters even with core.quotepath=false).
func parseQuotedFields(value string) ([]string, error) {
	fields := []string{}
	for len(value) > 0 {
		value = strings.TrimLeft(value, " ")
		if value == "" {
			break
		}
		if value[0] == '"' {
			end := 1
			escaped := false
			for end < len(value) {
				if value[end] == '\\' && !escaped {
					escaped = true
					end++
					continue
				}
				if value[end] == '"' && !escaped {
					break
				}
				escaped = false
				end++
			}
			if end >= len(value) {
				return nil, fmt.Errorf("unterminated quoted path")
			}
			unquoted, err := strconv.Unquote(value[:end+1])
			if err != nil {
				return nil, err
			}
			fields = append(fields, unquoted)
			value = value[end+1:]
			continue
		}

		nextSpace := strings.IndexByte(value, ' ')
		if nextSpace == -1 {
			fields = append(fields, value)
			break
		}
		fields = append(fields, value[:nextSpace])
		value = value[nextSpace+1:]
	}
	return fields, nil
}

func parseDiffPathValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") {
		if unquoted, err := strconv.Unquote(trimmed); err == nil {
			return unquoted
		}
	}
	return trimmed
}
