package workspacefs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

// fixture builds an architect workspace on disk. Every test starts from a
// complete, valid workspace and breaks exactly the one thing it is about, so a
// failure names a single cause.
type fixture struct {
	t         *testing.T
	Workspace string
	Repos     map[string]string
}

const (
	validOverview = `---
lastUpdatedAt: "2026-01-15T09:30:00Z"
---

# Example

Purpose and architecture.
`
	validState = `---
lastUpdatedAt: "2026-01-16T11:00:00Z"
---

# State

What is deployed today.
`
	validRoadmap = `---
lastUpdatedAt: "2026-01-17T08:15:00Z"
---

# Roadmap

Where we are going.
`
)

// newFixture creates a valid workspace with two configured repos, both
// existing directories.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{t: t, Workspace: t.TempDir(), Repos: map[string]string{}}
	for _, key := range []string{"api", "web"} {
		dir := t.TempDir()
		f.Repos[key] = dir
	}

	f.writeConfig("Example Project", f.Repos)
	f.write(ProjectOverviewFileName, validOverview)
	f.write(ProjectStateFileName, validState)
	f.write(RoadmapCurrentFileName, validRoadmap)
	f.mkdir(WorkflowsDirName)
	return f
}

func (f *fixture) writeConfig(name string, repos map[string]string) {
	f.t.Helper()
	var b strings.Builder
	b.WriteString("name: " + name + "\n")
	b.WriteString("repos:\n")
	for _, key := range sortedRepoKeys(repos) {
		b.WriteString("  " + key + ": " + repos[key] + "\n")
	}
	f.write(ConfigFileName, b.String())
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	path := joinWorkspace(f.Workspace, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", rel, err)
	}
}

func (f *fixture) writeBytes(rel string, content []byte) {
	f.t.Helper()
	path := joinWorkspace(f.Workspace, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		f.t.Fatalf("write %s: %v", rel, err)
	}
}

func (f *fixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(joinWorkspace(f.Workspace, rel), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", rel, err)
	}
}

func (f *fixture) remove(rel string) {
	f.t.Helper()
	if err := os.RemoveAll(joinWorkspace(f.Workspace, rel)); err != nil {
		f.t.Fatalf("remove %s: %v", rel, err)
	}
}

// check runs the workspace check with no ticket service; ticket-specific tests
// supply their own.
func (f *fixture) check() domain.WorkspaceReport {
	f.t.Helper()
	return f.checkWith(nil)
}

func (f *fixture) checkWith(tickets domain.TicketService) domain.WorkspaceReport {
	f.t.Helper()
	report, err := NewService(tickets).CheckWorkspace(context.Background(), "example", f.Workspace)
	if err != nil {
		f.t.Fatalf("CheckWorkspace: %v", err)
	}
	return report
}

func (f *fixture) workflows(scope ...string) domain.WorkflowList {
	f.t.Helper()
	list, err := NewService(nil).ListWorkflows(context.Background(), "example", f.Workspace, scope)
	if err != nil {
		f.t.Fatalf("ListWorkflows: %v", err)
	}
	return list
}

// --- assertions ---

// node finds a top-level report node by its path.
func node(t *testing.T, report domain.WorkspaceReport, path string) domain.WorkspaceNode {
	t.Helper()
	for _, n := range report.Nodes {
		if n.Path == path {
			return n
		}
	}
	t.Fatalf("no node for %q; nodes: %s", path, nodePaths(report))
	return domain.WorkspaceNode{}
}

func nodePaths(report domain.WorkspaceReport) string {
	paths := make([]string, 0, len(report.Nodes))
	for _, n := range report.Nodes {
		paths = append(paths, n.Path)
	}
	return strings.Join(paths, ", ")
}

// child finds a discovered entry below a directory node.
func child(t *testing.T, parent domain.WorkspaceNode, path string) domain.WorkspaceEntry {
	t.Helper()
	for _, c := range parent.Children {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("no child %q under %q", path, parent.Path)
	return domain.WorkspaceEntry{}
}

// codes lists the diagnostic codes on a node, for readable assertions.
func codes(diags []domain.WorkspaceDiagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(diags []domain.WorkspaceDiagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func findCode(t *testing.T, diags []domain.WorkspaceDiagnostic, code string) domain.WorkspaceDiagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Code == code {
			return d
		}
	}
	t.Fatalf("expected diagnostic %s, got %v", code, codes(diags))
	return domain.WorkspaceDiagnostic{}
}

func requireCode(t *testing.T, diags []domain.WorkspaceDiagnostic, code string) {
	t.Helper()
	if !hasCode(diags, code) {
		t.Fatalf("expected diagnostic %s, got %v", code, codes(diags))
	}
}

func requireNoCode(t *testing.T, diags []domain.WorkspaceDiagnostic, code string) {
	t.Helper()
	if hasCode(diags, code) {
		t.Fatalf("unexpected diagnostic %s in %v", code, codes(diags))
	}
}

// stubTickets is a TicketService that returns a fixed board, or an error.
type stubTickets struct {
	board domain.TicketBoard
	err   error
}

func (s stubTickets) ListTickets(context.Context, string) (domain.TicketBoard, error) {
	return s.board, s.err
}

func (s stubTickets) GetTicket(context.Context, string, string) (domain.Ticket, error) {
	panic("not used")
}

func (s stubTickets) CreateTicket(context.Context, string, domain.CreateTicketParams) (domain.Ticket, error) {
	panic("not used")
}

func (s stubTickets) EditTicket(context.Context, string, string, domain.EditTicketParams) (domain.Ticket, error) {
	panic("not used")
}

func (s stubTickets) UpdateTicketMetadata(context.Context, string, string, domain.UpdateTicketMetadataParams) (domain.Ticket, error) {
	panic("not used")
}

func (s stubTickets) DeleteTicket(context.Context, string, string) error { panic("not used") }

func (s stubTickets) MoveTicket(context.Context, string, string, domain.MoveTicketParams) (domain.Ticket, error) {
	panic("not used")
}

func (s stubTickets) ConcludeTicket(context.Context, string, string, domain.TicketConclusion) (domain.Ticket, error) {
	panic("not used")
}
