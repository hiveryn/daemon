package workspacefs

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestReadStableReturnsContentAndMtime(t *testing.T) {
	f := newFixture(t)
	file, err := readStable(joinWorkspace(f.Workspace, ProjectOverviewFileName))
	if err != nil {
		t.Fatalf("readStable: %v", err)
	}
	if string(file.Data) != validOverview {
		t.Errorf("content = %q", string(file.Data))
	}
	if file.ModTime.IsZero() || file.ModTime.Location() != time.UTC {
		t.Errorf("mod time = %v, want a UTC timestamp", file.ModTime)
	}
}

// shiftingStat reports a later mtime on each call, standing in for a writer
// that lands an edit while the file is being read.
func shiftingStat(t *testing.T) {
	t.Helper()
	original := statFile
	calls := 0
	statFile = func(path string) (fs.FileInfo, error) {
		info, err := original(path)
		if err != nil {
			return nil, err
		}
		calls++
		return shiftedInfo{FileInfo: info, offset: time.Duration(calls) * time.Second}, nil
	}
	t.Cleanup(func() { statFile = original })
}

type shiftedInfo struct {
	os.FileInfo
	offset time.Duration
}

func (i shiftedInfo) ModTime() time.Time { return i.FileInfo.ModTime().Add(i.offset) }

func TestReadStableDetectsMidReadEdit(t *testing.T) {
	f := newFixture(t)
	shiftingStat(t)

	_, err := readStable(joinWorkspace(f.Workspace, ProjectOverviewFileName))
	if !errors.Is(err, errIncompleteRead) {
		t.Fatalf("err = %v, want errIncompleteRead", err)
	}
}

// A file caught mid-edit is reported as unvalidated, not as invalid content:
// a torn read says nothing trustworthy about the document either way.
func TestCheckSurfacesIncompleteReadInsteadOfValidating(t *testing.T) {
	f := newFixture(t)
	shiftingStat(t)

	report := f.check()
	if report.Valid {
		t.Fatal("a workspace whose documents could not be read stably must not be reported valid")
	}

	n := node(t, report, ProjectOverviewFileName)
	if !n.Exists {
		t.Error("the file exists; only its content could not be read stably")
	}
	requireCode(t, n.Diagnostics, domain.DiagIncompleteRead)
	// No content verdict is invented from a torn read.
	for _, unexpected := range []string{domain.DiagEmptyBody, domain.DiagMissingFrontmatter, domain.DiagInvalidTimestamp, domain.DiagMissingField} {
		requireNoCode(t, n.Diagnostics, unexpected)
	}
	if n.DocumentUpdatedAt != nil {
		t.Error("a torn read must not yield a document timestamp")
	}
}
