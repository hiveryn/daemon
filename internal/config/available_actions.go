package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	sd "github.com/hiveryn/shared/domain"
	"gopkg.in/yaml.v3"
)

// ErrInvalidArchitectConfig marks an AddAvailableAction refused because the
// hiveryn.yaml on disk does not load under the loader's own ruleset; it has to
// be repaired first rather than rewritten around.
var ErrInvalidArchitectConfig = errors.New("hiveryn.yaml is invalid")

// ErrArchitectConfigChanged marks an AddAvailableAction abandoned because
// hiveryn.yaml changed on disk while the update was being prepared; nothing
// was written, and the caller may retry against the new content.
var ErrArchitectConfigChanged = errors.New("hiveryn.yaml changed during the update")

// AvailableActionsUpdate is AddAvailableAction's outcome: whether the file
// changed, and the resulting availableActions in file order.
type AvailableActionsUpdate struct {
	Changed          bool
	AvailableActions []string
}

// availableActionsMu serializes the daemon's own updates; edits by anything
// else are caught by the compare before the rename.
var availableActionsMu sync.Mutex

// AddAvailableAction appends name to availableActions in the architect's
// hiveryn.yaml. It is idempotent: an already listed name returns Changed
// false and writes nothing. The document is edited as a YAML node tree, so
// every other key, entry and comment is kept; only the new entry is added.
//
// The file must load under the same ruleset the loader applies
// (ErrInvalidArchitectConfig otherwise), and the result is checked against it
// before anything is written. The write goes to a temporary file in the same
// directory and is renamed over hiveryn.yaml only if the file still holds the
// bytes the update was computed from (ErrArchitectConfigChanged otherwise),
// so a concurrent edit is never silently overwritten.
//
// Whether the named Action exists in the library is deliberately not checked,
// exactly as the loader does not: discovery reports a missing definition.
func AddAvailableAction(workspacePath, architectKey, name string) (AvailableActionsUpdate, error) {
	if !sd.ValidActionName(name) {
		return AvailableActionsUpdate{}, fmt.Errorf("%q is not a valid action name: use lowercase letters, digits, '.', '_' or '-', starting with a letter or digit (at most 64 characters)", name)
	}
	availableActionsMu.Lock()
	defer availableActionsMu.Unlock()

	filePath := filepath.Join(workspacePath, architectConfigFileName)
	// Replace the file a symlinked hiveryn.yaml points at, not the link.
	if target, err := filepath.EvalSymlinks(filePath); err == nil {
		filePath = target
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return AvailableActionsUpdate{}, fmt.Errorf("stat %q: %w", filePath, err)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return AvailableActionsUpdate{}, fmt.Errorf("read %q: %w", filePath, err)
	}
	current, err := validArchitectBytes(architectKey, workspacePath, data)
	if err != nil {
		return AvailableActionsUpdate{}, fmt.Errorf("%w: %q: %w", ErrInvalidArchitectConfig, filePath, err)
	}
	if slices.Contains(current.AvailableActions, name) {
		return AvailableActionsUpdate{AvailableActions: current.AvailableActions}, nil
	}

	updated, err := appendAvailableAction(data, name)
	if err != nil {
		return AvailableActionsUpdate{}, fmt.Errorf("%w: %q: %w", ErrInvalidArchitectConfig, filePath, err)
	}
	next, err := validArchitectBytes(architectKey, workspacePath, updated)
	if err != nil {
		return AvailableActionsUpdate{}, fmt.Errorf("updated %q does not validate: %w", filePath, err)
	}
	if want := append(slices.Clone(current.AvailableActions), name); !slices.Equal(next.AvailableActions, want) {
		return AvailableActionsUpdate{}, fmt.Errorf("updated %q lists availableActions %v, want %v", filePath, next.AvailableActions, want)
	}

	if err := replaceIfUnchanged(filePath, data, updated, info.Mode().Perm()); err != nil {
		return AvailableActionsUpdate{}, err
	}
	return AvailableActionsUpdate{Changed: true, AvailableActions: next.AvailableActions}, nil
}

// validArchitectBytes decodes and validates hiveryn.yaml content exactly as the
// loader does.
func validArchitectBytes(architectKey, workspacePath string, data []byte) (ArchitectConfig, error) {
	file, err := decodeArchitectFile(data)
	if err != nil {
		return ArchitectConfig{}, err
	}
	resolved, err := architectConfigFromFile(architectKey, workspacePath, file)
	if err != nil {
		return ArchitectConfig{}, err
	}
	if err := validateArchitect(architectKey, resolved); err != nil {
		return ArchitectConfig{}, err
	}
	return resolved, nil
}

// appendAvailableAction returns data with name appended to the top-level
// availableActions sequence, creating the key at the end when it is absent or
// empty.
func appendAvailableAction(data []byte, name string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("top level is not a mapping")
	}
	root := doc.Content[0]
	entry := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}

	var list *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "availableActions" {
			list = root.Content[i+1]
			break
		}
	}
	switch {
	case list == nil:
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "availableActions"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}})
	case list.Kind == yaml.SequenceNode:
		list.Content = append(list.Content, entry)
	case list.Kind == yaml.ScalarNode && list.Tag == "!!null":
		// `availableActions:` with no value.
		*list = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}, HeadComment: list.HeadComment, LineComment: list.LineComment, FootComment: list.FootComment}
	default:
		return nil, errors.New("availableActions is not a list")
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	return buf.Bytes(), nil
}

// replaceIfUnchanged writes content to a temporary file beside path and renames
// it over path, provided path still holds original. The remaining window
// between that comparison and the rename is a few syscalls long; a concurrent
// edit landing inside it cannot be detected without file locking, which the
// editors writing hiveryn.yaml do not take.
func replaceIfUnchanged(path string, original, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %q: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %q: %w", tmpPath, err)
	}

	now, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reread %q: %w", path, err)
	}
	if !bytes.Equal(now, original) {
		return fmt.Errorf("%w: %q was edited while availableActions was being updated; nothing was written", ErrArchitectConfigChanged, path)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}
	committed = true
	return nil
}
