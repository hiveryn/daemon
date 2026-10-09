package workspacefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// ArchitectSystemDocument is the optional ARCHITECT_SYSTEM.md as loaded for an
// architect session.
//
// Present says the file exists. Loaded says its content passed the artifact's
// own structural check and Content carries it. A file that exists but could
// not be loaded is reported through Diagnostics so the session can be told
// exactly what is wrong instead of being told its preferences were applied.
type ArchitectSystemDocument struct {
	Path        string
	Present     bool
	Loaded      bool
	Content     string
	Diagnostics []domain.WorkspaceDiagnostic
}

// ReadArchitectSystem loads the workspace's optional ARCHITECT_SYSTEM.md
// through the same definition the workspace check executes. It never fails:
// an absent file is a valid workspace, and a broken one is described rather
// than treated as loaded. Architect startup must stay possible either way so
// the file can be repaired.
func ReadArchitectSystem(workspacePath string) ArchitectSystemDocument {
	def := definitions[domain.ArtifactArchitectSystem]
	diags := newDiagnostics(ArchitectSystemFileName)
	absPath := joinWorkspace(workspacePath, ArchitectSystemFileName)

	result := validateMarkdownArtifact(def, absPath, false, diags)
	doc := ArchitectSystemDocument{
		Path:        absPath,
		Present:     result.Exists,
		Diagnostics: diags.items,
	}
	if !result.Exists || diags.hasErrors() {
		return doc
	}
	doc.Loaded = true
	doc.Content = result.Data
	return doc
}

// WorkerContext is the validated read-only context a ticket session is
// launched with: the project documents (the required overview and state, then
// the optional roadmap when present) and the explicitly selected workflows, in
// selection order.
//
// Every document and workflow carries the text read and validated by this
// check, so the daemon hands the worker the current content in the first
// message of the run, labelled by name: a worker never needs filesystem access
// to the architect workspace. The content is never stored, and every launch and
// resume validates and reads again. The worker is granted no write scope in
// the workspace.
type WorkerContext struct {
	Documents []ProjectDocument
	Workflows []SelectedWorkflow
}

// ProjectDocument is one valid project document: its file name (the label a
// worker sees) and its full content, verbatim.
type ProjectDocument struct {
	Name    string
	Content string
}

// SelectedWorkflow is one valid selected workflow: its name (the file name
// without .md, the label a worker sees), its canonical path (what a session
// records as its selection) and its markdown body below the frontmatter,
// verbatim.
type SelectedWorkflow struct {
	Name string
	Path string
	Body string
}

// WorkflowPaths returns the canonical paths of the selected workflows, in
// selection order — what a session records as its selection.
func (c WorkerContext) WorkflowPaths() []string {
	paths := make([]string, 0, len(c.Workflows))
	for _, workflow := range c.Workflows {
		paths = append(paths, workflow.Path)
	}
	return paths
}

// ValidateWorkerContext checks that a ticket session can be launched (or
// resumed) against the workspace as it is on disk right now, and returns the
// content the worker's first message carries.
//
// It enforces the worker-launch column of VALIDATORS_RULES: hiveryn.yaml must
// load; PROJECT_OVERVIEW.md and PROJECT_STATE.md must be valid; ROADMAP_CURRENT.md
// is optional but must be valid when present; and every selected workflow must be a unique canonical path to a
// readable, valid file directly inside this workspace's workflows/ directory.
// The architect-only ARCHITECT_SYSTEM.md and
// workflows that were not selected are not consulted, so their problems never
// stop a worker.
//
// A failure is a *domain.ValidationError carrying every finding, so the
// selection or workspace can be repaired in one pass. A selected file that is
// missing, renamed, invalid, a symlink out of the workspace, or not in
// canonical form is an error; it is never dropped or replaced by a guess.
func ValidateWorkerContext(workspacePath, architectKey string, selected []string) (WorkerContext, error) {
	ctx, scope, findings := validateWorkerProjectContext(workspacePath, architectKey)
	if len(findings) > 0 {
		return WorkerContext{}, &domain.ValidationError{
			Field:   "workspace",
			Message: workspaceNotReadyMessage + strings.Join(findings, "\n"),
		}
	}

	workflows, findings := validateSelectedWorkflows(workspacePath, scope, selected)
	if len(findings) > 0 {
		return WorkerContext{}, &domain.ValidationError{
			Field:   "workflows",
			Message: "the selected workflows are not launchable; fix the files or adjust the selection explicitly:\n" + strings.Join(findings, "\n"),
		}
	}
	ctx.Workflows = workflows
	return ctx, nil
}

// workspaceNotReadyMessage heads the project-context failure so the launch
// error and the preflight describe the same condition in the same words.
const workspaceNotReadyMessage = "the architect workspace is not ready for a worker session; repair it and launch again:\n"

// PreflightWorkerContext reports the project-context findings that would block
// a ticket session launch right now, without a selection and without launching
// anything. An empty result means the required documents are ready.
//
// It is the same function the launch runs, so the desktop can show real launch
// blockers before creating a session without carrying a second opinion about
// what blocks a worker.
func PreflightWorkerContext(workspacePath, architectKey string) []string {
	_, _, findings := validateWorkerProjectContext(workspacePath, architectKey)
	return findings
}

// validateWorkerProjectContext enforces the project-context half of the
// worker-launch column of VALIDATORS_RULES: hiveryn.yaml must load;
// PROJECT_OVERVIEW.md and PROJECT_STATE.md must be valid; ROADMAP_CURRENT.md is
// optional but must be valid when present. It returns the documents' content
// from the validated read and the repo scope selected workflows are checked
// against.
//
// The architect-only ARCHITECT_SYSTEM.md is never
// consulted, so their problems never stop a worker.
func validateWorkerProjectContext(workspacePath, architectKey string) (WorkerContext, repoScope, []string) {
	if strings.TrimSpace(workspacePath) == "" {
		return WorkerContext{}, repoScope{}, []string{"architect workspace path is required"}
	}
	if info, err := os.Stat(workspacePath); err != nil || !info.IsDir() {
		return WorkerContext{}, repoScope{}, []string{fmt.Sprintf("architect workspace %s is not a readable directory", workspacePath)}
	}

	var findings []string

	resolved, err := config.ValidateArchitectConfig(workspacePath, architectKey)
	scope := repoScope{}
	if err != nil {
		findings = append(findings, fmt.Sprintf("%s: %v", ConfigFileName, err))
	} else {
		scope = scopeFromRepos(resolved.Repos)
	}

	ctx := WorkerContext{Documents: []ProjectDocument{}, Workflows: []SelectedWorkflow{}}
	for _, kind := range []domain.ArtifactKind{domain.ArtifactProjectOverview, domain.ArtifactProjectState, domain.ArtifactRoadmapCurrent} {
		def := definitions[kind]
		name := rootDocumentFileName(kind)
		diags := newDiagnostics(name)
		absPath := joinWorkspace(workspacePath, name)
		result := validateMarkdownArtifact(def, absPath, def.Required, diags)
		findings = append(findings, formatDiagnostics(diags.items)...)
		if !result.Exists && !def.Required {
			// An absent optional document is simply not part of the context.
			continue
		}

		// The document must be a real file inside the workspace, not a
		// symlink to content elsewhere.
		if _, err := canonicalInsideWorkspace(workspacePath, absPath); err != nil {
			findings = append(findings, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !diags.hasErrors() {
			ctx.Documents = append(ctx.Documents, ProjectDocument{Name: name, Content: result.Data})
		}
	}
	return ctx, scope, findings
}

// validateSelectedWorkflows checks each selected path and returns the
// canonical paths and bodies in selection order, or every finding when any
// path fails.
func validateSelectedWorkflows(workspacePath string, scope repoScope, selected []string) ([]SelectedWorkflow, []string) {
	workflows := make([]SelectedWorkflow, 0, len(selected))
	if len(selected) == 0 {
		return workflows, nil
	}

	var findings []string
	workflowsDir := joinWorkspace(workspacePath, WorkflowsDirName)
	realWorkflowsDir, err := filepath.EvalSymlinks(workflowsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []string{fmt.Sprintf("%s/ does not exist, so no workflow can be selected; create the directory and the selected files, or select none", WorkflowsDirName)}
		}
		return nil, []string{fmt.Sprintf("cannot resolve %s/: %v", WorkflowsDirName, err)}
	}

	seen := make(map[string]int, len(selected))
	for i, raw := range selected {
		label := fmt.Sprintf("workflows[%d]", i)
		path := strings.TrimSpace(raw)
		if path == "" {
			findings = append(findings, label+": is blank")
			continue
		}
		if !filepath.IsAbs(path) {
			findings = append(findings, fmt.Sprintf("%s: %q is not an absolute path; select the canonical path reported by the workflow listing", label, path))
			continue
		}
		if filepath.Clean(path) != path {
			findings = append(findings, fmt.Sprintf("%s: %q is not in canonical form; select %q as reported by the workflow listing", label, path, filepath.Clean(path)))
			continue
		}
		if first, dup := seen[path]; dup {
			findings = append(findings, fmt.Sprintf("%s: %s is already selected at workflows[%d]; a workflow is selected at most once", label, path, first))
			continue
		}
		seen[path] = i

		canonical, err := canonicalInsideWorkspace(workspacePath, path)
		if err != nil {
			var outside *errOutsideWorkspace
			if errors.As(err, &outside) {
				findings = append(findings, fmt.Sprintf("%s: %s resolves to %s, outside the architect workspace; a selected workflow must be a real file inside %s/", label, path, outside.Resolved, WorkflowsDirName))
			} else {
				findings = append(findings, fmt.Sprintf("%s: cannot resolve %s: %v", label, path, err))
			}
			continue
		}
		if canonical != path {
			findings = append(findings, fmt.Sprintf("%s: %s is not the canonical path; it resolves to %s — select that path instead", label, path, canonical))
			continue
		}
		if filepath.Dir(canonical) != realWorkflowsDir {
			findings = append(findings, fmt.Sprintf("%s: %s is not directly inside this workspace's %s/ directory (%s)", label, path, WorkflowsDirName, realWorkflowsDir))
			continue
		}
		name := filepath.Base(canonical)
		if !isMarkdown(name) {
			findings = append(findings, fmt.Sprintf("%s: %s is not a markdown (*.md) workflow file", label, path))
			continue
		}
		if _, err := os.Lstat(canonical); err != nil {
			if os.IsNotExist(err) {
				findings = append(findings, fmt.Sprintf("%s: %s does not exist — it was renamed or deleted after being selected; select the current file or remove it from the selection", label, path))
			} else {
				findings = append(findings, fmt.Sprintf("%s: cannot inspect %s: %v", label, path, err))
			}
			continue
		}

		workflow, body := validateWorkflowDocument(workspacePath, realWorkflowsDir, name, scope)
		if !workflow.Valid {
			for _, line := range formatDiagnostics(workflow.Diagnostics) {
				findings = append(findings, label+": "+line)
			}
			continue
		}
		workflows = append(workflows, SelectedWorkflow{Name: workflow.Name, Path: workflow.Path, Body: body})
	}

	if len(findings) > 0 {
		return nil, findings
	}
	return workflows, nil
}

// formatDiagnostics renders error-severity diagnostics as one line each, in a
// fixed order, for an actionable error message.
func formatDiagnostics(diags []domain.WorkspaceDiagnostic) []string {
	lines := make([]string, 0, len(diags))
	for _, diag := range diags {
		if diag.Severity != domain.DiagnosticError {
			continue
		}
		where := diag.Path
		if diag.Line > 0 {
			where = fmt.Sprintf("%s:%d", diag.Path, diag.Line)
		}
		lines = append(lines, fmt.Sprintf("%s %s: %s", where, diag.Code, diag.Message))
	}
	sort.Stable(sort.StringSlice(lines))
	return lines
}

// NamedWorkflow is one workflow selected by name: the name as normalized (the
// file name without its .md extension) and the canonical path a session
// records.
type NamedWorkflow struct {
	Name string
	Path string
}

// ResolveWorkflowNames maps workflow names to the canonical workflow paths of
// this workspace, for a selection made by name rather than from the listing.
//
// A name is NAME or NAME.md and denotes the file directly inside workflows/;
// anything containing a path separator, and "." or "..", is refused, so a name
// can never reach outside the directory. Equivalent names (NAME and NAME.md,
// or a repeat) are one selection, kept at its first position. The list is the
// whole selection: nothing is added from repository suggestions, and an empty
// list selects nothing. Every unknown or malformed name is reported at once,
// together with the available names, as a *domain.ValidationError.
//
// Only names are resolved here. Whether each selected file is launchable is
// ValidateWorkerContext's verdict, which the launch runs on the returned paths.
func ResolveWorkflowNames(workspacePath, architectKey string, names []string) ([]NamedWorkflow, error) {
	selected := make([]NamedWorkflow, 0, len(names))
	if len(names) == 0 {
		return selected, nil
	}

	// A broken config only turns off the repo-key check inside each workflow's
	// validation, which is not consulted here; the launch reports it.
	scope, _ := loadRepoScope(workspacePath, architectKey)
	available, err := discoverWorkflows(workspacePath, scope, newDiagnostics(WorkflowsDirName))
	if err != nil {
		return nil, err
	}
	byName := make(map[string]domain.Workflow, len(available))
	availableNames := make([]string, 0, len(available))
	for _, workflow := range available {
		byName[workflow.Name] = workflow
		availableNames = append(availableNames, workflow.Name)
	}

	var findings []string
	seen := make(map[string]struct{}, len(names))
	for i, raw := range names {
		label := fmt.Sprintf("workflows[%d]", i)
		name := strings.TrimSpace(raw)
		if strings.EqualFold(filepath.Ext(name), markdownExt) {
			name = strings.TrimSpace(name[:len(name)-len(markdownExt)])
		}
		switch {
		case name == "":
			findings = append(findings, fmt.Sprintf("%s: %q is not a workflow name", label, raw))
			continue
		case strings.ContainsAny(name, `/\`) || name == "." || name == "..":
			findings = append(findings, fmt.Sprintf("%s: %q is a path; give the name of a file in %s/ (NAME or NAME.md)", label, raw, WorkflowsDirName))
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		workflow, ok := byName[name]
		if !ok {
			findings = append(findings, fmt.Sprintf("%s: %q is not a workflow in %s/", label, raw, WorkflowsDirName))
			continue
		}
		seen[name] = struct{}{}
		selected = append(selected, NamedWorkflow{Name: name, Path: workflow.Path})
	}

	if len(findings) > 0 {
		list := "none (" + WorkflowsDirName + "/ holds no *.md files)"
		if len(availableNames) > 0 {
			list = strings.Join(availableNames, ", ")
		}
		return nil, &domain.ValidationError{
			Field:   "workflows",
			Message: "invalid workflow selection:\n" + strings.Join(findings, "\n") + "\navailable workflows: " + list,
		}
	}
	return selected, nil
}
