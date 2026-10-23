package workspacefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

const validManualWorkflow = "# Manual\n\nDo the thing.\n"

func (f *fixture) workflowPath(name string) string {
	f.t.Helper()
	path, err := filepath.EvalSymlinks(joinWorkspace(f.Workspace, WorkflowsDirName+"/"+name))
	if err != nil {
		f.t.Fatalf("resolve %s: %v", name, err)
	}
	return path
}

func mustValidationError(t *testing.T, err error, field string, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error mentioning %v, got nil", fragments)
	}
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *domain.ValidationError, got %T: %v", err, err)
	}
	if verr.Field != field {
		t.Fatalf("validation field = %q, want %q (%v)", verr.Field, field, err)
	}
	for _, fragment := range fragments {
		if !strings.Contains(verr.Message, fragment) {
			t.Fatalf("expected error to mention %q, got:\n%s", fragment, verr.Message)
		}
	}
}

func TestValidateWorkerContextEmptySelectionIsValid(t *testing.T) {
	f := newFixture(t)
	ctx, err := ValidateWorkerContext(f.Workspace, "example", nil)
	if err != nil {
		t.Fatalf("ValidateWorkerContext: %v", err)
	}
	if len(ctx.Workflows) != 0 {
		t.Fatalf("empty selection yielded workflows %v", ctx.Workflows)
	}
	// The documents carry their full content, frontmatter included, in a
	// fixed order, labelled by file name.
	want := []ProjectDocument{
		{Name: ProjectOverviewFileName, Content: validOverview},
	}
	if !reflect.DeepEqual(ctx.Documents, want) {
		t.Fatalf("documents = %+v, want %+v", ctx.Documents, want)
	}
}

// Selection order is preserved and nothing is added: the fixture has a second
// valid workflow that matches the repo scope, and it must not appear.
func TestValidateWorkerContextKeepsSelectionOrderAndAddsNothing(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/b.md", validManualWorkflow)
	f.write(WorkflowsDirName+"/a.md", validManualWorkflow)
	f.write(WorkflowsDirName+"/suggested.md", "Body.\n")

	ctx, err := ValidateWorkerContext(f.Workspace, "example", []string{f.workflowPath("b.md"), f.workflowPath("a.md")})
	if err != nil {
		t.Fatalf("ValidateWorkerContext: %v", err)
	}
	if got := ctx.WorkflowPaths(); len(got) != 2 || got[0] != f.workflowPath("b.md") || got[1] != f.workflowPath("a.md") {
		t.Fatalf("workflows = %v", got)
	}
}

// A selected workflow's body is carried verbatim from the validated read, with
// only the frontmatter removed: separators, fenced code and a `---` rule inside
// the body all survive.
func TestValidateWorkerContextCarriesSelectedWorkflowBodies(t *testing.T) {
	f := newFixture(t)
	body := "# Deliver\n\nStep one.\n\n---\n\n```yaml\nattach: manual\n---\n```\n\n  indented line\n"
	f.write(WorkflowsDirName+"/deliver.md", ""+body)

	ctx, err := ValidateWorkerContext(f.Workspace, "example", []string{f.workflowPath("deliver.md")})
	if err != nil {
		t.Fatalf("ValidateWorkerContext: %v", err)
	}
	if len(ctx.Workflows) != 1 {
		t.Fatalf("workflows = %+v", ctx.Workflows)
	}
	got := ctx.Workflows[0]
	if got.Name != "deliver" || got.Path != f.workflowPath("deliver.md") {
		t.Fatalf("name = %q, path = %q", got.Name, got.Path)
	}
	if got.Body != body {
		t.Fatalf("body = %q, want %q", got.Body, body)
	}
}

// Problems in files a worker never reads must not stop the launch.
func TestValidateWorkerContextIgnoresArchitectOnlyArtifactsAndUnselectedWorkflows(t *testing.T) {
	f := newFixture(t)
	f.write("ARCHITECT_SYSTEM.md", "")                                   // invalid, architect-only
	f.write(WorkflowsDirName+"/broken.md", "---\nattach: nope\n---\n\n") // invalid, not selected
	f.mkdir(WorkflowsDirName + "/nested")                                // structural error, not selected
	f.write(WorkflowsDirName+"/ok.md", validManualWorkflow)

	ctx, err := ValidateWorkerContext(f.Workspace, "example", []string{f.workflowPath("ok.md")})
	if err != nil {
		t.Fatalf("architect-only problems blocked a worker launch: %v", err)
	}
	if len(ctx.Workflows) != 1 {
		t.Fatalf("workflows = %v", ctx.Workflows)
	}
}

// The roadmap is optional: absent, it is left out of the worker context; when
// present, it has to be valid like the other documents.
func TestValidateWorkerContextRequiredDocumentsBlockLaunch(t *testing.T) {
	cases := map[string]struct {
		breakIt  func(f *fixture)
		fragment string
	}{
		"missing state": {func(f *fixture) { f.remove(ProjectOverviewFileName) }, "OVERVIEW.md MISSING_REQUIRED_FILE"},
		"empty overview body": {func(f *fixture) {
			f.write(ProjectOverviewFileName, "---\nupdated_at: \"2026-01-15T09:30:00Z\"\n---\n")
		}, "OVERVIEW.md"},
		"invalid config":   {func(f *fixture) { f.write(ConfigFileName, "name: X\nrepos:\n  api: \"\"\n") }, "hiveryn.yaml"},
		"leftover prompts": {func(f *fixture) { f.write(ConfigFileName, "name: X\nrepos:\n  api: /tmp\nprompts:\n  architect: {}\n") }, "prompt overrides were removed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.breakIt(f)
			_, err := ValidateWorkerContext(f.Workspace, "example", nil)
			mustValidationError(t, err, "workspace", tc.fragment)
		})
	}
}

func TestValidateWorkerContextSelectedWorkflowFailuresAreActionable(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/ok.md", validManualWorkflow)
	f.write(WorkflowsDirName+"/broken.md", "")
	f.write("notes.md", validManualWorkflow)
	ok := f.workflowPath("ok.md")
	workflowsDir := filepath.Dir(ok)

	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte(validManualWorkflow), 0o644); err != nil {
		t.Fatalf("write outside workflow: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workflowsDir, "escape.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := map[string]struct {
		selected []string
		fragment string
	}{
		"blank":            {[]string{" "}, "is blank"},
		"relative":         {[]string{"workflows/ok.md"}, "not an absolute path"},
		"not clean":        {[]string{workflowsDir + "/./ok.md"}, "not in canonical form"},
		"duplicate":        {[]string{ok, ok}, "already selected"},
		"missing":          {[]string{filepath.Join(workflowsDir, "renamed.md")}, "does not exist"},
		"invalid selected": {[]string{filepath.Join(workflowsDir, "broken.md")}, "EMPTY_BODY"},
		"outside dir":      {[]string{filepath.Join(filepath.Dir(workflowsDir), "notes.md")}, "not directly inside"},
		"symlink escape":   {[]string{filepath.Join(workflowsDir, "escape.md")}, "outside the architect workspace"},
		"not markdown":     {[]string{filepath.Join(workflowsDir, "ok.txt")}, "not a markdown"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateWorkerContext(f.Workspace, "example", tc.selected)
			mustValidationError(t, err, "workflows", tc.fragment)
		})
	}

	// One good and one bad entry: the good one is not silently kept.
	_, err := ValidateWorkerContext(f.Workspace, "example", []string{ok, filepath.Join(workflowsDir, "renamed.md")})
	mustValidationError(t, err, "workflows", "workflows[1]", "does not exist")
}

func TestValidateWorkerContextMissingWorkflowsDirOnlyMattersWhenSelecting(t *testing.T) {
	f := newFixture(t)
	f.remove(WorkflowsDirName)
	if _, err := ValidateWorkerContext(f.Workspace, "example", nil); err != nil {
		t.Fatalf("empty selection must not need workflows/: %v", err)
	}
	_, err := ValidateWorkerContext(f.Workspace, "example", []string{filepath.Join(f.Workspace, WorkflowsDirName, "x.md")})
	mustValidationError(t, err, "workflows", "workflows/ does not exist")
}

func (f *fixture) preflight() domain.WorkerPreflight {
	f.t.Helper()
	preflight, err := NewService(nil).PreflightWorker(context.Background(), "example", f.Workspace)
	if err != nil {
		f.t.Fatalf("preflight: %v", err)
	}
	return preflight
}

func TestPreflightWorkerReadyWorkspaceIsLaunchable(t *testing.T) {
	f := newFixture(t)
	preflight := f.preflight()
	if !preflight.Launchable || len(preflight.Problems) != 0 {
		t.Fatalf("valid workspace not launchable: %+v", preflight)
	}
	if preflight.ArchitectKey != "example" || preflight.CheckedAt.IsZero() {
		t.Fatalf("preflight identity/timestamp missing: %+v", preflight)
	}
}

// The optional roadmap is absent in the same run that is launchable, so its
// absence is proven not to block a worker.
func TestPreflightWorkerMissingRequiredDocumentBlocks(t *testing.T) {
	f := newFixture(t)
	f.remove(ProjectOverviewFileName)
	preflight := f.preflight()
	if preflight.Launchable {
		t.Fatalf("missing OVERVIEW.md did not block a worker: %+v", preflight)
	}
	if !strings.Contains(strings.Join(preflight.Problems, "\n"), ProjectOverviewFileName) {
		t.Fatalf("problems do not name the missing document: %+v", preflight.Problems)
	}
}

// Architect-only artifacts and unselected invalid workflows are exactly what
// separates this answer from the workspace check's aggregate verdict.
func TestPreflightWorkerIgnoresArchitectOnlyAndUnselectedWorkflows(t *testing.T) {
	f := newFixture(t)
	f.write("ARCHITECT_SYSTEM.md", "")
	f.write(WorkflowsDirName+"/broken.md", "")

	if report := f.check(); report.Valid {
		t.Fatal("fixture should be an invalid workspace for the aggregate check")
	}
	if preflight := f.preflight(); !preflight.Launchable {
		t.Fatalf("architect-only/unselected problems blocked a worker: %+v", preflight.Problems)
	}
}

// The preflight and the launch must agree; one is the other's dry run.
func TestPreflightWorkerAgreesWithLaunchValidation(t *testing.T) {
	f := newFixture(t)
	f.remove(ProjectOverviewFileName)

	preflight := f.preflight()
	_, err := ValidateWorkerContext(f.Workspace, "example", nil)
	if preflight.Launchable {
		t.Fatalf("preflight said launchable: %+v", preflight)
	}
	mustValidationError(t, err, "workspace", ProjectOverviewFileName)
	for _, problem := range preflight.Problems {
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || !strings.Contains(verr.Message, problem) {
			t.Fatalf("launch error does not carry preflight problem %q:\n%v", problem, err)
		}
	}
}
