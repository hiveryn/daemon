package workspacefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

// workflow finds a discovered workflow by name.
func workflow(t *testing.T, list domain.WorkflowList, name string) domain.Workflow {
	t.Helper()
	for _, w := range list.Workflows {
		if w.Name == name {
			return w
		}
	}
	var names []string
	for _, w := range list.Workflows {
		names = append(names, w.Name)
	}
	t.Fatalf("no workflow %q; got %v", name, names)
	return domain.Workflow{}
}

func TestWorkflowManualIsValidWithoutRepos(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/VM_OPS.md", "---\nattach: manual\n---\n\n# VM operations\n\nThe procedure.\n")

	w := workflow(t, f.workflows(), "VM_OPS")
	if !w.Valid {
		t.Fatalf("expected valid, diagnostics: %v", codes(w.Diagnostics))
	}
	if w.Attach != domain.WorkflowAttachManual {
		t.Errorf("attach = %q", w.Attach)
	}
	if len(w.Repos) != 0 {
		t.Errorf("repos = %v, want empty", w.Repos)
	}
}

func TestWorkflowSuggestedRequiresConfiguredRepos(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/DELIVER.md", "---\nattach: suggested\nrepos:\n- api\n- web\n---\n\n# Delivery\n\nThe procedure.\n")

	w := workflow(t, f.workflows(), "DELIVER")
	if !w.Valid {
		t.Fatalf("expected valid, diagnostics: %v", codes(w.Diagnostics))
	}
	if strings.Join(w.Repos, ",") != "api,web" {
		t.Errorf("repos = %v", w.Repos)
	}
}

func TestWorkflowInvalidApplicability(t *testing.T) {
	tests := []struct {
		name    string
		content string
		code    string
	}{
		{
			name:    "attach is missing",
			content: "---\nrepos:\n- api\n---\n\n# Body\n",
			code:    domain.DiagWorkflowInvalidAttach,
		},
		{
			name:    "attach is not a known mode",
			content: "---\nattach: always\n---\n\n# Body\n",
			code:    domain.DiagInvalidFrontmatter,
		},
		{
			name:    "suggested with no repos",
			content: "---\nattach: suggested\n---\n\n# Body\n",
			code:    domain.DiagWorkflowMissingRepos,
		},
		{
			name:    "suggested with an empty repos list",
			content: "---\nattach: suggested\nrepos: []\n---\n\n# Body\n",
			code:    domain.DiagWorkflowMissingRepos,
		},
		{
			name:    "suggested referencing an unconfigured repo",
			content: "---\nattach: suggested\nrepos:\n- api\n- ghost\n---\n\n# Body\n",
			code:    domain.DiagWorkflowUnknownRepo,
		},
		{
			name:    "suggested with a duplicated repo",
			content: "---\nattach: suggested\nrepos:\n- api\n- api\n---\n\n# Body\n",
			code:    domain.DiagWorkflowDuplicateRepo,
		},
		{
			name:    "manual with repos",
			content: "---\nattach: manual\nrepos:\n- api\n---\n\n# Body\n",
			code:    domain.DiagWorkflowReposNotAllowed,
		},
		{
			// "No dependencies" is enforced structurally: the frontmatter is
			// exclusive, so any extra key is rejected.
			name:    "declares dependencies",
			content: "---\nattach: manual\ndependencies:\n- OTHER\n---\n\n# Body\n",
			code:    domain.DiagUnexpectedField,
		},
		{
			name:    "no frontmatter",
			content: "# Body only\n",
			code:    domain.DiagMissingFrontmatter,
		},
		{
			name:    "empty body",
			content: "---\nattach: manual\n---\n",
			code:    domain.DiagEmptyBody,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(WorkflowsDirName+"/CASE.md", test.content)

			w := workflow(t, f.workflows("api", "web"), "CASE")
			if w.Valid {
				t.Fatal("expected the workflow to be invalid")
			}
			requireCode(t, w.Diagnostics, test.code)
			if w.Suggested {
				t.Error("an invalid workflow must never be suggested")
			}
		})
	}
}

// An invalid workflow is reported, not dropped: silently omitting it would
// leave the user with no way to notice the problem.
func TestWorkflowInvalidIsReportedNotDropped(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/BROKEN.md", "---\nattach: suggested\nrepos:\n- ghost\n---\n\n# Broken\n")
	f.write(WorkflowsDirName+"/GOOD.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# Good\n")

	list := f.workflows("api")
	if len(list.Workflows) != 2 {
		t.Fatalf("expected both workflows to be listed, got %d", len(list.Workflows))
	}
	if workflow(t, list, "BROKEN").Valid {
		t.Error("BROKEN should be invalid")
	}
	if !workflow(t, list, "GOOD").Suggested {
		t.Error("GOOD should be suggested for api")
	}
}

// An invalid workflow is excluded from suggestions even when its repos match:
// its frontmatter could not be trusted to say what it applies to.
func TestWorkflowInvalidIsExcludedFromSuggestions(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/BROKEN.md", "---\nattach: suggested\nrepos:\n- api\n- api\n---\n\n# Broken\n")

	if workflow(t, f.workflows("api"), "BROKEN").Suggested {
		t.Fatal("a workflow with an invalid repos list must not be suggested")
	}
}

// Suggestions match any repo in the writable scope, not all of them.
func TestWorkflowSuggestionsMatchAnyScopeRepo(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/API_ONLY.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# API\n")
	f.write(WorkflowsDirName+"/WEB_ONLY.md", "---\nattach: suggested\nrepos:\n- web\n---\n\n# Web\n")
	f.write(WorkflowsDirName+"/MANUAL.md", "---\nattach: manual\n---\n\n# Manual\n")

	list := f.workflows("api")
	if !workflow(t, list, "API_ONLY").Suggested {
		t.Error("API_ONLY should be suggested for the api scope")
	}
	if workflow(t, list, "WEB_ONLY").Suggested {
		t.Error("WEB_ONLY should not be suggested for the api scope")
	}
	if workflow(t, list, "MANUAL").Suggested {
		t.Error("a manual workflow is never suggested by a repo match")
	}

	both := f.workflows("api", "web")
	if !workflow(t, both, "API_ONLY").Suggested || !workflow(t, both, "WEB_ONLY").Suggested {
		t.Error("a multi-repo scope should suggest each workflow covering one of its repos")
	}
}

// An empty scope suggests nothing, rather than suggesting everything.
func TestWorkflowEmptyScopeSuggestsNothing(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/API.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# API\n")

	list := f.workflows()
	if len(list.ScopeRepos) != 0 {
		t.Errorf("scope = %v, want empty", list.ScopeRepos)
	}
	if workflow(t, list, "API").Suggested {
		t.Fatal("an empty writable scope must suggest nothing")
	}
}

// The scope is normalized so repeated or differently ordered inputs produce the
// same suggestions and the same echoed scope.
func TestWorkflowScopeIsNormalized(t *testing.T) {
	f := newFixture(t)
	list := f.workflows("  web ", "api", "web", "")

	if strings.Join(list.ScopeRepos, ",") != "api,web" {
		t.Fatalf("scope = %v", list.ScopeRepos)
	}
}

func TestWorkflowExposesCanonicalPath(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/CANON.md", "---\nattach: manual\n---\n\n# Canon\n")

	w := workflow(t, f.workflows(), "CANON")
	if !filepath.IsAbs(w.Path) {
		t.Errorf("path %q is not absolute", w.Path)
	}
	if w.RelPath != WorkflowsDirName+"/CANON.md" {
		t.Errorf("rel_path = %q", w.RelPath)
	}
	resolved, err := filepath.EvalSymlinks(joinWorkspace(f.Workspace, WorkflowsDirName+"/CANON.md"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if w.Path != resolved {
		t.Errorf("path = %q, want the canonical %q", w.Path, resolved)
	}
}

// A symlink leading out of the workspace is refused, not followed: it would
// silently widen what a worker session loads.
func TestWorkflowSymlinkOutsideWorkspaceIsRefused(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "ELSEWHERE.md")
	if err := os.WriteFile(outside, []byte("---\nattach: manual\n---\n\n# Elsewhere\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := joinWorkspace(f.Workspace, WorkflowsDirName+"/ESCAPE.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	w := workflow(t, f.workflows("api"), "ESCAPE")
	if w.Valid {
		t.Fatal("a workflow symlinked outside the workspace must be invalid")
	}
	requireCode(t, w.Diagnostics, domain.DiagOutsideWorkspace)
}

// A symlink that stays inside the workspace is fine — the boundary rule is
// about escaping it, not about symlinks as such.
func TestWorkflowSymlinkInsideWorkspaceIsAllowed(t *testing.T) {
	f := newFixture(t)
	f.write("shared/COMMON.md", "---\nattach: manual\n---\n\n# Common\n")
	link := joinWorkspace(f.Workspace, WorkflowsDirName+"/COMMON.md")
	if err := os.Symlink(joinWorkspace(f.Workspace, "shared/COMMON.md"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	w := workflow(t, f.workflows(), "COMMON")
	if !w.Valid {
		t.Fatalf("expected valid, diagnostics: %v", codes(w.Diagnostics))
	}
}

// Workflows are flat: a subdirectory is an error, never silently walked.
func TestWorkflowSubdirectoryIsReported(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/delivery/NESTED.md", "---\nattach: manual\n---\n\n# Nested\n")

	list := f.workflows()
	requireCode(t, list.Diagnostics, domain.DiagWorkflowSubdirectory)
	for _, w := range list.Workflows {
		if w.Name == "NESTED" {
			t.Fatal("a nested workflow must not be discovered")
		}
	}

	if node(t, f.check(), WorkflowsDirName).Valid {
		t.Error("a workflows subdirectory must make the workflows node invalid")
	}
}

// A non-markdown file in workflows/ is left alone; the directory may hold a
// README.
func TestWorkflowIgnoresNonMarkdownFiles(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/notes.txt", "not a workflow")

	if got := len(f.workflows().Workflows); got != 0 {
		t.Fatalf("discovered %d workflows, want 0", got)
	}
}

// An empty workflows/ directory is valid.
func TestWorkflowEmptyDirectoryIsValid(t *testing.T) {
	f := newFixture(t)

	list := f.workflows("api")
	if len(list.Workflows) != 0 {
		t.Fatalf("workflows = %d, want 0", len(list.Workflows))
	}
	if len(list.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none", codes(list.Diagnostics))
	}
	if !node(t, f.check(), WorkflowsDirName).Valid {
		t.Error("an empty workflows/ directory must be valid")
	}
}

// When the config cannot be read, repo keys cannot be checked. That gap is
// reported rather than turned into one bogus unknown-repo error per key.
func TestWorkflowUnknownRepoCheckSkippedWhenConfigIsUnreadable(t *testing.T) {
	f := newFixture(t)
	f.remove(ConfigFileName)
	f.write(WorkflowsDirName+"/DELIVER.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# Delivery\n")

	list := f.workflows("api")
	requireCode(t, list.Diagnostics, domain.DiagConfigInvalid)
	requireNoCode(t, workflow(t, list, "DELIVER").Diagnostics, domain.DiagWorkflowUnknownRepo)
}

// Discovery is the same whether it is reached through the workflow list or
// through the workspace check.
func TestWorkflowNodesAppearInWorkspaceCheck(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/ONE.md", "---\nattach: manual\n---\n\n# One\n")
	f.write(WorkflowsDirName+"/TWO.md", "---\nattach: suggested\nrepos:\n- ghost\n---\n\n# Two\n")

	workflows := node(t, f.check(), WorkflowsDirName)
	if workflows.Type != domain.WorkspaceNodeDirectory {
		t.Errorf("type = %q", workflows.Type)
	}
	if len(workflows.Children) != 2 {
		t.Fatalf("children = %d, want 2", len(workflows.Children))
	}
	if !child(t, workflows, WorkflowsDirName+"/ONE.md").Valid {
		t.Error("ONE.md should be valid")
	}
	two := child(t, workflows, WorkflowsDirName+"/TWO.md")
	if two.Valid {
		t.Error("TWO.md should be invalid")
	}
	requireCode(t, two.Diagnostics, domain.DiagWorkflowUnknownRepo)
}

// A workflow whose repos are invalid makes the whole workspace invalid.
func TestWorkflowErrorInvalidatesWorkspace(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/BAD.md", "---\nattach: suggested\nrepos:\n- ghost\n---\n\n# Bad\n")

	if f.check().Valid {
		t.Fatal("an invalid workflow must make the workspace invalid")
	}
}
