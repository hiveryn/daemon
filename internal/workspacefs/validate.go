package workspacefs

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

// diagnostics accumulates findings for one node, keeping insertion order so a
// report reads top-down and reruns are byte-identical for identical input.
type diagnostics struct {
	items []domain.WorkspaceDiagnostic
	path  string
}

func newDiagnostics(path string) *diagnostics {
	return &diagnostics{items: []domain.WorkspaceDiagnostic{}, path: path}
}

func (d *diagnostics) add(severity domain.DiagnosticSeverity, code string, line int, format string, args ...any) {
	d.items = append(d.items, domain.WorkspaceDiagnostic{
		Code:     code,
		Severity: severity,
		Path:     d.path,
		Line:     line,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (d *diagnostics) errorf(code string, line int, format string, args ...any) {
	d.add(domain.DiagnosticError, code, line, format, args...)
}

func (d *diagnostics) warnf(code string, line int, format string, args ...any) {
	d.add(domain.DiagnosticWarning, code, line, format, args...)
}

// hasErrors reports whether any finding blocks validity. Warnings never do.
func (d *diagnostics) hasErrors() bool {
	for _, item := range d.items {
		if item.Severity == domain.DiagnosticError {
			return true
		}
	}
	return false
}

// documentResult is what validating one markdown artifact produced.
type documentResult struct {
	Exists            bool
	ModifiedAt        *time.Time
	DocumentUpdatedAt *time.Time
	// Metadata is the parsed frontmatter, available to callers that need a
	// field the generic pass does not interpret (workflow attach/repos).
	Metadata *yaml.Node
	// FrontmatterLine is the offset to add to a frontmatter node's line to get
	// a file line.
	FrontmatterLine int
}

// validateMarkdownArtifact runs the shared markdown pipeline for one artifact:
// existence, stable read, UTF-8, body, frontmatter presence, then the field
// specs from the definition. It reports every problem it finds rather than
// stopping at the first, so one check yields a complete repair list.
func validateMarkdownArtifact(def definition, absPath string, required bool, diags *diagnostics) documentResult {
	result := documentResult{}

	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			if required {
				diags.errorf(domain.DiagMissingRequiredFile, 0, "%s is required but does not exist", diags.path)
			}
			return result
		}
		diags.errorf(domain.DiagUnreadable, 0, "cannot inspect %s: %v", diags.path, err)
		return result
	}
	result.Exists = true

	if info.IsDir() {
		diags.errorf(domain.DiagNotAFile, 0, "%s is a directory; a markdown file is expected", diags.path)
		return result
	}

	file, err := readStable(absPath)
	if err != nil {
		if errors.Is(err, errIncompleteRead) {
			// Not reported as invalid content: the file was mid-edit, so we
			// have no trustworthy view of it either way. Re-run the check once
			// the edit lands.
			diags.errorf(domain.DiagIncompleteRead, 0, "%s changed while it was being read, so it was not validated; re-run the check once the edit is finished", diags.path)
			return result
		}
		diags.errorf(domain.DiagUnreadable, 0, "cannot read %s: %v", diags.path, err)
		return result
	}
	modTime := file.ModTime
	result.ModifiedAt = &modTime

	if !validUTF8(file.Data) {
		diags.errorf(domain.DiagInvalidUTF8, 0, "%s is not valid UTF-8 text", diags.path)
		return result
	}

	doc, err := parseMarkdown(string(file.Data))
	if err != nil {
		line := 0
		if errors.Is(err, errNoFrontmatterTerminator) {
			line = 1
		}
		diags.errorf(domain.DiagInvalidFrontmatter, line, "cannot parse %s: %v", diags.path, err)
		return result
	}
	result.Metadata = doc.Metadata
	result.FrontmatterLine = frontmatterLineOffset

	if def.RequireBody && strings.TrimSpace(doc.Body) == "" {
		line := 0
		if doc.FrontmatterEndLine > 0 {
			line = doc.FrontmatterEndLine
		}
		diags.errorf(domain.DiagEmptyBody, line, "%s has no markdown body below its frontmatter", diags.path)
	}

	result.DocumentUpdatedAt = validateFrontmatter(def, doc, diags)
	return result
}

// validateFrontmatter executes a definition's frontmatterSpec against a parsed
// document, returning the document's own declared timestamp when the spec has a
// timestamp field and it parsed. This is the single generic pass — the rules an
// agent reads from describeArtifact are enforced right here.
func validateFrontmatter(def definition, doc markdownDocument, diags *diagnostics) *time.Time {
	spec := def.Frontmatter

	if doc.Metadata == nil {
		if spec.Required {
			diags.errorf(domain.DiagMissingFrontmatter, 1, "%s has no YAML frontmatter block; it must open with a `---` line", diags.path)
		}
		return nil
	}
	if doc.Metadata.Kind != yaml.MappingNode {
		diags.errorf(domain.DiagInvalidFrontmatter, doc.Metadata.Line+frontmatterLineOffset, "%s frontmatter must be a YAML mapping of fields", diags.path)
		return nil
	}

	entries := mappingEntries(doc.Metadata, frontmatterLineOffset)
	byKey := make(map[string]mappingEntry, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = entry
	}

	var documentUpdatedAt *time.Time
	for _, field := range spec.Fields {
		entry, present := byKey[field.Name]
		if !present {
			if field.Required {
				diags.errorf(domain.DiagMissingField, 1, "%s frontmatter is missing required field %q", diags.path, field.Name)
			}
			continue
		}
		if parsed := validateField(field, entry, diags); parsed != nil && field.Type == domain.ArtifactFieldTimestampUTC {
			documentUpdatedAt = parsed
		}
	}

	if spec.Exclusive {
		allowed := make(map[string]struct{}, len(spec.Fields))
		for _, field := range spec.Fields {
			allowed[field.Name] = struct{}{}
		}
		for _, entry := range entries {
			if _, ok := allowed[entry.Key]; !ok {
				diags.errorf(domain.DiagUnexpectedField, entry.Line,
					"%s frontmatter has unexpected field %q; only %s %s allowed here",
					diags.path, entry.Key, quotedFieldNames(spec.Fields), pluralIs(len(spec.Fields)))
			}
		}
	}

	return documentUpdatedAt
}

// validateField checks one frontmatter value against its spec, returning the
// parsed time for a timestamp field.
func validateField(field fieldSpec, entry mappingEntry, diags *diagnostics) *time.Time {
	switch field.Type {
	case domain.ArtifactFieldTimestampUTC:
		if entry.Value.Kind != yaml.ScalarNode {
			diags.errorf(domain.DiagInvalidTimestamp, entry.Line, "%s frontmatter field %q must be an RFC3339 UTC datetime string", diags.path, field.Name)
			return nil
		}
		parsed, err := parseRFC3339UTC(entry.Value.Value)
		if err != nil {
			diags.errorf(domain.DiagInvalidTimestamp, entry.Line, "%s frontmatter field %q: %v", diags.path, field.Name, err)
			return nil
		}
		return &parsed

	case domain.ArtifactFieldEnum:
		if entry.Value.Kind != yaml.ScalarNode || !slicesContain(field.Enum, entry.Value.Value) {
			diags.errorf(domain.DiagInvalidFrontmatter, entry.Line, "%s frontmatter field %q must be one of: %s", diags.path, field.Name, strings.Join(field.Enum, ", "))
			return nil
		}

	case domain.ArtifactFieldStringList:
		if _, ok := scalarStrings(entry.Value); !ok {
			diags.errorf(domain.DiagInvalidFrontmatter, entry.Line, "%s frontmatter field %q must be a list of strings", diags.path, field.Name)
		}

	case domain.ArtifactFieldString:
		if entry.Value.Kind != yaml.ScalarNode || strings.TrimSpace(entry.Value.Value) == "" {
			diags.errorf(domain.DiagInvalidFrontmatter, entry.Line, "%s frontmatter field %q must be a nonempty string", diags.path, field.Name)
		}
	}
	return nil
}

// parseRFC3339UTC parses an RFC3339 datetime and requires it to be expressed in
// UTC. A local-offset timestamp is rejected rather than silently converted:
// these timestamps are compared against each other and against archive
// filenames, so the offset has to be unambiguous.
func parseRFC3339UTC(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, errors.New("is empty; expected an RFC3339 UTC datetime such as \"2026-01-15T09:30:00Z\"")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC3339 datetime such as \"2026-01-15T09:30:00Z\"", value)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		return time.Time{}, fmt.Errorf("%q is not UTC; use a Z suffix, such as \"2026-01-15T09:30:00Z\"", value)
	}
	return parsed.UTC(), nil
}

// validateDirectory checks that a required workspace directory exists and is a
// directory. An empty one is valid.
func validateDirectory(absPath string, diags *diagnostics) (exists bool, modifiedAt *time.Time) {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			diags.errorf(domain.DiagMissingRequiredDirectory, 0, "%s/ is required but does not exist; create it (it may stay empty)", diags.path)
			return false, nil
		}
		diags.errorf(domain.DiagUnreadable, 0, "cannot inspect %s/: %v", diags.path, err)
		return false, nil
	}
	if !info.IsDir() {
		diags.errorf(domain.DiagNotADirectory, 0, "%s exists but is not a directory", diags.path)
		return true, nil
	}
	modTime := info.ModTime().UTC()
	return true, &modTime
}

// sortedNames returns directory entry names in a stable order so two checks of
// unchanged state produce identical reports.
func sortedNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func quotedFieldNames(fields []fieldSpec) string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, fmt.Sprintf("%q", field.Name))
	}
	return strings.Join(names, " and ")
}

func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
