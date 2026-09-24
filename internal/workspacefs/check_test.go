package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestCheckValidWorkspace(t *testing.T) {
	f := newFixture(t)
	report := f.check()

	if !report.Valid {
		t.Fatalf("expected valid workspace, diagnostics: %v", allDiagnostics(report))
	}
	for _, path := range []string{
		ConfigFileName, ProjectOverviewFileName, ProjectStateFileName,
		RoadmapCurrentFileName, ArchitectSystemFileName, WorkflowsDirName,
	} {
		n := node(t, report, path)
		if len(n.Diagnostics) != 0 {
			t.Errorf("%s has diagnostics %v", path, codes(n.Diagnostics))
		}
	}
}

// The current documents report the document's own timestamp separately
// from the filesystem mtime, because they answer different questions.
func TestCheckReportsDocumentTimeAndMtimeSeparately(t *testing.T) {
	f := newFixture(t)
	overview := node(t, f.check(), ProjectOverviewFileName)

	if overview.DocumentUpdatedAt == nil {
		t.Fatal("expected document_updated_at from lastUpdatedAt")
	}
	if got := overview.DocumentUpdatedAt.Format("2006-01-02T15:04:05Z"); got != "2026-01-15T09:30:00Z" {
		t.Errorf("document_updated_at = %s", got)
	}
	if overview.ModifiedAt == nil {
		t.Fatal("expected a filesystem mtime")
	}
	if overview.ModifiedAt.Equal(*overview.DocumentUpdatedAt) {
		t.Error("mtime and document time collapsed into one value")
	}
}

// A workspace missing everything still produces a full report. This is the
// startup case: the architect has to be able to see what to create.
func TestCheckMissingWorkspaceStaysInspectable(t *testing.T) {
	f := newFixture(t)
	for _, rel := range []string{
		ConfigFileName, ProjectOverviewFileName, ProjectStateFileName,
		RoadmapCurrentFileName, WorkflowsDirName,
	} {
		f.remove(rel)
	}

	report := f.check()
	if report.Valid {
		t.Fatal("expected an empty workspace to be invalid")
	}
	if len(report.Nodes) != 6 {
		t.Fatalf("expected all 6 expected nodes to be reported, got %d: %s", len(report.Nodes), nodePaths(report))
	}

	requireCode(t, node(t, report, ConfigFileName).Diagnostics, domain.DiagMissingRequiredFile)
	requireCode(t, node(t, report, ProjectOverviewFileName).Diagnostics, domain.DiagMissingRequiredFile)
	requireCode(t, node(t, report, ProjectStateFileName).Diagnostics, domain.DiagMissingRequiredFile)
	if diags := node(t, report, RoadmapCurrentFileName).Diagnostics; len(diags) != 0 {
		t.Fatalf("optional %s reported as a problem when absent: %v", RoadmapCurrentFileName, codes(diags))
	}
	requireCode(t, node(t, report, WorkflowsDirName).Diagnostics, domain.DiagMissingRequiredDirectory)
}

// Roadmap history lives in the workspace's Git history, not in archive files.
// A workspace without archives/roadmaps/ is valid, and one left over from the
// old layout is neither reported nor validated — it is not deleted either.
func TestCheckIgnoresLegacyRoadmapArchives(t *testing.T) {
	f := newFixture(t)
	if report := f.check(); !report.Valid {
		t.Fatalf("workspace without archives/roadmaps/ is invalid: %v", allDiagnostics(report))
	}

	f.write("archives/roadmaps/ROADMAP-2026-01-14.md", "no frontmatter")
	f.write("archives/roadmaps/not-an-archive.txt", "")
	report := f.check()
	if !report.Valid {
		t.Fatalf("legacy archive files made the workspace invalid: %v", allDiagnostics(report))
	}
	for _, n := range report.Nodes {
		if strings.HasPrefix(n.Path, "archives") {
			t.Fatalf("legacy archive directory reported as node %q", n.Path)
		}
	}
}

// ARCHITECT_SYSTEM.md is the only optional root document, and its absence must
// not be reported as a problem.
func TestCheckOptionalArchitectSystemAbsentIsValid(t *testing.T) {
	f := newFixture(t)
	n := node(t, f.check(), ArchitectSystemFileName)

	if n.Exists {
		t.Fatal("fixture should not write ARCHITECT_SYSTEM.md")
	}
	if n.Required {
		t.Error("ARCHITECT_SYSTEM.md must be optional")
	}
	if !n.Valid || len(n.Diagnostics) != 0 {
		t.Errorf("absent optional file reported as a problem: %v", codes(n.Diagnostics))
	}
}

// An unreadable custom personality file must be surfaced, never silently
// treated as loaded.
func TestCheckEmptyArchitectSystemIsReported(t *testing.T) {
	f := newFixture(t)
	f.write(ArchitectSystemFileName, "\n   \n")

	n := node(t, f.check(), ArchitectSystemFileName)
	if n.Valid {
		t.Fatal("expected an empty ARCHITECT_SYSTEM.md to be invalid")
	}
	requireCode(t, n.Diagnostics, domain.DiagEmptyBody)
}

func TestCheckMalformedDocuments(t *testing.T) {
	tests := []struct {
		name    string
		content string
		code    string
	}{
		{
			name:    "no frontmatter at all",
			content: "# Overview\n\nBody with no frontmatter.\n",
			code:    domain.DiagMissingFrontmatter,
		},
		{
			name:    "frontmatter never closed",
			content: "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\n\n# Overview\n",
			code:    domain.DiagInvalidFrontmatter,
		},
		{
			name:    "unparseable yaml",
			content: "---\nlastUpdatedAt: \"unterminated\n---\n\n# Overview\n",
			code:    domain.DiagInvalidFrontmatter,
		},
		{
			name:    "missing lastUpdatedAt",
			content: "---\nauthor: someone\n---\n\n# Overview\n",
			code:    domain.DiagMissingField,
		},
		{
			name:    "lastUpdatedAt is not RFC3339",
			content: "---\nlastUpdatedAt: \"15 January 2026\"\n---\n\n# Overview\n",
			code:    domain.DiagInvalidTimestamp,
		},
		{
			name:    "lastUpdatedAt is not UTC",
			content: "---\nlastUpdatedAt: \"2026-01-15T09:30:00+11:00\"\n---\n\n# Overview\n",
			code:    domain.DiagInvalidTimestamp,
		},
		{
			name:    "empty body",
			content: "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\n---\n\n   \n",
			code:    domain.DiagEmptyBody,
		},
		{
			name:    "frontmatter is a list, not a mapping",
			content: "---\n- one\n- two\n---\n\n# Overview\n",
			code:    domain.DiagInvalidFrontmatter,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(ProjectOverviewFileName, test.content)

			report := f.check()
			if report.Valid {
				t.Fatal("expected the workspace to be invalid")
			}
			n := node(t, report, ProjectOverviewFileName)
			if n.Valid {
				t.Error("expected the node to be invalid")
			}
			requireCode(t, n.Diagnostics, test.code)
		})
	}
}

// Other frontmatter keys are allowed on the current documents: the rule is
// "contains lastUpdatedAt", not "contains only lastUpdatedAt".
func TestCheckCurrentDocumentAllowsExtraFrontmatterKeys(t *testing.T) {
	f := newFixture(t)
	f.write(ProjectStateFileName, "---\nlastUpdatedAt: \"2026-01-15T09:30:00Z\"\nreviewer: someone\n---\n\n# State\n\nBody.\n")

	n := node(t, f.check(), ProjectStateFileName)
	if !n.Valid {
		t.Fatalf("extra frontmatter keys rejected: %v", codes(n.Diagnostics))
	}
}

// A malformed document points at the line to fix.
func TestCheckDiagnosticsCarryLineNumbers(t *testing.T) {
	f := newFixture(t)
	f.write(ProjectOverviewFileName, "---\ntitle: Example\nlastUpdatedAt: \"nope\"\n---\n\n# Overview\n")

	diag := findCode(t, node(t, f.check(), ProjectOverviewFileName).Diagnostics, domain.DiagInvalidTimestamp)
	if diag.Line != 3 {
		t.Errorf("line = %d, want 3 (the lastUpdatedAt line in the file)", diag.Line)
	}
	if diag.Path != ProjectOverviewFileName {
		t.Errorf("path = %q", diag.Path)
	}
}

func TestCheckInvalidUTF8(t *testing.T) {
	f := newFixture(t)
	f.writeBytes(ProjectStateFileName, []byte{'-', '-', '-', '\n', 0xff, 0xfe, '\n', '-', '-', '-', '\n'})

	requireCode(t, node(t, f.check(), ProjectStateFileName).Diagnostics, domain.DiagInvalidUTF8)
}

func TestCheckInvalidConfigIsReportedNotFatal(t *testing.T) {
	f := newFixture(t)
	// name is required by the loader's ruleset.
	f.write(ConfigFileName, "repos:\n  api: "+f.Repos["api"]+"\n")

	report := f.check()
	if report.Valid {
		t.Fatal("expected an invalid config to make the workspace invalid")
	}
	requireCode(t, node(t, report, ConfigFileName).Diagnostics, domain.DiagConfigInvalid)

	// The rest of the tree is still inspected, so one bad file does not hide
	// everything else.
	if !node(t, report, ProjectOverviewFileName).Valid {
		t.Error("a bad config should not invalidate unrelated documents")
	}
}

// availableActions is part of the config ruleset: listed names need not exist
// in the Actions library (that is reported by getAvailableActions), but they
// must be well-formed and unique.
func TestCheckConfigAvailableActions(t *testing.T) {
	f := newFixture(t)
	f.write(ConfigFileName, "name: Example\nrepos:\n  api: "+f.Repos["api"]+"\navailableActions:\n  - demo-evidence\n")
	if n := node(t, f.check(), ConfigFileName); !n.Valid {
		t.Fatalf("config with availableActions invalid: %+v", n.Diagnostics)
	}

	f.write(ConfigFileName, "name: Example\nrepos:\n  api: "+f.Repos["api"]+"\navailableActions:\n  - demo\n  - demo\n")
	requireCode(t, node(t, f.check(), ConfigFileName).Diagnostics, domain.DiagConfigInvalid)
}

func TestDescribeConfigExampleLoads(t *testing.T) {
	schema, err := Describe(domain.ArtifactHiverynYAML)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.write(ConfigFileName, strings.ReplaceAll(strings.ReplaceAll(schema.Example, "/Users/you/repos/example-api", f.Repos["api"]), "/Users/you/repos/example-web", f.Repos["api"]))
	if n := node(t, f.check(), ConfigFileName); !n.Valid {
		t.Fatalf("documented hiveryn.yaml example is invalid: %+v", n.Diagnostics)
	}
}

func TestCheckMissingConfigIsReported(t *testing.T) {
	f := newFixture(t)
	f.remove(ConfigFileName)

	requireCode(t, node(t, f.check(), ConfigFileName).Diagnostics, domain.DiagMissingRequiredFile)
}

// A repo directory that moved after being configured is reported, but is a
// workspace finding — it must not be something the config loader rejects, or
// the daemon could not start.
func TestCheckMissingRepoPathIsReported(t *testing.T) {
	f := newFixture(t)
	f.writeConfig("Example Project", map[string]string{
		"api":  f.Repos["api"],
		"gone": filepath.Join(t.TempDir(), "does-not-exist"),
	})

	diag := findCode(t, node(t, f.check(), ConfigFileName).Diagnostics, domain.DiagRepoPathMissing)
	if want := "repos.gone"; !strings.Contains(diag.Message, want) {
		t.Errorf("message %q does not name %s", diag.Message, want)
	}
}

func TestCheckRepoPathIsNotADirectory(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.writeConfig("Example Project", map[string]string{"api": file})

	requireCode(t, node(t, f.check(), ConfigFileName).Diagnostics, domain.DiagRepoPathNotDirectory)
}

func TestCheckTicketTotalsAndBacklogWarnings(t *testing.T) {
	f := newFixture(t)
	tickets := stubTickets{board: domain.TicketBoard{
		Backlog: []domain.TicketSummary{
			{ID: "t-1", Warnings: []domain.TicketWarning{{Code: "BROKEN_REFERENCE", Message: "references t-9, which does not exist"}}},
			{ID: "t-2"},
		},
		Progress: []domain.TicketSummary{{ID: "t-3"}},
		Done: []domain.TicketSummary{
			// A done-ticket warning must not appear: only backlog warnings do.
			{ID: "t-4", Warnings: []domain.TicketWarning{{Code: "DONE_WITHOUT_CONCLUSION", Message: "no conclusion.md"}}},
			{ID: "t-5"},
			{ID: "t-6"},
		},
	}}

	report := f.checkWith(tickets)
	want := domain.WorkspaceTicketTotals{Backlog: 2, Progress: 1, Done: 3}
	if !reflect.DeepEqual(report.Tickets.Totals, want) {
		t.Errorf("totals = %+v, want %+v", report.Tickets.Totals, want)
	}
	if len(report.Tickets.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly the one backlog warning", report.Tickets.Warnings)
	}
	if got := report.Tickets.Warnings[0]; got.TicketID != "t-1" || got.Code != "BROKEN_REFERENCE" {
		t.Errorf("warning = %+v", got)
	}
}

// Ticket warnings are diagnostic only: they never make a workspace invalid.
func TestCheckTicketWarningsDoNotInvalidateWorkspace(t *testing.T) {
	f := newFixture(t)
	tickets := stubTickets{board: domain.TicketBoard{
		Backlog: []domain.TicketSummary{
			{ID: "t-1", Warnings: []domain.TicketWarning{{Code: "BROKEN_REFERENCE", Message: "broken"}}},
		},
	}}

	if !f.checkWith(tickets).Valid {
		t.Fatal("ticket warnings must not make the workspace invalid")
	}
}

// A failed ticket scan must surface with its error text, not silently report
// zero tickets.
func TestCheckTicketScanFailureIsSurfaced(t *testing.T) {
	f := newFixture(t)
	report := f.checkWith(stubTickets{err: errors.New("ticket.md is required for ticket t-7")})

	diag := findCode(t, report.Diagnostics, domain.DiagTicketScanFailed)
	if !strings.Contains(diag.Message, "ticket.md is required for ticket t-7") {
		t.Errorf("message %q dropped the underlying error", diag.Message)
	}
	if diag.Severity != domain.DiagnosticWarning {
		t.Errorf("severity = %s; a ticket-system failure is not a workspace structural error", diag.Severity)
	}
}

// An unreadable workspace root is reported rather than returned as an error:
// the architect needs to be told which path is wrong.
func TestCheckUnreadableWorkspaceRoot(t *testing.T) {
	report, err := NewService(nil).CheckWorkspace(t.Context(), "example", filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("CheckWorkspace returned an error instead of a report: %v", err)
	}
	if report.Valid {
		t.Fatal("expected invalid")
	}
	requireCode(t, report.Diagnostics, domain.DiagUnreadable)
}

// The check is read-only: nothing it does may change the workspace.
func TestCheckDoesNotMutateWorkspace(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/REVIEW.md", "---\nattach: manual\n---\n\n# Review\n")
	before := snapshot(t, f.Workspace)

	f.check()
	f.workflows("api")

	if after := snapshot(t, f.Workspace); !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace changed:\nbefore %v\nafter  %v", before, after)
	}
}

// Two checks over unchanged files must produce identical reports.
func TestCheckIsDeterministic(t *testing.T) {
	f := newFixture(t)
	f.write(WorkflowsDirName+"/ZED.md", "---\nattach: manual\n---\n\n# Zed\n")
	f.write(WorkflowsDirName+"/ALPHA.md", "---\nattach: suggested\nrepos:\n- api\n---\n\n# Alpha\n")

	first, second := f.check(), f.check()
	// checked_at is the one field that legitimately differs between runs.
	first.CheckedAt = second.CheckedAt
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two checks of unchanged state produced different reports")
	}

	workflows := node(t, first, WorkflowsDirName)
	if got := []string{workflows.Children[0].Path, workflows.Children[1].Path}; got[0] >= got[1] {
		t.Errorf("children are not in a stable sorted order: %v", got)
	}
}

// --- helpers ---

func allDiagnostics(report domain.WorkspaceReport) []string {
	out := codes(report.Diagnostics)
	for _, n := range report.Nodes {
		out = append(out, codes(n.Diagnostics)...)
		for _, entry := range n.Children {
			out = append(out, codes(entry.Diagnostics)...)
		}
	}
	return out
}

// snapshot records every path under root with its size and mtime, so a test can
// prove nothing was written.
func snapshot(t *testing.T, root string) map[string][2]any {
	t.Helper()
	out := map[string][2]any{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = [2]any{info.Size(), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
