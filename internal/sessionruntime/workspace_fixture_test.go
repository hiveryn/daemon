package sessionruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/workspacefs"
)

// testWorkspace creates a complete, valid architect workspace on disk: a
// hiveryn.yaml naming the given repos, the project documents (roadmap included),
// and empty workflows/ and archives/roadmaps/ directories. Ticket sessions are
// validated against the workspace at every launch, so any test that launches
// one needs this rather than a bare temp dir.
func testWorkspace(t *testing.T, repos map[string]string) string {
	t.Helper()
	workspace := t.TempDir()

	var b strings.Builder
	b.WriteString("name: Hiveryn\nrepos:\n")
	keys := make([]string, 0, len(repos))
	for key := range repos {
		keys = append(keys, key)
	}
	for _, key := range sortedStrings(keys) {
		b.WriteString("  " + key + ": " + repos[key] + "\n")
	}
	if len(repos) == 0 {
		b.Reset()
		b.WriteString("name: Hiveryn\nrepos: {}\n")
	}
	writeWorkspaceFile(t, workspace, workspacefs.ConfigFileName, b.String())

	for _, name := range []string{workspacefs.ProjectOverviewFileName, workspacefs.ProjectStateFileName, workspacefs.RoadmapCurrentFileName} {
		writeWorkspaceFile(t, workspace, name, "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\n---\n\n# "+name+"\n\nBody.\n")
	}
	for _, dir := range []string{workspacefs.WorkflowsDirName, workspacefs.RoadmapArchiveDir} {
		if err := os.MkdirAll(filepath.Join(workspace, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return workspace
}

// writeWorkflow writes a workflow file into the workspace and returns its path
// as the workflow listing would report it (canonical, symlinks resolved).
func writeWorkflow(t *testing.T, workspace, name, content string) string {
	t.Helper()
	writeWorkspaceFile(t, workspace, workspacefs.WorkflowsDirName+"/"+name, content)
	path, err := filepath.EvalSymlinks(filepath.Join(workspace, workspacefs.WorkflowsDirName, name))
	if err != nil {
		t.Fatalf("resolve workflow path: %v", err)
	}
	return path
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

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
