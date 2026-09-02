package roadmapfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

type fakeTicketService struct {
	board   domain.TicketBoard
	tickets map[string]domain.Ticket
	listErr error
}

func (f *fakeTicketService) ListTickets(context.Context, string) (domain.TicketBoard, error) {
	if f.listErr != nil {
		return domain.TicketBoard{}, f.listErr
	}
	return f.board, nil
}

func (f *fakeTicketService) GetTicket(_ context.Context, _ string, id string) (domain.Ticket, error) {
	ticket, ok := f.tickets[id]
	if !ok {
		return domain.Ticket{}, &domain.NotFoundError{Resource: "ticket", ID: id}
	}
	return ticket, nil
}

func (f *fakeTicketService) CreateTicket(context.Context, string, domain.CreateTicketParams) (domain.Ticket, error) {
	panic("not used")
}

func (f *fakeTicketService) EditTicket(context.Context, string, string, domain.EditTicketParams) (domain.Ticket, error) {
	panic("not used")
}

func (f *fakeTicketService) UpdateTicketMetadata(context.Context, string, string, domain.UpdateTicketMetadataParams) (domain.Ticket, error) {
	panic("not used")
}

func (f *fakeTicketService) DeleteTicket(context.Context, string, string) error {
	panic("not used")
}

func (f *fakeTicketService) MoveTicket(context.Context, string, string, domain.MoveTicketParams) (domain.Ticket, error) {
	panic("not used")
}

func (f *fakeTicketService) ConcludeTicket(context.Context, string, string, domain.TicketConclusion) (domain.Ticket, error) {
	panic("not used")
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	return NewService(&fakeTicketService{}), t.TempDir()
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func item(id string, parentID *string, order int) domain.RoadmapItem {
	return domain.RoadmapItem{
		ID:              id,
		Kind:            domain.RoadmapItemGoal,
		Title:           "Title " + id,
		Status:          domain.RoadmapStatusPlanned,
		Outcome:         "Outcome " + id,
		ParentID:        parentID,
		Order:           order,
		SuccessCriteria: []string{},
		Tickets:         []string{},
		DependsOn:       []string{},
	}
}

func TestReadEmptyRoadmap(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	view, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if view.View != "current" {
		t.Fatalf("view = %q, want current", view.View)
	}
	if view.Items == nil || len(view.Items) != 0 {
		t.Fatalf("items = %#v, want empty non-nil", view.Items)
	}
	if view.Tickets == nil || view.Warnings == nil {
		t.Fatalf("tickets/warnings must be non-nil, got %#v / %#v", view.Tickets, view.Warnings)
	}
	if view.Version == "" {
		t.Fatal("version must be non-empty for the empty roadmap")
	}

	again, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if again.Version != view.Version {
		t.Fatalf("empty roadmap version not stable: %q vs %q", view.Version, again.Version)
	}
	if _, err := os.Stat(roadmapDir(workspace)); !os.IsNotExist(err) {
		t.Fatalf("read must not create the roadmap directory, stat err = %v", err)
	}
}

func TestVersionChangesWithContent(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	empty, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	st := state{
		current: domain.Roadmap{SchemaVersion: 1, Title: "Test", Items: []domain.RoadmapItem{item("a", nil, 10)}},
		archive: domain.RoadmapArchive{SchemaVersion: 1},
	}
	if _, err := storeState(workspace, st, false); err != nil {
		t.Fatalf("storeState: %v", err)
	}
	view, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("Read after write: %v", err)
	}
	if view.Version == empty.Version {
		t.Fatal("version must change when content changes")
	}
	if len(view.Items) != 1 || view.Items[0].ID != "a" {
		t.Fatalf("items = %#v", view.Items)
	}
	if view.Title != "Test" {
		t.Fatalf("title = %q", view.Title)
	}
}

func TestDeterministicSerialization(t *testing.T) {
	t.Parallel()
	_, workspace := newTestService(t)

	a := item("a", nil, 10)
	b := item("b", strPtr("a"), 10)
	c := item("c", strPtr("a"), 20)
	archivedAt := time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC)
	entry := domain.RoadmapArchiveEntry{
		RootID:        "old",
		ArchivedAt:    archivedAt,
		OriginalOrder: 20,
		Items:         []domain.RoadmapItem{item("old", nil, 20)},
	}

	first := state{
		current: domain.Roadmap{SchemaVersion: 1, Title: "T", Items: []domain.RoadmapItem{a, b, c}},
		archive: domain.RoadmapArchive{SchemaVersion: 1, Entries: []domain.RoadmapArchiveEntry{entry}},
	}
	if _, err := storeState(workspace, first, false); err != nil {
		t.Fatalf("storeState: %v", err)
	}
	firstCurrent := readFile(t, filepath.Join(roadmapDir(workspace), currentFileName))
	firstArchive := readFile(t, filepath.Join(roadmapDir(workspace), archiveFileName))

	// Same logical state, scrambled item order and nil slices.
	c2 := c
	c2.SuccessCriteria = nil
	second := state{
		current: domain.Roadmap{SchemaVersion: 1, Title: "T", Items: []domain.RoadmapItem{c2, a, b}},
		archive: domain.RoadmapArchive{SchemaVersion: 1, Entries: []domain.RoadmapArchiveEntry{entry}},
	}
	if _, err := storeState(workspace, second, false); err != nil {
		t.Fatalf("second storeState: %v", err)
	}
	if got := readFile(t, filepath.Join(roadmapDir(workspace), currentFileName)); got != firstCurrent {
		t.Fatalf("current.yaml not deterministic:\n--- first ---\n%s\n--- second ---\n%s", firstCurrent, got)
	}
	if got := readFile(t, filepath.Join(roadmapDir(workspace), archiveFileName)); got != firstArchive {
		t.Fatalf("archive.yaml not deterministic")
	}
	if !strings.Contains(firstCurrent, "success_criteria: []") {
		t.Fatalf("empty arrays must serialize as [], got:\n%s", firstCurrent)
	}
	if !strings.Contains(firstCurrent, "parent_id: null") {
		t.Fatalf("top-level items must serialize parent_id: null, got:\n%s", firstCurrent)
	}
}

func TestFocusedReads(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	// a -> b -> d ; a -> c
	st := state{
		current: domain.Roadmap{SchemaVersion: 1, Items: []domain.RoadmapItem{
			item("a", nil, 10),
			item("b", strPtr("a"), 10),
			item("c", strPtr("a"), 20),
			item("d", strPtr("b"), 10),
		}},
		archive: domain.RoadmapArchive{SchemaVersion: 1},
	}
	if _, err := storeState(workspace, st, false); err != nil {
		t.Fatalf("storeState: %v", err)
	}

	ids := func(view domain.RoadmapView) []string {
		out := []string{}
		for _, it := range view.Items {
			out = append(out, it.ID)
		}
		return out
	}

	full, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{ID: "a"})
	if err != nil {
		t.Fatalf("Read subtree: %v", err)
	}
	if got := strings.Join(ids(full), ","); got != "a,b,d,c" {
		t.Fatalf("full subtree DFS = %s, want a,b,d,c", got)
	}

	only, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{ID: "a", Depth: intPtr(0)})
	if err != nil {
		t.Fatalf("Read depth 0: %v", err)
	}
	if got := strings.Join(ids(only), ","); got != "a" {
		t.Fatalf("depth 0 = %s, want a", got)
	}

	one, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{ID: "a", Depth: intPtr(1)})
	if err != nil {
		t.Fatalf("Read depth 1: %v", err)
	}
	if got := strings.Join(ids(one), ","); got != "a,b,c" {
		t.Fatalf("depth 1 = %s, want a,b,c", got)
	}

	roots, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{Depth: intPtr(0)})
	if err != nil {
		t.Fatalf("Read roots: %v", err)
	}
	if got := strings.Join(ids(roots), ","); got != "a" {
		t.Fatalf("roots-only = %s, want a", got)
	}

	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{ID: "nope"}); err == nil {
		t.Fatal("unknown id must be NOT_FOUND")
	} else {
		var notFound *domain.NotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("want NotFoundError, got %T: %v", err, err)
		}
	}

	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{View: "bogus"}); err == nil {
		t.Fatal("bogus view must fail")
	}
	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{View: "archive", Depth: intPtr(1)}); err == nil {
		t.Fatal("depth on archive view must fail")
	}
	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{Depth: intPtr(-1)}); err == nil {
		t.Fatal("negative depth must fail")
	}
}

func TestArchiveReads(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	root := item("done-goal", nil, 10)
	child := item("done-child", strPtr("done-goal"), 10)
	st := state{
		current: domain.Roadmap{SchemaVersion: 1},
		archive: domain.RoadmapArchive{SchemaVersion: 1, Entries: []domain.RoadmapArchiveEntry{{
			RootID:        "done-goal",
			ArchivedAt:    time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC),
			Summary:       "Delivered.",
			OriginalOrder: 10,
			Items:         []domain.RoadmapItem{root, child},
		}}},
	}
	if _, err := storeState(workspace, st, false); err != nil {
		t.Fatalf("storeState: %v", err)
	}

	list, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{View: "archive"})
	if err != nil {
		t.Fatalf("Read archive list: %v", err)
	}
	if len(list.ArchiveEntries) != 1 {
		t.Fatalf("archive entries = %#v", list.ArchiveEntries)
	}
	summary := list.ArchiveEntries[0]
	if summary.RootID != "done-goal" || summary.RootTitle != "Title done-goal" || summary.ItemCount != 2 || summary.Summary != "Delivered." {
		t.Fatalf("summary = %#v", summary)
	}
	if list.ArchiveEntry != nil || len(list.Items) != 0 {
		t.Fatalf("archive listing must be compact, got %#v", list)
	}

	entry, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{View: "archive", ID: "done-goal"})
	if err != nil {
		t.Fatalf("Read archive entry: %v", err)
	}
	if entry.ArchiveEntry == nil || len(entry.ArchiveEntry.Items) != 2 {
		t.Fatalf("entry = %#v", entry.ArchiveEntry)
	}

	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{View: "archive", ID: "nope"}); err == nil {
		t.Fatal("unknown archive entry must be NOT_FOUND")
	}
}

func TestParseErrorsAndSchemaVersion(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	dir := roadmapDir(workspace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, currentFileName), []byte("items: [not: closed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{}); err == nil {
		t.Fatal("invalid YAML must fail the read")
	} else {
		var validation *domain.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("want ValidationError, got %T: %v", err, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, currentFileName), []byte("schema_version: 99\ntitle: x\nitems: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{}); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("unsupported schema_version must fail, got %v", err)
	}

	// Unknown keys are rejected (strict decode) so misspelled or unsupported
	// keys fail loudly instead of silently vanishing on the next write.
	if err := os.WriteFile(filepath.Join(dir, currentFileName), []byte("schema_version: 1\ntitle: x\nitems: []\nowner: kareem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}

	// Multi-document files (e.g. a merge concatenation) are rejected instead
	// of silently loading — and later destroying — only the first document.
	if err := os.WriteFile(filepath.Join(dir, currentFileName), []byte("schema_version: 1\ntitle: x\nitems: []\n---\nschema_version: 1\ntitle: y\nitems: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{}); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("multi-document file must fail actionably, got %v", err)
	}
}

func TestTicketResolutionFailureDegradesToWarning(t *testing.T) {
	t.Parallel()
	fake := &fakeTicketService{listErr: errors.New("board unreadable")}
	svc := NewService(fake)
	workspace := t.TempDir()

	st := state{
		current: domain.Roadmap{SchemaVersion: 1, Items: func() []domain.RoadmapItem {
			it := item("a", nil, 10)
			it.Tickets = []string{"2026-01-01-0000-x"}
			return []domain.RoadmapItem{it}
		}()},
		archive: domain.RoadmapArchive{SchemaVersion: 1},
	}
	if _, err := storeState(workspace, st, false); err != nil {
		t.Fatalf("storeState: %v", err)
	}

	view, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("read must not fail on ticket-resolution errors, got %v", err)
	}
	if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], "ticket resolution unavailable") {
		t.Fatalf("warnings = %#v", view.Warnings)
	}
	if len(view.Items) != 1 || len(view.Tickets) != 0 {
		t.Fatalf("view = %#v", view)
	}

	// Updates commit and report the same warning instead of aborting.
	result, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{
		Version: view.Version,
		Ops:     []domain.RoadmapOp{{Type: domain.RoadmapOpLinkTicket, ID: "a", TicketID: "2026-01-01-0000-y"}},
	})
	if err != nil {
		t.Fatalf("update must not fail on ticket-resolution errors, got %v", err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "ticket resolution unavailable") {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	after, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if after.Version != result.Version {
		t.Fatal("update must have committed despite resolution failure")
	}
}

func TestCrossFileDuplicateWarning(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	dup := item("dup", nil, 10)
	st := state{
		current: domain.Roadmap{SchemaVersion: 1, Items: []domain.RoadmapItem{dup}},
		archive: domain.RoadmapArchive{SchemaVersion: 1, Entries: []domain.RoadmapArchiveEntry{{
			RootID:        "dup",
			ArchivedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			OriginalOrder: 10,
			Items:         []domain.RoadmapItem{dup},
		}}},
	}
	if _, err := storeState(workspace, st, false); err != nil {
		t.Fatalf("storeState: %v", err)
	}
	view, err := svc.Read(t.Context(), workspace, domain.RoadmapQuery{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], "interrupted write") {
		t.Fatalf("warnings = %#v", view.Warnings)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(data)
}
