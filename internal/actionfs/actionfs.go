// Package actionfs inspects registered Action checkouts: one Git repository per
// action, at the path actions.yaml registers on its machine. It is read-only
// and deterministic, like workspacefs: a missing or malformed file — or a
// checkout that cannot be reached — is reported as a problem on the
// definition, never as a failure, so a broken action stays visible and
// repairable.
//
// The definition contract is deliberately small:
//
//	<checkout>/.git          the action is a Git repository
//	<checkout>/action.yaml   name (== the registered name), description,
//	                         optional artifacts and required suggestions
//	                         (example invocation prompts); no other key
//	<checkout>/KICKOFF.md    launch instructions containing {{prompt}} and
//	                         {{output_dir}}; no other {{...}} placeholder
//
// The rules run on the files a checkout yields, read locally (InspectLocal)
// or over SSH in one round trip (InspectRemote); remote paths are never
// interpreted locally.
//
// Every finding is a Diagnostic with a stable code. Errors are the definition's
// Problems and make it invalid; warnings are
// authoring advice that never blocks a launch. Inspect applies the same rules
// to an action repository at any local path, which is what `hiverynd action
// validate` runs offline.
package actionfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
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
	CodeCheckoutMissing           = "ACTION_CHECKOUT_MISSING"
	CodeCheckoutUnreachable       = "ACTION_CHECKOUT_UNREACHABLE"
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
	CodeSuggestionsTotalTooLong   = "SUGGESTIONS_TOTAL_TOO_LONG"
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
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Artifacts   yaml.Node `yaml:"artifacts"`
	Suggestions []string  `yaml:"suggestions"`
}

// fileRead is one definition file as read from a checkout: its content, or
// the finding that explains why there is none.
type fileRead struct {
	data    []byte
	finding *finding
}

// checkoutFiles is everything the rules read from one checkout.
type checkoutFiles struct {
	gitDir   bool
	manifest fileRead
	kickoff  fileRead
}

// Inspect applies the definition rules to the action repository at dir, which
// need not be registered; the action name is dir's base name, as it would be
// registered by default. dir must be an existing local directory: anything
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
	return InspectLocal(abs, filepath.Base(abs)), nil
}

// InspectLocal inspects the local checkout at dir registered as name. A
// missing checkout is an invalid definition that says so.
func InspectLocal(dir, name string) Definition {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		reason := "is not a directory"
		if err != nil {
			reason = err.Error()
		}
		return unavailable(dir, name, "", CodeCheckoutMissing, "registered checkout "+reason)
	}
	files := checkoutFiles{
		manifest: readBounded(filepath.Join(dir, DefinitionFileName)),
		kickoff:  readBounded(filepath.Join(dir, KickoffFileName)),
	}
	if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
		files.gitDir = true
	}
	return inspect(dir, name, files, filepath.Join)
}

// InspectRemote inspects the checkout at dir on an SSH machine in one round
// trip. An unreachable machine or a missing checkout is an invalid definition
// carrying the cause; dir is never interpreted locally.
func InspectRemote(ctx context.Context, alias, machine, dir, name string) Definition {
	script := "d=" + remoteexec.Quote(dir) + "\n" + `if ! test -d "$d"; then printf 'nodir\n'; exit 0; fi
if test -d "$d/.git" || test -f "$d/.git"; then printf 'git 1\n'; else printf 'git 0\n'; fi
for f in ` + remoteexec.Args(DefinitionFileName, KickoffFileName) + `; do
  p="$d/$f"
  if ! test -e "$p"; then printf 'missing\n'
  elif ! test -f "$p"; then printf 'notregular\n'
  elif ! size=$(wc -c <"$p" 2>/dev/null); then printf 'unreadable\n'
  elif test "$size" -gt ` + strconv.Itoa(maxDefinitionBytes) + `; then printf 'large %s\n' "$size"
  elif ! data=$(base64 <"$p" 2>/dev/null); then printf 'unreadable\n'
  else printf 'file\n%s\n.\n' "$data"
  fi
done`
	out, err := remoteexec.Run(ctx, alias, script, nil)
	if err != nil {
		return unavailable(dir, name, machine, CodeCheckoutUnreachable, fmt.Sprintf("cannot inspect the checkout on %s: %v", machineLabel(machine), err))
	}
	files, missing, err := parseRemoteCheckout(out)
	if err != nil {
		return unavailable(dir, name, machine, CodeCheckoutUnreachable, fmt.Sprintf("cannot inspect the checkout on %s: %v", machineLabel(machine), err))
	}
	if missing {
		return unavailable(dir, name, machine, CodeCheckoutMissing, "registered checkout is not a directory on "+machineLabel(machine))
	}
	def := inspect(dir, name, files, path.Join)
	def.Machine = machine
	return def
}

func machineLabel(machine string) string {
	if machine == "" {
		return "local"
	}
	return "machine " + machine
}

func parseRemoteCheckout(out []byte) (checkoutFiles, bool, error) {
	r := bufio.NewReader(bytes.NewReader(out))
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("truncated inspection response")
		}
		return strings.TrimSuffix(s, "\n"), nil
	}
	first, err := line()
	if err != nil {
		return checkoutFiles{}, false, err
	}
	if first == "nodir" {
		return checkoutFiles{}, true, nil
	}
	files := checkoutFiles{gitDir: first == "git 1"}
	if first != "git 0" && first != "git 1" {
		return checkoutFiles{}, false, fmt.Errorf("unexpected inspection response %q", first)
	}
	for _, target := range []*fileRead{&files.manifest, &files.kickoff} {
		status, err := line()
		if err != nil {
			return checkoutFiles{}, false, err
		}
		switch {
		case status == "missing":
			target.finding = &finding{CodeFileMissing, "file is missing"}
		case status == "notregular":
			target.finding = &finding{CodeFileNotRegular, "not a regular file"}
		case status == "unreadable":
			target.finding = &finding{CodeFileUnreadable, "cannot read file"}
		case strings.HasPrefix(status, "large "):
			target.finding = &finding{CodeFileTooLarge, fmt.Sprintf("file is %s bytes; the limit is %d", strings.TrimPrefix(status, "large "), maxDefinitionBytes)}
		case status == "file":
			var encoded strings.Builder
			for {
				l, err := line()
				if err != nil {
					return checkoutFiles{}, false, err
				}
				if l == "." {
					break
				}
				encoded.WriteString(strings.TrimSpace(l))
			}
			data, err := base64.StdEncoding.DecodeString(encoded.String())
			if err != nil {
				return checkoutFiles{}, false, fmt.Errorf("decode file content: %w", err)
			}
			target.data = data
		default:
			return checkoutFiles{}, false, fmt.Errorf("unexpected inspection response %q", status)
		}
	}
	return files, false, nil
}

// unavailable is the definition of a registered checkout whose files cannot
// be read at all.
func unavailable(dir, name, machine, code, message string) Definition {
	def := Definition{ActionDefinition: domain.ActionDefinition{Name: name, Path: dir, Machine: machine}}
	def.Diagnostics = []Diagnostic{{Code: code, Severity: SeverityError, Path: dir, Message: message}}
	def.Problems = []domain.ActionProblem{{Path: dir, Message: message}}
	return def
}

func inspect(dir, dirName string, files checkoutFiles, join func(...string) string) Definition {
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
		problem(dir, CodeNameInvalid, "name %q is not a valid action name: use lowercase letters, digits, '.', '_' or '-', starting with a letter or digit (at most 64 characters)", dirName)
	}
	if !files.gitDir {
		problem(dir, CodeNotGitRepository, "not a Git repository: an action checkout must contain .git")
	}

	manifestPath := join(dir, DefinitionFileName)
	if f := files.manifest.finding; f != nil {
		problem(manifestPath, f.code, "%s", f.message)
	} else {
		var m manifest
		decoder := yaml.NewDecoder(bytes.NewReader(files.manifest.data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				problem(manifestPath, CodeDefinitionEmpty, "file is empty; required keys: name, description, suggestions")
			} else {
				problem(manifestPath, CodeDefinitionInvalidYAML, "invalid YAML (allowed keys: name, description, artifacts, suggestions): %v", err)
			}
		} else {
			def.Description = strings.TrimSpace(m.Description)
			if m.Artifacts.Kind != 0 {
				if m.Artifacts.Kind != yaml.ScalarNode || m.Artifacts.Tag != "!!str" {
					problem(manifestPath, CodeArtifactsRequired, "artifacts must be a nonempty string when declared; omit it for no files")
				} else {
					def.Artifacts = strings.TrimSpace(m.Artifacts.Value)
				}
			}
			switch name := strings.TrimSpace(m.Name); {
			case name == "":
				problem(manifestPath, CodeDefinitionNameRequired, "name is required and must equal the registered name %q", dirName)
			case name != dirName:
				problem(manifestPath, CodeDefinitionNameMismatch, "name %q must equal the registered name %q", name, dirName)
			}
			if def.Description == "" {
				problem(manifestPath, CodeDescriptionRequired, "description is required: say what the action does and what the caller's prompt must contain")
			}
			if m.Artifacts.Kind != 0 && def.Artifacts == "" {
				problem(manifestPath, CodeArtifactsRequired, "artifacts must be nonempty when declared; omit it for an Action without files")
			}
			var findings []finding
			def.Suggestions, findings = suggestions(m.Suggestions)
			for _, f := range findings {
				problem(manifestPath, f.code, "%s", f.message)
			}
		}
	}

	kickoffPath := join(dir, KickoffFileName)
	if f := files.kickoff.finding; f != nil {
		problem(kickoffPath, f.code, "%s", f.message)
	} else {
		def.Kickoff = string(files.kickoff.data)
		for _, f := range kickoffProblems(def.Kickoff, def.Artifacts != "") {
			problem(kickoffPath, f.code, "%s", f.message)
		}
	}

	def.Valid = len(def.Problems) == 0
	return def
}

// suggestions trims the required example prompts and reports every entry
// that breaks the bounds: blank, too long, repeated, or too many.
func suggestions(raw []string) ([]string, []finding) {
	if len(raw) == 0 {
		return nil, []finding{{CodeSuggestionsMissing, "suggestions is required: provide 1–3 unique example prompts"}}
	}
	var problems []finding
	if len(raw) > domain.MaxActionSuggestions {
		problems = append(problems, finding{CodeSuggestionsTooMany, fmt.Sprintf("suggestions has %d entries; the limit is %d", len(raw), domain.MaxActionSuggestions)})
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]int, len(raw))
	total := 0
	for i, entry := range raw {
		entry = strings.TrimSpace(entry)
		length := len(strings.Fields(entry))
		total += length
		switch {
		case entry == "":
			problems = append(problems, finding{CodeSuggestionBlank, fmt.Sprintf("suggestions[%d] is blank; each suggestion is a prompt the launch form can fill in", i)})
			continue
		case length > domain.MaxActionSuggestionWords:
			problems = append(problems, finding{CodeSuggestionTooLong, fmt.Sprintf("suggestions[%d] is %d words; the limit is %d", i, length, domain.MaxActionSuggestionWords)})
		}
		if first, ok := seen[entry]; ok {
			problems = append(problems, finding{CodeSuggestionDuplicate, fmt.Sprintf("suggestions[%d] repeats suggestions[%d]", i, first)})
			continue
		}
		seen[entry] = i
		out = append(out, entry)
	}
	if total > domain.MaxActionSuggestionsTotalWords {
		problems = append(problems, finding{CodeSuggestionsTotalTooLong, fmt.Sprintf("suggestions total %d words; the limit is %d", total, domain.MaxActionSuggestionsTotalWords)})
	}
	return out, problems
}

func kickoffProblems(kickoff string, delivers ...bool) []finding {
	var problems []finding
	if strings.TrimSpace(kickoff) == "" {
		return []finding{{CodeKickoffEmpty, "file is empty"}}
	}
	if !strings.Contains(kickoff, PromptPlaceholder) {
		problems = append(problems, finding{CodeKickoffMissingPrompt, "missing the " + PromptPlaceholder + " placeholder, which receives the caller's prompt"})
	}
	if (len(delivers) == 0 || delivers[0]) && !strings.Contains(kickoff, OutputDirPlaceholder) {
		problems = append(problems, finding{CodeKickoffMissingOutputDir, "missing the " + OutputDirPlaceholder + " placeholder, which receives the output directory"})
	}
	for _, found := range placeholderPattern.FindAllString(kickoff, -1) {
		if found != PromptPlaceholder && found != OutputDirPlaceholder {
			problems = append(problems, finding{CodeKickoffUnknownPlaceholder, fmt.Sprintf("unknown placeholder %s; only %s and %s are supplied", found, PromptPlaceholder, OutputDirPlaceholder)})
		}
	}
	return problems
}

func readBounded(path string) fileRead {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileRead{finding: &finding{CodeFileMissing, "file is missing"}}
		}
		return fileRead{finding: &finding{CodeFileUnreadable, fmt.Sprintf("cannot read file: %v", err)}}
	}
	if !info.Mode().IsRegular() {
		return fileRead{finding: &finding{CodeFileNotRegular, "not a regular file"}}
	}
	if info.Size() > maxDefinitionBytes {
		return fileRead{finding: &finding{CodeFileTooLarge, fmt.Sprintf("file is %d bytes; the limit is %d", info.Size(), maxDefinitionBytes)}}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileRead{finding: &finding{CodeFileUnreadable, fmt.Sprintf("cannot read file: %v", err)}}
	}
	return fileRead{data: data}
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
