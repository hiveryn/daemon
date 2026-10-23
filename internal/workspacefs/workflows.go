package workspacefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
)

// discoverWorkflows reads workflows/ and validates every immediate *.md child
// as plain Markdown.
//
// The directory is flat by rule, so a subdirectory is reported as an error
// rather than walked: a nested workflow would never be discovered by anything
// else, and silently ignoring it is exactly the invisible failure this check
// exists to prevent. Non-markdown files are left alone — the directory is
// allowed to hold a README.
//
// dirDiags collects findings about the directory itself; each workflow carries
// its own.
func discoverWorkflows(workspace string, dirDiags *diagnostics) ([]domain.Workflow, error) {
	dir := joinWorkspace(workspace, WorkflowsDirName)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.Workflow{}, nil
		}
		return nil, fmt.Errorf("read workflows directory %q: %w", dir, err)
	}

	byName := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		byName[entry.Name()] = entry
	}

	workflows := []domain.Workflow{}
	for _, name := range sortedNames(entries) {
		entry := byName[name]
		if entry.IsDir() {
			dirDiags.errorf(domain.DiagWorkflowSubdirectory, 0,
				"%s/%s is a directory; workflows are flat — move its *.md files directly into %s/ and remove the subdirectory",
				WorkflowsDirName, name, WorkflowsDirName)
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), markdownExt) {
			continue
		}
		workflows = append(workflows, validateWorkflow(workspace, dir, name))
	}
	return workflows, nil
}

// validateWorkflow validates one workflow file and resolves its canonical path.
func validateWorkflow(workspace, dir, name string) domain.Workflow {
	workflow, _ := validateWorkflowDocument(workspace, dir, name)
	return workflow
}

// validateWorkflowDocument returns the complete plain Markdown from the same
// stable read, only when valid.
func validateWorkflowDocument(workspace, dir, name string) (domain.Workflow, string) {
	def := definitions[domain.ArtifactWorkflow]
	rel := WorkflowsDirName + "/" + name
	diags := newDiagnostics(rel)

	absPath := filepath.Join(dir, name)
	workflow := domain.Workflow{
		Name:    strings.TrimSuffix(name, filepath.Ext(name)),
		Path:    absPath,
		RelPath: rel,
	}

	// The canonical path is what a session records and the daemon later reads
	// for the worker, so a symlink out of the workspace is refused here rather
	// than followed.
	canonical, err := canonicalInsideWorkspace(workspace, absPath)
	if err != nil {
		var outside *errOutsideWorkspace
		if errors.As(err, &outside) {
			diags.errorf(domain.DiagOutsideWorkspace, 0,
				"%s resolves to %s, outside the architect workspace; a workflow must be a real file inside %s/",
				rel, outside.Resolved, WorkflowsDirName)
			workflow.Diagnostics = diags.items
			return workflow, ""
		}
		diags.errorf(domain.DiagUnreadable, 0, "cannot resolve %s: %v", rel, err)
		workflow.Diagnostics = diags.items
		return workflow, ""
	}
	workflow.Path = canonical

	result := validateMarkdownArtifact(def, absPath, true, diags)
	workflow.ModifiedAt = result.ModifiedAt

	workflow.Diagnostics = diags.items
	workflow.Valid = !diags.hasErrors()
	if !workflow.Valid {
		return workflow, ""
	}
	return workflow, result.Data
}
