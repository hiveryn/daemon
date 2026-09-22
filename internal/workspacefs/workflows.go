package workspacefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

// repoScope is the architect's configured repo keys.
//
// Known is false when hiveryn.yaml could not be read or was invalid. In that
// case repo-key checks are skipped entirely rather than reporting every key as
// unknown: the config error is the real finding, and burying it under one
// WORKFLOW_UNKNOWN_REPO per key per workflow would make the report useless.
type repoScope struct {
	Keys  map[string]struct{}
	Known bool
}

func (s repoScope) configured(key string) bool {
	_, ok := s.Keys[key]
	return ok
}

// discoverWorkflows reads workflows/ and validates every immediate *.md child
// against scope, the architect's configured repo map.
//
// The directory is flat by rule, so a subdirectory is reported as an error
// rather than walked: a nested workflow would never be discovered by anything
// else, and silently ignoring it is exactly the invisible failure this check
// exists to prevent. Non-markdown files are left alone — the directory is
// allowed to hold a README.
//
// dirDiags collects findings about the directory itself; each workflow carries
// its own.
func discoverWorkflows(workspace string, scope repoScope, dirDiags *diagnostics) ([]domain.Workflow, error) {
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
		workflows = append(workflows, validateWorkflow(workspace, dir, name, scope))
	}
	return workflows, nil
}

// validateWorkflow validates one workflow file and resolves its canonical path.
func validateWorkflow(workspace, dir, name string, scope repoScope) domain.Workflow {
	def := definitions[domain.ArtifactWorkflow]
	rel := WorkflowsDirName + "/" + name
	diags := newDiagnostics(rel)

	absPath := filepath.Join(dir, name)
	workflow := domain.Workflow{
		Name:    strings.TrimSuffix(name, filepath.Ext(name)),
		Path:    absPath,
		RelPath: rel,
		Repos:   []string{},
	}

	// The canonical path is what a session records and a worker later reads, so
	// a symlink out of the workspace is refused here rather than followed.
	canonical, err := canonicalInsideWorkspace(workspace, absPath)
	if err != nil {
		var outside *errOutsideWorkspace
		if errors.As(err, &outside) {
			diags.errorf(domain.DiagOutsideWorkspace, 0,
				"%s resolves to %s, outside the architect workspace; a workflow must be a real file inside %s/",
				rel, outside.Resolved, WorkflowsDirName)
			workflow.Diagnostics = diags.items
			return workflow
		}
		diags.errorf(domain.DiagUnreadable, 0, "cannot resolve %s: %v", rel, err)
		workflow.Diagnostics = diags.items
		return workflow
	}
	workflow.Path = canonical

	result := validateMarkdownArtifact(def, absPath, true, diags)
	workflow.ModifiedAt = result.ModifiedAt

	if result.Metadata != nil {
		attach, repos := readWorkflowApplicability(result.Metadata, scope, diags)
		workflow.Attach = attach
		workflow.Repos = repos
	}

	workflow.Diagnostics = diags.items
	workflow.Valid = !diags.hasErrors()
	return workflow
}

// readWorkflowApplicability interprets the attach/repos pair.
//
// The generic frontmatter pass has already checked that attach is one of the
// two allowed values, that repos is a string list, and that no other key is
// present (which is what rules out a dependencies field). What is left is the
// conditional relationship between the two, which no field spec can express:
// suggested requires a nonempty list of unique configured repo keys, and manual
// forbids the list entirely.
func readWorkflowApplicability(metadata *yaml.Node, scope repoScope, diags *diagnostics) (domain.WorkflowAttach, []string) {
	entries := mappingEntries(metadata, frontmatterLineOffset)
	var attachEntry, reposEntry *mappingEntry
	for i := range entries {
		switch entries[i].Key {
		case "attach":
			attachEntry = &entries[i]
		case "repos":
			reposEntry = &entries[i]
		}
	}

	if attachEntry == nil {
		diags.errorf(domain.DiagWorkflowInvalidAttach, 1,
			"workflow frontmatter is missing %q; use `attach: manual` or `attach: suggested` with a repos list", "attach")
		return "", []string{}
	}

	attach := domain.WorkflowAttach(attachEntry.Value.Value)
	if attachEntry.Value.Kind != yaml.ScalarNode || !attach.Valid() {
		// The generic pass already reported the bad enum value.
		return "", []string{}
	}

	repos := []string{}
	reposLine := 1
	if reposEntry != nil {
		reposLine = reposEntry.Line
		values, ok := scalarStrings(reposEntry.Value)
		if !ok {
			// The generic pass already reported the bad list shape.
			return attach, repos
		}
		repos = values
	}

	if attach == domain.WorkflowAttachManual {
		if len(repos) > 0 {
			diags.errorf(domain.DiagWorkflowReposNotAllowed, reposLine,
				"`attach: manual` takes no repos; a manual workflow is added by hand and is never matched against a repo scope. Remove repos, or switch to `attach: suggested`.")
		}
		return attach, []string{}
	}

	if len(repos) == 0 {
		diags.errorf(domain.DiagWorkflowMissingRepos, reposLine,
			"`attach: suggested` requires a nonempty repos list of configured repo keys; use `attach: manual` for a workflow that is not tied to any repository")
		return attach, repos
	}

	seen := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if _, duplicate := seen[repo]; duplicate {
			diags.errorf(domain.DiagWorkflowDuplicateRepo, reposLine, "repos lists %q more than once", repo)
			continue
		}
		seen[repo] = struct{}{}
		if scope.Known && !scope.configured(repo) {
			diags.errorf(domain.DiagWorkflowUnknownRepo, reposLine,
				"repos references %q, which is not a repo key in %s; add the repo to the config or correct the key", repo, ConfigFileName)
		}
	}

	return attach, repos
}

// markSuggested sets Suggested on every valid `attach: suggested` workflow
// whose repos overlap scopeRepos.
//
// Overlap is "any", not "all": a ticket that spans several repositories should
// be offered each workflow that covers one of them. An empty scope suggests
// nothing — a session with no writable repositories has nothing to match — and
// an invalid workflow is never suggested however well its repos match, because
// its frontmatter could not be trusted to say what it applies to.
func markSuggested(workflows []domain.Workflow, scopeRepos []string) {
	scope := make(map[string]struct{}, len(scopeRepos))
	for _, repo := range scopeRepos {
		if trimmed := strings.TrimSpace(repo); trimmed != "" {
			scope[trimmed] = struct{}{}
		}
	}
	if len(scope) == 0 {
		return
	}

	for i := range workflows {
		workflow := &workflows[i]
		if !workflow.Valid || workflow.Attach != domain.WorkflowAttachSuggested {
			continue
		}
		for _, repo := range workflow.Repos {
			if _, ok := scope[repo]; ok {
				workflow.Suggested = true
				break
			}
		}
	}
}

// normalizeScopeRepos trims, deduplicates and sorts a writable repo scope so
// the echoed scope and the resulting suggestions are deterministic.
func normalizeScopeRepos(repos []string) []string {
	seen := make(map[string]struct{}, len(repos))
	normalized := make([]string, 0, len(repos))
	for _, repo := range repos {
		trimmed := strings.TrimSpace(repo)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	sort.Strings(normalized)
	return normalized
}
