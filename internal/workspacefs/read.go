package workspacefs

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// errIncompleteRead means the file changed while it was being read, so what we
// have is a torn snapshot of an in-progress edit.
//
// VALIDATORS_RULES requires the daemon to stay consistent while files are being
// written: report an incomplete edit rather than validate half a document. A
// torn read is never reported as valid — in either direction. Claiming a
// mid-write file is broken would be as wrong as claiming it is fine.
var errIncompleteRead = errors.New("file changed while it was being read (edit in progress)")

// frontmatterLineOffset converts a line number inside the frontmatter block to
// a line number in the file. Frontmatter content starts on file line 2, right
// after the opening `---`.
const frontmatterLineOffset = 1

// stableFile is a file read that is known not to have been torn by a concurrent
// write.
type stableFile struct {
	Data    []byte
	ModTime time.Time
}

// statFile is a seam so a test can simulate a file changing mid-read, which is
// otherwise a race no test can trigger deterministically.
var statFile = os.Stat

// readStable reads path and verifies the file did not change underneath the
// read, returning errIncompleteRead when it did.
//
// The check is stat → read → stat on size and mtime. It cannot be airtight
// without locking the writer out, which this package will not do — it is
// read-only. What it does guarantee is that an edit landing during the read is
// surfaced instead of silently validated, which is the requirement.
func readStable(path string) (stableFile, error) {
	before, err := statFile(path)
	if err != nil {
		return stableFile{}, err
	}
	if before.IsDir() {
		return stableFile{}, fmt.Errorf("%s is a directory, not a file", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return stableFile{}, err
	}

	after, err := statFile(path)
	if err != nil {
		return stableFile{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return stableFile{}, errIncompleteRead
	}

	return stableFile{Data: data, ModTime: after.ModTime().UTC()}, nil
}

// markdownDocument is a parsed workspace markdown file. Metadata is nil when
// the file carries no frontmatter block at all, which is distinct from an empty
// one. FrontmatterEndLine is the file line holding the closing `---`.
type markdownDocument struct {
	Metadata           *yaml.Node
	Body               string
	FrontmatterEndLine int
}

// errNoFrontmatterTerminator means the document opens a frontmatter block that
// is never closed.
var errNoFrontmatterTerminator = errors.New("frontmatter opens with `---` but is never closed by a line containing only `---`")

// parseMarkdown splits a workspace markdown file into its YAML frontmatter and
// body.
//
// This is deliberately its own parser rather than architectfs's: the workspace
// check reports the line a bad field sits on, so the frontmatter's offset
// within the file has to survive parsing. The recognized shape is the same —
// a leading `---` line, YAML, a closing `---` line.
func parseMarkdown(content string) (markdownDocument, error) {
	if !strings.HasPrefix(content, "---\n") {
		return markdownDocument{Body: content}, nil
	}

	rest := content[4:]
	end := strings.Index(rest, "\n---\n")
	frontmatter := ""
	body := ""
	switch {
	case end >= 0:
		frontmatter = rest[:end]
		body = rest[end+5:]
	case strings.HasSuffix(rest, "\n---"):
		// A frontmatter-only file with no trailing newline after the closer.
		frontmatter = strings.TrimSuffix(rest, "\n---")
	default:
		return markdownDocument{}, errNoFrontmatterTerminator
	}

	var parsed yaml.Node
	if err := yaml.Unmarshal([]byte(frontmatter), &parsed); err != nil {
		return markdownDocument{}, fmt.Errorf("parse frontmatter: %w", err)
	}

	doc := markdownDocument{
		Body:               strings.TrimPrefix(body, "\n"),
		FrontmatterEndLine: strings.Count(frontmatter, "\n") + 3,
	}
	if parsed.Kind == yaml.DocumentNode && len(parsed.Content) > 0 {
		doc.Metadata = parsed.Content[0]
	} else if parsed.Kind != 0 {
		doc.Metadata = &parsed
	}
	return doc, nil
}

// validUTF8 reports whether data is valid UTF-8 text.
func validUTF8(data []byte) bool {
	return utf8.Valid(data)
}

// mappingEntry is one key/value pair of a YAML mapping, carrying the key's
// source line so a diagnostic can point at it.
type mappingEntry struct {
	Key   string
	Line  int
	Value *yaml.Node
}

// mappingEntries flattens a YAML mapping node in document order. A nil or
// non-mapping node yields nothing; callers check the node kind themselves when
// that distinction matters.
func mappingEntries(node *yaml.Node, lineOffset int) []mappingEntry {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	entries := make([]mappingEntry, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		entries = append(entries, mappingEntry{
			Key:   node.Content[i].Value,
			Line:  node.Content[i].Line + lineOffset,
			Value: node.Content[i+1],
		})
	}
	return entries
}

// scalarStrings extracts the literal text of every scalar in a YAML sequence.
// ok is false when the node is not a sequence of scalars.
func scalarStrings(node *yaml.Node) (values []string, ok bool) {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil, false
	}
	values = make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, false
		}
		values = append(values, item.Value)
	}
	return values, true
}
