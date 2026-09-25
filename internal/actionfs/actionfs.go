// Package actionfs inspects the global Actions library: one Git repository per
// action directly under the actions root (HIVERYN_HOME/actions). It is
// read-only and deterministic, like workspacefs: a missing or malformed file is
// reported as a problem on the listed definition, never as a failure, so a
// broken action stays visible and repairable.
//
// The definition contract is deliberately small:
//
//	<root>/<name>/.git          the action is a Git repository
//	<root>/<name>/action.yaml   name (== <name>), description, artifacts and the
//	                            optional suggestions (manual-launch prompts);
//	                            no other key
//	<root>/<name>/KICKOFF.md    launch instructions containing {{prompt}} and
//	                            {{output_dir}}; no other {{...}} placeholder
//
// Every finding is a Diagnostic with a stable code. Errors are the definition's
// Problems and make it invalid; warnings (today only missing suggestions) are
// authoring advice that never blocks a launch. Inspect applies the same rules
// to an action repository at any path, which is what `hiverynd action
// validate` runs offline.
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
	"unicode/utf8"

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

// Severity of a Diagnostic. Only errors invalidate a definition.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic codes are stable identifiers for tooling (`hiverynd action
// validate --json`); the message is for people and may change.
const (
	CodeNameInvalid               = "ACTION_NAME_INVALID"
	CodeNotGitRepository          = "ACTION_NOT_GIT_REPOSITORY"
	CodeFileMissing               = "FILE_MISSING"
	CodeFileNotRegular            = "FILE_NOT_REGULAR"
	CodeFileTooLarge              = "FILE_TOO_LARGE"
	CodeFileUnreadable            = "FILE_UNREADABLE"
	CodeDefinitionEmpty           = "DEFINITION_EMPTY"
	CodeDefinitionInvalidYAML     = "DEFINITION_INVALID_YAML"
	CodeDefinitionNameRequired    = "DEFINITION_NAME_REQUIRED"
	CodeDefinitionNameMismatch    = "DEFINITION_NAME_MISMATCH"
	CodeDescriptionRequired       = "DEFINITION_DESCRIPTION_REQUIRED"
	CodeArtifactsRequired         = "DEFINITION_ARTIFACTS_REQUIRED"
	CodeSuggestionsMissing        = "SUGGESTIONS_MISSING"
	CodeSuggestionsTooMany        = "SUGGESTIONS_TOO_MANY"
	CodeSuggestionBlank           = "SUGGESTION_BLANK"
	CodeSuggestionTooLong         = "SUGGESTION_TOO_LONG"
	CodeSuggestionDuplicate       = "SUGGESTION_DUPLICATE"
	CodeKickoffEmpty              = "KICKOFF_EMPTY"
	CodeKickoffMissingPrompt      = "KICKOFF_MISSING_PROMPT"
	CodeKickoffMissingOutputDir   = "KICKOFF_MISSING_OUTPUT_DIR"
	CodeKickoffUnknownPlaceholder = "KICKOFF_UNKNOWN_PLACEHOLDER"
)

// Diagnostic is one finding about a definition, anchored to a file or the
// action directory.
type Diagnostic struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`
	Message  string   `json:"message"`
}

// finding is a coded message before it is anchored to a path.
type finding struct {
	code    string
	message string
}

// Definition is an inspected action plus its kickoff template and every
// diagnostic, none of which are part of the wire shape. Problems holds exactly
// the error diagnostics.
type Definition struct {
	domain.ActionDefinition
	Kickoff     string
	Diagnostics []Diagnostic
}

type manifest struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Artifacts   string   `yaml:"artifacts"`
	Suggestions []string `yaml:"suggestions"`
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

// Inspect applies the definition rules to the action repository at dir, which
// need not be inside the Actions library; the action name is dir's base name,
// as it would be once installed. dir must be an existing directory: anything
// else is returned as an error with the original filesystem context, since
// there is no definition to report on.
func Inspect(dir string) (Definition, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Definition{}, fmt.Errorf("resolve action path %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Definition{}, fmt.Errorf("action path: %w", err)
	}
	if !info.IsDir() {
		return Definition{}, fmt.Errorf("action path %s is not a directory", abs)
	}
	return inspect(abs, filepath.Base(abs)), nil
}

func inspect(dir, dirName string) Definition {
	def := Definition{ActionDefinition: domain.ActionDefinition{Name: dirName, Path: dir, Problems: []domain.ActionProblem{}}, Diagnostics: []Diagnostic{}}
	report := func(severity Severity, path, code, format string, args ...any) {
		d := Diagnostic{Code: code, Severity: severity, Path: path, Message: fmt.Sprintf(format, args...)}
		def.Diagnostics = append(def.Diagnostics, d)
		if severity == SeverityError {
			def.Problems = append(def.Problems, domain.ActionProblem{Path: d.Path, Message: d.Message})
		}
	}
	problem := func(path, code, format string, args ...any) {
		report(SeverityError, path, code, format, args...)
	}

	if !domain.ValidActionName(dirName) {
		problem(dir, CodeNameInvalid, "directory name %q is not a valid action name: use lowercase letters, digits, '.', '_' or '-', starting with a letter or digit (at most 64 characters)", dirName)
	}
	if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		problem(dir, CodeNotGitRepository, "not a Git repository: an action directory must contain .git")
	}

	manifestPath := filepath.Join(dir, DefinitionFileName)
	if data, f := readBounded(manifestPath); f != nil {
		problem(manifestPath, f.code, "%s", f.message)
	} else {
		var m manifest
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				problem(manifestPath, CodeDefinitionEmpty, "file is empty; required keys: name, description, artifacts")
			} else {
				problem(manifestPath, CodeDefinitionInvalidYAML, "invalid YAML (allowed keys: name, description, artifacts, suggestions): %v", err)
			}
		} else {
			def.Description = strings.TrimSpace(m.Description)
			def.Artifacts = strings.TrimSpace(m.Artifacts)
			switch name := strings.TrimSpace(m.Name); {
			case name == "":
				problem(manifestPath, CodeDefinitionNameRequired, "name is required and must equal the directory name %q", dirName)
			case name != dirName:
				problem(manifestPath, CodeDefinitionNameMismatch, "name %q must equal the directory name %q", name, dirName)
			}
			if def.Description == "" {
				problem(manifestPath, CodeDescriptionRequired, "description is required: say what the action does and what the caller's prompt must contain")
			}
			if def.Artifacts == "" {
				problem(manifestPath, CodeArtifactsRequired, "artifacts is required: describe the delivered artifact package")
			}
			if len(m.Suggestions) == 0 {
				state := "absent"
				if m.Suggestions != nil {
					state = "empty"
				}
				report(SeverityWarning, manifestPath, CodeSuggestionsMissing, "suggestions is %s; add 2-3 useful example prompts so the manual launch form can offer them", state)
			}
			var findings []finding
			def.Suggestions, findings = suggestions(m.Suggestions)
			for _, f := range findings {
				problem(manifestPath, f.code, "%s", f.message)
			}
		}
	}

	kickoffPath := filepath.Join(dir, KickoffFileName)
	if data, f := readBounded(kickoffPath); f != nil {
		problem(kickoffPath, f.code, "%s", f.message)
	} else {
		def.Kickoff = string(data)
		for _, f := range kickoffProblems(def.Kickoff) {
			problem(kickoffPath, f.code, "%s", f.message)
		}
	}

	def.Valid = len(def.Problems) == 0
	return def
}

// suggestions trims the optional suggested prompts and reports every entry
// that breaks the bounds: blank, too long, repeated, or too many.
func suggestions(raw []string) ([]string, []finding) {
	if len(raw) == 0 {
		return nil, nil
	}
	var problems []finding
	if len(raw) > domain.MaxActionSuggestions {
		problems = append(problems, finding{CodeSuggestionsTooMany, fmt.Sprintf("suggestions has %d entries; the limit is %d", len(raw), domain.MaxActionSuggestions)})
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]int, len(raw))
	for i, entry := range raw {
		entry = strings.TrimSpace(entry)
		switch length := utf8.RuneCountInString(entry); {
		case entry == "":
			problems = append(problems, finding{CodeSuggestionBlank, fmt.Sprintf("suggestions[%d] is blank; each suggestion is a prompt the launch form can fill in", i)})
			continue
		case length > domain.MaxActionSuggestionLength:
			problems = append(problems, finding{CodeSuggestionTooLong, fmt.Sprintf("suggestions[%d] is %d characters; the limit is %d", i, length, domain.MaxActionSuggestionLength)})
		}
		if first, ok := seen[entry]; ok {
			problems = append(problems, finding{CodeSuggestionDuplicate, fmt.Sprintf("suggestions[%d] repeats suggestions[%d]", i, first)})
			continue
		}
		seen[entry] = i
		out = append(out, entry)
	}
	return out, problems
}

func kickoffProblems(kickoff string) []finding {
	var problems []finding
	if strings.TrimSpace(kickoff) == "" {
		return []finding{{CodeKickoffEmpty, "file is empty"}}
	}
	if !strings.Contains(kickoff, PromptPlaceholder) {
		problems = append(problems, finding{CodeKickoffMissingPrompt, "missing the " + PromptPlaceholder + " placeholder, which receives the caller's prompt"})
	}
	if !strings.Contains(kickoff, OutputDirPlaceholder) {
		problems = append(problems, finding{CodeKickoffMissingOutputDir, "missing the " + OutputDirPlaceholder + " placeholder, which receives the output directory"})
	}
	for _, found := range placeholderPattern.FindAllString(kickoff, -1) {
		if found != PromptPlaceholder && found != OutputDirPlaceholder {
			problems = append(problems, finding{CodeKickoffUnknownPlaceholder, fmt.Sprintf("unknown placeholder %s; only %s and %s are supplied", found, PromptPlaceholder, OutputDirPlaceholder)})
		}
	}
	return problems
}

func readBounded(path string) ([]byte, *finding) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &finding{CodeFileMissing, "file is missing"}
		}
		return nil, &finding{CodeFileUnreadable, fmt.Sprintf("cannot read file: %v", err)}
	}
	if !info.Mode().IsRegular() {
		return nil, &finding{CodeFileNotRegular, "not a regular file"}
	}
	if info.Size() > maxDefinitionBytes {
		return nil, &finding{CodeFileTooLarge, fmt.Sprintf("file is %d bytes; the limit is %d", info.Size(), maxDefinitionBytes)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &finding{CodeFileUnreadable, fmt.Sprintf("cannot read file: %v", err)}
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
