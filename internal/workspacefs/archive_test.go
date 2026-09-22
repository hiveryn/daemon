package workspacefs

import (
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestArchiveValidFile(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md", "---\narchivedAt: \"2026-01-14T09:20:59Z\"\n---\n\n# Archived roadmap\n\nPreserved history.\n")

	archives := node(t, f.check(), RoadmapArchiveDir)
	entry := child(t, archives, RoadmapArchiveDir+"/ROADMAP-2026-01-14.md")
	if !entry.Valid {
		t.Fatalf("expected valid, diagnostics: %v", codes(entry.Diagnostics))
	}
	if entry.DocumentUpdatedAt == nil || entry.DocumentUpdatedAt.Format(time.RFC3339) != "2026-01-14T09:20:59Z" {
		t.Errorf("document_updated_at = %v, want archivedAt", entry.DocumentUpdatedAt)
	}
}

func TestArchiveSameDayCollisionNames(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md", "---\narchivedAt: \"2026-01-14T08:00:00Z\"\n---\n\n# First\n")
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14-02.md", "---\narchivedAt: \"2026-01-14T17:00:00Z\"\n---\n\n# Second\n")
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14-03.md", "---\narchivedAt: \"2026-01-14T21:00:00Z\"\n---\n\n# Third\n")

	archives := node(t, f.check(), RoadmapArchiveDir)
	if len(archives.Children) != 3 {
		t.Fatalf("children = %d, want 3", len(archives.Children))
	}
	for _, entry := range archives.Children {
		if !entry.Valid {
			t.Errorf("%s invalid: %v", entry.Path, codes(entry.Diagnostics))
		}
	}
}

func TestArchiveInvalidNames(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "no ROADMAP prefix", file: "2026-01-14.md"},
		{name: "lowercase prefix", file: "roadmap-2026-01-14.md"},
		{name: "underscores instead of dashes", file: "ROADMAP_2026_01_14.md"},
		{name: "two-digit year", file: "ROADMAP-26-01-14.md"},
		{name: "unpadded month", file: "ROADMAP-2026-1-14.md"},
		{name: "impossible calendar date", file: "ROADMAP-2026-13-40.md"},
		{name: "unpadded sequence suffix", file: "ROADMAP-2026-01-14-2.md"},
		{name: "sequence suffix starting at 01", file: "ROADMAP-2026-01-14-01.md"},
		{name: "trailing text", file: "ROADMAP-2026-01-14-final.md"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(RoadmapArchiveDir+"/"+test.file, "---\narchivedAt: \"2026-01-14T09:00:00Z\"\n---\n\n# Body\n")

			report := f.check()
			if report.Valid {
				t.Fatal("expected a malformed archive name to make the workspace invalid")
			}
			entry := child(t, node(t, report, RoadmapArchiveDir), RoadmapArchiveDir+"/"+test.file)
			requireCode(t, entry.Diagnostics, domain.DiagArchiveNameInvalid)
		})
	}
}

func TestArchiveFrontmatterIsExclusive(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md",
		"---\narchivedAt: \"2026-01-14T09:00:00Z\"\nlastUpdatedAt: \"2026-01-14T09:00:00Z\"\n---\n\n# Body\n")

	entry := child(t, node(t, f.check(), RoadmapArchiveDir), RoadmapArchiveDir+"/ROADMAP-2026-01-14.md")
	diag := findCode(t, entry.Diagnostics, domain.DiagUnexpectedField)
	if !strings.Contains(diag.Message, "lastUpdatedAt") {
		t.Errorf("message %q does not name the offending field", diag.Message)
	}
	if diag.Line != 3 {
		t.Errorf("line = %d, want 3", diag.Line)
	}
}

func TestArchiveMissingArchivedAt(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md", "---\ntitle: Old roadmap\n---\n\n# Body\n")

	entry := child(t, node(t, f.check(), RoadmapArchiveDir), RoadmapArchiveDir+"/ROADMAP-2026-01-14.md")
	requireCode(t, entry.Diagnostics, domain.DiagMissingField)
	requireCode(t, entry.Diagnostics, domain.DiagUnexpectedField)
}

func TestArchiveDateMustMatchFilename(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md", "---\narchivedAt: \"2026-02-20T09:00:00Z\"\n---\n\n# Body\n")

	entry := child(t, node(t, f.check(), RoadmapArchiveDir), RoadmapArchiveDir+"/ROADMAP-2026-01-14.md")
	diag := findCode(t, entry.Diagnostics, domain.DiagArchiveDateMismatch)
	if !strings.Contains(diag.Message, "2026-02-20") || !strings.Contains(diag.Message, "2026-01-14") {
		t.Errorf("message %q does not name both dates", diag.Message)
	}
}

// A collision-suffixed name still cross-checks against its date part.
func TestArchiveDateMatchesSuffixedFilename(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14-02.md", "---\narchivedAt: \"2026-01-14T23:59:59Z\"\n---\n\n# Body\n")

	entry := child(t, node(t, f.check(), RoadmapArchiveDir), RoadmapArchiveDir+"/ROADMAP-2026-01-14-02.md")
	if !entry.Valid {
		t.Fatalf("expected valid, diagnostics: %v", codes(entry.Diagnostics))
	}
}

// An archived body is history: it is checked structurally and never reconciled
// against the current roadmap or the current repo map.
func TestArchiveBodyIsNotReconciledWithCurrentState(t *testing.T) {
	f := newFixture(t)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md",
		"---\narchivedAt: \"2026-01-14T09:00:00Z\"\n---\n\n# Old roadmap\n\nStatus: active. Covers the `retired-repo` repository, removed since.\n")

	entry := child(t, node(t, f.check(), RoadmapArchiveDir), RoadmapArchiveDir+"/ROADMAP-2026-01-14.md")
	if !entry.Valid {
		t.Fatalf("historical prose must not be validated: %v", codes(entry.Diagnostics))
	}
}

func TestArchiveEmptyDirectoryIsValid(t *testing.T) {
	f := newFixture(t)

	archives := node(t, f.check(), RoadmapArchiveDir)
	if !archives.Valid || len(archives.Children) != 0 {
		t.Fatalf("empty archive directory reported as a problem: %v", codes(archives.Diagnostics))
	}
}

func TestParseArchiveName(t *testing.T) {
	tests := []struct {
		name     string
		wantErr  bool
		sequence int
	}{
		{name: "ROADMAP-2026-01-14.md", sequence: 1},
		{name: "ROADMAP-2026-01-14-02.md", sequence: 2},
		{name: "ROADMAP-2026-01-14-99.md", sequence: 99},
		{name: "ROADMAP-2026-01-14-01.md", wantErr: true},
		{name: "ROADMAP-2026-01-14-1.md", wantErr: true},
		{name: "ROADMAP-2026-02-30.md", wantErr: true},
		{name: "ROADMAP-2026-01-14.markdown", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseArchiveName(test.name)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", test.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArchiveName(%q): %v", test.name, err)
			}
			if parsed.Sequence != test.sequence {
				t.Errorf("sequence = %d, want %d", parsed.Sequence, test.sequence)
			}
		})
	}
}

// The naming rule has one implementation, and it never hands back a name that
// would overwrite an existing archive.
func TestNextArchiveNameNeverOverwrites(t *testing.T) {
	f := newFixture(t)
	dir := joinWorkspace(f.Workspace, RoadmapArchiveDir)
	day := time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)

	for _, want := range []string{"ROADMAP-2026-01-14.md", "ROADMAP-2026-01-14-02.md", "ROADMAP-2026-01-14-03.md"} {
		got, err := NextArchiveName(dir, day)
		if err != nil {
			t.Fatalf("NextArchiveName: %v", err)
		}
		if got != want {
			t.Fatalf("NextArchiveName = %q, want %q", got, want)
		}
		f.write(RoadmapArchiveDir+"/"+got, "---\narchivedAt: \"2026-01-14T09:00:00Z\"\n---\n\n# Body\n")
	}
}

// A different day starts again at the unsuffixed name.
func TestNextArchiveNameIsPerDay(t *testing.T) {
	f := newFixture(t)
	dir := joinWorkspace(f.Workspace, RoadmapArchiveDir)
	f.write(RoadmapArchiveDir+"/ROADMAP-2026-01-14.md", "---\narchivedAt: \"2026-01-14T09:00:00Z\"\n---\n\n# Body\n")

	got, err := NextArchiveName(dir, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextArchiveName: %v", err)
	}
	if got != "ROADMAP-2026-01-15.md" {
		t.Errorf("NextArchiveName = %q", got)
	}
}
