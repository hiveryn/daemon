// Package actionfs inspects the global Actions library: one Git repository per
// action directly under the actions root (HIVERYN_HOME/actions). It is
// read-only and deterministic, like workspacefs: a missing or malformed file is
// reported as a problem on the listed definition, never as a failure, so a
// broken action stays visible and repairable.
//
// The definition contract is deliberately small:
//
//	<root>/<name>/.git          the action is a Git repository
//	<root>/<name>/action.yaml   exactly: name (== <name>), description, artifacts
//	<root>/<name>/KICKOFF.md    launch instructions containing {{prompt}} and
//	                            {{output_dir}}; no other {{...}} placeholder
package actionfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

const (
	DefinitionFileName = "action.yaml"
	KickoffFileName    = "KICKOFF.md"

	PromptPlaceholder    = "{{prompt}}"
	OutputDirPlaceholder = "{{output_dir}}"

	// maxDefinitionBytes bounds either definition file; both are prose read
	// into memory and into a prompt.
	maxDefinitionBytes = 256 * 1024
)

var placeholderPattern = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Definition is an inspected action plus its kickoff template, which is not
// part of the wire shape.
type Definition struct {
	domain.ActionDefinition
	Kickoff string
}

type manifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Artifacts   string `yaml:"artifacts"`
}

// List inspects every directory under root. A missing root is an empty
// library. Hidden entries and plain files are ignored.
func List(root string) ([]Definition, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Definition{}, nil
		}
		return nil, fmt.Errorf("read actions root %s: %w", root, err)
	}
	out := []Definition{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, inspect(path, entry.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get inspects one action by name. An unknown name is a NotFoundError; an
// invalid definition is returned with its problems.
func Get(root, name string) (Definition, error) {
	if !domain.ValidActionName(name) {
		return Definition{}, &domain.NotFoundError{Resource: "action", ID: name}
	}
	path := filepath.Join(root, name)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return Definition{}, &domain.NotFoundError{Resource: "action", ID: name}
	}
	return inspect(path, name), nil
}

func inspect(dir, dirName string) Definition {
	def := Definition{ActionDefinition: domain.ActionDefinition{Name: dirName, Path: dir, Problems: []domain.ActionProblem{}}}
	problem := func(path, format string, args ...any) {
		def.Problems = append(def.Problems, domain.ActionProblem{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if !domain.ValidActionName(dirName) {
		problem(dir, "directory name %q is not a valid action name: use lowercase letters, digits, '.', '_' or '-', starting with a letter or digit (at most 64 characters)", dirName)
	}
	if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		problem(dir, "not a Git repository: an action directory must contain .git")
	}

	manifestPath := filepath.Join(dir, DefinitionFileName)
	if data, err := readBounded(manifestPath); err != nil {
		problem(manifestPath, "%v", err)
	} else {
		var m manifest
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				problem(manifestPath, "file is empty; required keys: name, description, artifacts")
			} else {
				problem(manifestPath, "invalid YAML (allowed keys: name, description, artifacts): %v", err)
			}
		} else {
			def.Description = strings.TrimSpace(m.Description)
			def.Artifacts = strings.TrimSpace(m.Artifacts)
			switch name := strings.TrimSpace(m.Name); {
			case name == "":
				problem(manifestPath, "name is required and must equal the directory name %q", dirName)
			case name != dirName:
				problem(manifestPath, "name %q must equal the directory name %q", name, dirName)
			}
			if def.Description == "" {
				problem(manifestPath, "description is required: say what the action does and what the caller's prompt must contain")
			}
			if def.Artifacts == "" {
				problem(manifestPath, "artifacts is required: describe the delivered artifact package")
			}
		}
	}

	kickoffPath := filepath.Join(dir, KickoffFileName)
	if data, err := readBounded(kickoffPath); err != nil {
		problem(kickoffPath, "%v", err)
	} else {
		def.Kickoff = string(data)
		for _, message := range kickoffProblems(def.Kickoff) {
			problem(kickoffPath, "%s", message)
		}
	}

	def.Valid = len(def.Problems) == 0
	return def
}

func kickoffProblems(kickoff string) []string {
	var problems []string
	if strings.TrimSpace(kickoff) == "" {
		return []string{"file is empty"}
	}
	if !strings.Contains(kickoff, PromptPlaceholder) {
		problems = append(problems, "missing the "+PromptPlaceholder+" placeholder, which receives the caller's prompt")
	}
	if !strings.Contains(kickoff, OutputDirPlaceholder) {
		problems = append(problems, "missing the "+OutputDirPlaceholder+" placeholder, which receives the output directory")
	}
	for _, found := range placeholderPattern.FindAllString(kickoff, -1) {
		if found != PromptPlaceholder && found != OutputDirPlaceholder {
			problems = append(problems, fmt.Sprintf("unknown placeholder %s; only %s and %s are supplied", found, PromptPlaceholder, OutputDirPlaceholder))
		}
	}
	return problems
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("file is missing")
		}
		return nil, fmt.Errorf("cannot read file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > maxDefinitionBytes {
		return nil, fmt.Errorf("file is %d bytes; the limit is %d", info.Size(), maxDefinitionBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}
	return data, nil
}

// RenderKickoff substitutes the caller's prompt and the output directory into
// a valid definition's KICKOFF.md. Substitution is a single pass, so text in
// the prompt that looks like a placeholder is left as written.
func RenderKickoff(def Definition, prompt, outputDir string) (string, error) {
	if !def.Valid {
		return "", fmt.Errorf("action %s definition is invalid", def.Name)
	}
	replacer := strings.NewReplacer(PromptPlaceholder, strings.TrimSpace(prompt), OutputDirPlaceholder, outputDir)
	return strings.TrimSpace(replacer.Replace(def.Kickoff)), nil
}

// InvalidError reports every problem of a definition that cannot launch.
func InvalidError(def Definition) error {
	parts := make([]string, 0, len(def.Problems))
	for _, p := range def.Problems {
		parts = append(parts, p.Path+": "+p.Message)
	}
	return &domain.ValidationError{Field: "action", Message: fmt.Sprintf("%s has an invalid definition: %s", def.Name, strings.Join(parts, "; "))}
}
