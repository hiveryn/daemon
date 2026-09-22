package api

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

// newWorkspaceTestHandler wires a handler over one architect whose workspace is
// a complete, valid file-based workspace.
func newWorkspaceTestHandler(t *testing.T) (http.Handler, string) {
	t.Helper()

	configDir := t.TempDir()
	workspace := t.TempDir()
	apiRepo := writeGitRepo(t)

	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {Name: "Hiveryn", Path: workspace, Repos: map[string]string{"api": apiRepo}},
	})

	writeWorkspaceFile(t, workspace, workspacefs.ProjectOverviewFileName, "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\n---\n\n# Overview\n")
	writeWorkspaceFile(t, workspace, workspacefs.ProjectStateFileName, "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\n---\n\n# State\n")
	writeWorkspaceFile(t, workspace, workspacefs.RoadmapCurrentFileName, "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\n---\n\n# Roadmap\n")
	mkdirWorkspace(t, workspace, workspacefs.WorkflowsDirName)
	mkdirWorkspace(t, workspace, workspacefs.RoadmapArchiveDir)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}

	handler := NewHandler(Dependencies{
		Config:       cfg,
		ConfigSource: source,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Workspaces:   workspacefs.NewService(nil),
	})
	return handler, workspace
}

func writeWorkspaceFile(t *testing.T, workspace, rel, content string) {
	t.Helper()
	path := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func mkdirWorkspace(t *testing.T, workspace, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
}

func TestWorkspaceCheckReturnsReport(t *testing.T) {
	handler, workspace := newWorkspaceTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workspace/check", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}

	var report domain.WorkspaceReport
	decodeEnvelopeData(t, body, &report)
	if !report.Valid {
		t.Errorf("expected a valid workspace, diagnostics: %+v", report.Diagnostics)
	}
	if report.ArchitectKey != "hiveryn" || report.WorkspacePath != workspace {
		t.Errorf("identity = %q %q", report.ArchitectKey, report.WorkspacePath)
	}
	if len(report.Nodes) != 7 {
		t.Errorf("nodes = %d, want the 7 expected entries", len(report.Nodes))
	}
	if report.CheckedAt.IsZero() {
		t.Error("checked_at is unset")
	}
}

// A broken workspace is a successful request whose payload describes the
// breakage — not an HTTP error.
func TestWorkspaceCheckBrokenWorkspaceStillReturns200(t *testing.T) {
	handler, workspace := newWorkspaceTestHandler(t)
	if err := os.Remove(filepath.Join(workspace, workspacefs.ProjectStateFileName)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workspace/check", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}

	var report domain.WorkspaceReport
	decodeEnvelopeData(t, body, &report)
	if report.Valid {
		t.Fatal("expected invalid")
	}
}

func TestWorkspaceCheckUnknownArchitect(t *testing.T) {
	handler, _ := newWorkspaceTestHandler(t)

	status, _ := request(t, handler, http.MethodGet, "/api/architects/nope/workspace/check", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestWorkspaceDescribeArtifact(t *testing.T) {
	handler, _ := newWorkspaceTestHandler(t)

	for _, kind := range domain.ArtifactKinds() {
		t.Run(string(kind), func(t *testing.T) {
			status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workspace/artifacts/"+string(kind), nil)
			if status != http.StatusOK {
				t.Fatalf("status = %d, body: %s", status, body)
			}
			var schema domain.ArtifactSchema
			decodeEnvelopeData(t, body, &schema)
			if schema.Kind != kind {
				t.Errorf("kind = %s", schema.Kind)
			}
			if schema.Example == "" || len(schema.Rules) == 0 {
				t.Errorf("schema is missing its example or rules: %+v", schema)
			}
		})
	}
}

// Tickets and conclusions are not artifact kinds and must be rejected.
func TestWorkspaceDescribeArtifactRejectsUnknownKind(t *testing.T) {
	handler, _ := newWorkspaceTestHandler(t)

	for _, kind := range []string{"TICKET", "CONCLUSION", "nope"} {
		status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workspace/artifacts/"+kind, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", kind, status)
		}
		if !strings.Contains(string(body), "PROJECT_OVERVIEW") {
			t.Errorf("%s: error does not list the valid kinds: %s", kind, body)
		}
	}
}

func TestWorkspaceListWorkflows(t *testing.T) {
	handler, workspace := newWorkspaceTestHandler(t)
	writeWorkspaceFile(t, workspace, workspacefs.WorkflowsDirName+"/DELIVER.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# Deliver\n")
	writeWorkspaceFile(t, workspace, workspacefs.WorkflowsDirName+"/MANUAL.md", "---\nattach: manual\n---\n\n# Manual\n")

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workflows?repos=api", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}

	var list domain.WorkflowList
	decodeEnvelopeData(t, body, &list)
	if len(list.Workflows) != 2 {
		t.Fatalf("workflows = %d, want 2", len(list.Workflows))
	}
	if strings.Join(list.ScopeRepos, ",") != "api" {
		t.Errorf("scope_repos = %v", list.ScopeRepos)
	}

	byName := map[string]domain.Workflow{}
	for _, w := range list.Workflows {
		byName[w.Name] = w
	}
	if !byName["DELIVER"].Suggested {
		t.Error("DELIVER should be suggested for the api scope")
	}
	if byName["MANUAL"].Suggested {
		t.Error("a manual workflow is never suggested")
	}
	if !filepath.IsAbs(byName["DELIVER"].Path) {
		t.Errorf("path %q is not canonical/absolute", byName["DELIVER"].Path)
	}
}

func TestWorkspaceListWorkflowsWithoutScope(t *testing.T) {
	handler, workspace := newWorkspaceTestHandler(t)
	writeWorkspaceFile(t, workspace, workspacefs.WorkflowsDirName+"/DELIVER.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# Deliver\n")

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workflows", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}

	var list domain.WorkflowList
	decodeEnvelopeData(t, body, &list)
	if len(list.ScopeRepos) != 0 {
		t.Errorf("scope_repos = %v, want empty", list.ScopeRepos)
	}
	if list.Workflows[0].Suggested {
		t.Error("no scope means no suggestions")
	}
}

// An invalid workflow is listed with its diagnostics rather than dropped.
func TestWorkspaceListWorkflowsReportsInvalid(t *testing.T) {
	handler, workspace := newWorkspaceTestHandler(t)
	writeWorkspaceFile(t, workspace, workspacefs.WorkflowsDirName+"/BROKEN.md", "---\nattach: suggested\nrepos:\n- ghost\n---\n\n# Broken\n")

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/workflows?repos=api", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}

	var list domain.WorkflowList
	decodeEnvelopeData(t, body, &list)
	if len(list.Workflows) != 1 {
		t.Fatalf("workflows = %d, want the invalid one to still be listed", len(list.Workflows))
	}
	if list.Workflows[0].Valid || len(list.Workflows[0].Diagnostics) == 0 {
		t.Errorf("workflow = %+v, want invalid with diagnostics", list.Workflows[0])
	}
}

func TestParseRepoScope(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{raw: "", want: nil},
		{raw: "   ", want: nil},
		{raw: "api", want: []string{"api"}},
		{raw: "api,web", want: []string{"api", "web"}},
		{raw: " api , web ", want: []string{"api", "web"}},
		{raw: "api,,web,", want: []string{"api", "web"}},
	}

	for _, test := range tests {
		got := parseRepoScope(test.raw)
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Errorf("parseRepoScope(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}
