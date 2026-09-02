package roadmapfs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func statusPtr(s domain.RoadmapItemStatus) *domain.RoadmapItemStatus { return &s }

func createOp(id string, parentID *string) domain.RoadmapOp {
	return domain.RoadmapOp{
		Type:     domain.RoadmapOpCreate,
		ID:       id,
		Kind:     domain.RoadmapItemGoal,
		Title:    strPtr("Title " + id),
		Outcome:  strPtr("Outcome " + id),
		ParentID: parentID,
	}
}

func mustRead(t *testing.T, svc *Service, workspace string, query domain.RoadmapQuery) domain.RoadmapView {
	t.Helper()
	view, err := svc.Read(t.Context(), workspace, query)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return view
}

func mustUpdate(t *testing.T, svc *Service, workspace string, ops ...domain.RoadmapOp) domain.RoadmapUpdateResult {
	t.Helper()
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	result, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: ops})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return result
}

func itemByID(t *testing.T, view domain.RoadmapView, id string) domain.RoadmapItem {
	t.Helper()
	for _, it := range view.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %q not in view %#v", id, view.Items)
	return domain.RoadmapItem{}
}

func TestCreateFlow(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	title := "Hiveryn roadmap"
	result, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{
		Version: view.Version,
		Title:   &title,
		Ops:     []domain.RoadmapOp{createOp("first-goal", nil)},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Version == view.Version || result.Version == "" {
		t.Fatalf("version = %q (was %q)", result.Version, view.Version)
	}
	if len(result.Applied) != 1 || result.Applied[0].ItemID != "first-goal" || result.Applied[0].Index != 0 {
		t.Fatalf("applied = %#v", result.Applied)
	}

	after := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if after.Version != result.Version {
		t.Fatalf("read version %q != update version %q", after.Version, result.Version)
	}
	if after.Title != title {
		t.Fatalf("title = %q", after.Title)
	}
	created := itemByID(t, after, "first-goal")
	if created.Order != 10 || created.Status != domain.RoadmapStatusPlanned || created.ParentID != nil {
		t.Fatalf("created = %#v", created)
	}
	if data := readFile(t, filepath.Join(roadmapDir(workspace), currentFileName)); !strings.Contains(data, "tickets: []") {
		t.Fatalf("current.yaml missing empty arrays:\n%s", data)
	}
}

func TestVersionGuard(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	if _, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Ops: []domain.RoadmapOp{createOp("a", nil)}}); err == nil {
		t.Fatal("missing version must fail")
	}

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	mustUpdate(t, svc, workspace, createOp("a", nil))

	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{createOp("b", nil)}})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale version: want ConflictError, got %T: %v", err, err)
	}

	view = mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if _, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version}); err == nil || !strings.Contains(err.Error(), "nothing to do") {
		t.Fatalf("empty batch must fail, got %v", err)
	}
}

func TestBatchAtomicity(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("existing", nil))

	before := readFile(t, filepath.Join(roadmapDir(workspace), currentFileName))
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		createOp("fresh", nil),
		createOp("existing", nil), // duplicate ID -> fails
	}})
	if err == nil {
		t.Fatal("duplicate create must fail")
	}
	if !strings.Contains(err.Error(), "ops[1]") {
		t.Fatalf("error must name the failing op index, got %q", err.Error())
	}
	if after := readFile(t, filepath.Join(roadmapDir(workspace), currentFileName)); after != before {
		t.Fatal("failed batch must write nothing")
	}
	fresh := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if len(fresh.Items) != 1 {
		t.Fatalf("items = %#v", fresh.Items)
	}
}

func TestCreateReferenceWithinBatch(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	depOp := createOp("dependent", nil)
	depOp.DependsOn = &[]string{"base"}
	mustUpdate(t, svc, workspace,
		createOp("base", nil),
		depOp,
		createOp("child", strPtr("base")),
	)
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if got := itemByID(t, view, "dependent").DependsOn; len(got) != 1 || got[0] != "base" {
		t.Fatalf("depends_on = %#v", got)
	}
	if got := itemByID(t, view, "child").ParentID; got == nil || *got != "base" {
		t.Fatalf("parent = %v", got)
	}
}

func TestOrderInsertionAndRenumbering(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("a", nil), createOp("b", nil), createOp("c", nil))

	// Insert between a (10) and b (20) using an order value of 15.
	mid := createOp("mid", nil)
	mid.Order = intPtr(15)
	mustUpdate(t, svc, workspace, mid)

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	order := []string{}
	for _, it := range view.Items {
		order = append(order, it.ID)
	}
	if got := strings.Join(order, ","); got != "a,mid,b,c" {
		t.Fatalf("sibling order = %s, want a,mid,b,c", got)
	}
	for i, it := range view.Items {
		if it.Order != (i+1)*10 {
			t.Fatalf("item %q order = %d, want %d", it.ID, it.Order, (i+1)*10)
		}
	}
}

func TestUpdateOp(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("a", nil), createOp("b", nil))

	mustUpdate(t, svc, workspace, domain.RoadmapOp{
		Type:            domain.RoadmapOpUpdate,
		ID:              "a",
		Title:           strPtr("New title"),
		Status:          statusPtr(domain.RoadmapStatusActive),
		Outcome:         strPtr("New outcome"),
		SuccessCriteria: &[]string{"one", "two"},
		DependsOn:       &[]string{"b", "b"},
	})
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	updated := itemByID(t, view, "a")
	if updated.Title != "New title" || updated.Status != domain.RoadmapStatusActive || updated.Outcome != "New outcome" {
		t.Fatalf("updated = %#v", updated)
	}
	if len(updated.SuccessCriteria) != 2 || len(updated.DependsOn) != 1 || updated.DependsOn[0] != "b" {
		t.Fatalf("lists = %#v / %#v (depends_on must be deduplicated)", updated.SuccessCriteria, updated.DependsOn)
	}

	fresh := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if _, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: fresh.Version, Ops: []domain.RoadmapOp{{Type: domain.RoadmapOpUpdate, ID: "missing"}}}); err == nil {
		t.Fatal("update of missing item must fail")
	} else {
		var notFound *domain.NotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("want NotFoundError, got %T: %v", err, err)
		}
	}
	if _, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: fresh.Version, Ops: []domain.RoadmapOp{{Type: domain.RoadmapOpUpdate, ID: "a"}}}); err == nil || !strings.Contains(err.Error(), "no fields") {
		t.Fatalf("field-less update must fail, got %v", err)
	}
}

func TestMoveOp(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace,
		createOp("a", nil),
		createOp("b", nil),
		createOp("a-child", strPtr("a")),
	)

	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpMove, ID: "a-child", ParentID: strPtr("b")})
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	moved := itemByID(t, view, "a-child")
	if moved.ParentID == nil || *moved.ParentID != "b" || moved.Order != 10 {
		t.Fatalf("moved = %#v", moved)
	}

	fresh := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: fresh.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpMove, ID: "b", ParentID: strPtr("a-child")},
	}})
	if err == nil || !strings.Contains(err.Error(), "its own descendant") {
		t.Fatalf("move under own descendant must fail, got %v", err)
	}

	// Move to top level via explicit empty parent.
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpMove, ID: "a-child", ParentID: strPtr(""), Order: intPtr(5)})
	view = mustRead(t, svc, workspace, domain.RoadmapQuery{})
	moved = itemByID(t, view, "a-child")
	if moved.ParentID != nil || moved.Order != 10 {
		t.Fatalf("moved to root = %#v", moved)
	}
}

func TestMoveOmittedParentReordersInPlace(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace,
		createOp("parent", nil),
		createOp("first", strPtr("parent")),
		createOp("second", strPtr("parent")),
	)

	// No parent_id: pure reorder — the item must keep its current parent.
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpMove, ID: "second", Order: intPtr(5)})
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	moved := itemByID(t, view, "second")
	if moved.ParentID == nil || *moved.ParentID != "parent" {
		t.Fatalf("omitted parent_id must keep the current parent, got %#v", moved)
	}
	if moved.Order != 10 || itemByID(t, view, "first").Order != 20 {
		t.Fatalf("reorder failed: second=%d first=%d", moved.Order, itemByID(t, view, "first").Order)
	}
}

func TestArchiveRestoreCannotMix(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("a", nil), createOp("b", nil))
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "a"})

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpRestore, ID: "a"},
		{Type: domain.RoadmapOpArchive, ID: "b"},
	}})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed archive+restore batch must be rejected, got %v", err)
	}
}

func TestRestoreResolvesDescendantTickets(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace,
		createOp("goal", nil),
		createOp("child", strPtr("goal")),
		domain.RoadmapOp{Type: domain.RoadmapOpLinkTicket, ID: "child", TicketID: "2026-01-01-0000-gone"},
	)
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "goal"})

	result := mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpRestore, ID: "goal"})
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "2026-01-01-0000-gone") {
		t.Fatalf("restore must resolve descendants' tickets and keep their warnings, got %#v", result.Warnings)
	}
}

func TestLengthCapsCountRunes(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	// 150 multi-byte characters (450 bytes) is within the 200-character cap.
	cjk := createOp("cjk-goal", nil)
	cjk.Title = strPtr(strings.Repeat("日", 150))
	mustUpdate(t, svc, workspace, cjk)
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if got := itemByID(t, view, "cjk-goal").Title; got != strings.Repeat("日", 150) {
		t.Fatalf("title = %q", got)
	}
}

func TestLinkUnlinkIdempotent(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("a", nil))

	link := domain.RoadmapOp{Type: domain.RoadmapOpLinkTicket, ID: "a", TicketID: "2026-01-01-0000-x"}
	first := mustUpdate(t, svc, workspace, link)
	if !strings.Contains(first.Applied[0].Detail, "linked ticket") {
		t.Fatalf("detail = %q", first.Applied[0].Detail)
	}
	second := mustUpdate(t, svc, workspace, link)
	if !strings.Contains(second.Applied[0].Detail, "no-op") {
		t.Fatalf("re-link detail = %q", second.Applied[0].Detail)
	}

	unlink := domain.RoadmapOp{Type: domain.RoadmapOpUnlinkTicket, ID: "a", TicketID: "2026-01-01-0000-x"}
	third := mustUpdate(t, svc, workspace, unlink)
	if !strings.Contains(third.Applied[0].Detail, "unlinked") {
		t.Fatalf("unlink detail = %q", third.Applied[0].Detail)
	}
	fourth := mustUpdate(t, svc, workspace, unlink)
	if !strings.Contains(fourth.Applied[0].Detail, "no-op") {
		t.Fatalf("re-unlink detail = %q", fourth.Applied[0].Detail)
	}
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if got := itemByID(t, view, "a").Tickets; len(got) != 0 {
		t.Fatalf("tickets = %#v", got)
	}
}

func TestArchiveRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace,
		createOp("keep", nil),
		createOp("goal", nil),
		createOp("child", strPtr("goal")),
		createOp("grandchild", strPtr("child")),
	)

	result := mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "goal", Summary: "Shipped."})
	if !strings.Contains(result.Applied[0].Detail, "archived 3") {
		t.Fatalf("detail = %q", result.Applied[0].Detail)
	}

	current := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if len(current.Items) != 1 || current.Items[0].ID != "keep" || current.Items[0].Order != 10 {
		t.Fatalf("current after archive = %#v", current.Items)
	}
	archive := mustRead(t, svc, workspace, domain.RoadmapQuery{View: "archive"})
	if len(archive.ArchiveEntries) != 1 || archive.ArchiveEntries[0].ItemCount != 3 || archive.ArchiveEntries[0].Summary != "Shipped." {
		t.Fatalf("archive = %#v", archive.ArchiveEntries)
	}
	entry := mustRead(t, svc, workspace, domain.RoadmapQuery{View: "archive", ID: "goal"})
	if root := entry.ArchiveEntry.Items[0]; root.ID != "goal" || root.ParentID != nil {
		t.Fatalf("snapshot root = %#v", root)
	}

	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpRestore, ID: "goal"})
	restored := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if len(restored.Items) != 4 {
		t.Fatalf("restored items = %#v", restored.Items)
	}
	goal := itemByID(t, restored, "goal")
	if goal.ParentID != nil {
		t.Fatalf("goal parent = %v", goal.ParentID)
	}
	if child := itemByID(t, restored, "child"); child.ParentID == nil || *child.ParentID != "goal" {
		t.Fatalf("child = %#v", child)
	}
	emptyArchive := mustRead(t, svc, workspace, domain.RoadmapQuery{View: "archive"})
	if len(emptyArchive.ArchiveEntries) != 0 {
		t.Fatalf("archive entry must be removed after restore, got %#v", emptyArchive.ArchiveEntries)
	}
}

func TestArchiveInboundLinkConflict(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	dep := createOp("dependent", nil)
	dep.DependsOn = &[]string{"target"}
	mustUpdate(t, svc, workspace, createOp("target", nil), dep)

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpArchive, ID: "target"},
	}})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want ConflictError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), `"dependent" depends on "target"`) {
		t.Fatalf("conflict must list offending links, got %q", err.Error())
	}
}

func TestRestoreWithReplacementParent(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("parent", nil), createOp("leaf", strPtr("parent")))
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "leaf"})
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "parent"})

	// Original parent is archived, so a plain restore of leaf must fail.
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpRestore, ID: "leaf"},
	}})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("restore without parent must fail actionably, got %v", err)
	}

	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpRestore, ID: "leaf", ParentID: strPtr(""), Order: intPtr(1)})
	restored := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	leaf := itemByID(t, restored, "leaf")
	if leaf.ParentID != nil || leaf.Order != 10 {
		t.Fatalf("leaf = %#v", leaf)
	}
}

func TestRestoreUnresolvedDependencies(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	dep := createOp("b", nil)
	dep.DependsOn = &[]string{"a"}
	mustUpdate(t, svc, workspace, createOp("a", nil), dep)
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "b"})
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "a"})

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpRestore, ID: "b"},
	}})
	if err == nil || !strings.Contains(err.Error(), `"b" depends on "a"`) {
		t.Fatalf("restore with unresolved deps must fail listing them, got %v", err)
	}

	// Restoring the dependency first (in the same batch) makes it valid.
	mustUpdate(t, svc, workspace,
		domain.RoadmapOp{Type: domain.RoadmapOpRestore, ID: "a"},
		domain.RoadmapOp{Type: domain.RoadmapOpRestore, ID: "b"},
	)
	restored := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if len(restored.Items) != 2 {
		t.Fatalf("items = %#v", restored.Items)
	}
}

func TestCreateReusingArchivedIDConflicts(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("goal", nil))
	mustUpdate(t, svc, workspace, domain.RoadmapOp{Type: domain.RoadmapOpArchive, ID: "goal"})

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{createOp("goal", nil)}})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want ConflictError for archived ID reuse, got %T: %v", err, err)
	}
}

func TestLinkTicketRejectsMalformedID(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("a", nil))

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpLinkTicket, ID: "a", TicketID: "not-a-ticket"},
	}})
	if err == nil || !strings.Contains(err.Error(), `^\d{4}-\d{2}-\d{2}-\d{4}-[a-z0-9-]+$`) {
		t.Fatalf("malformed ticket link must fail with the expected ID shape in the message, got %v", err)
	}
}

func TestKindNestingRejected(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	milestone := createOp("small-milestone", nil)
	milestone.Kind = domain.RoadmapItemMilestone
	mustUpdate(t, svc, workspace, milestone)

	// create: a goal cannot nest under a milestone.
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		createOp("big-goal", strPtr("small-milestone")),
	}})
	if err == nil || !strings.Contains(err.Error(), "allowed nesting is goal > initiative > milestone") {
		t.Fatalf("create with bad nesting must fail actionably, got %v", err)
	}

	// move: same rule.
	mustUpdate(t, svc, workspace, createOp("big-goal", nil))
	view = mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err = svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpMove, ID: "big-goal", ParentID: strPtr("small-milestone")},
	}})
	if err == nil || !strings.Contains(err.Error(), "allowed nesting is goal > initiative > milestone") {
		t.Fatalf("move with bad nesting must fail actionably, got %v", err)
	}

	// Same kind under same kind stays allowed.
	mustUpdate(t, svc, workspace, createOp("sub-goal", strPtr("big-goal")))
}

func TestDependencyOnAncestorRejected(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)
	mustUpdate(t, svc, workspace, createOp("parent", nil), createOp("child", strPtr("parent")))

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpUpdate, ID: "child", DependsOn: &[]string{"parent"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "ancestor") || !strings.Contains(err.Error(), "remove the dependency") {
		t.Fatalf("dep on ancestor must fail actionably, got %v", err)
	}

	_, err = svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpUpdate, ID: "parent", DependsOn: &[]string{"child"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "descendant") {
		t.Fatalf("dep on descendant must fail actionably, got %v", err)
	}
}

func TestLengthCaps(t *testing.T) {
	t.Parallel()
	svc, workspace := newTestService(t)

	longTitle := createOp("a", nil)
	longTitle.Title = strPtr(strings.Repeat("x", 201))
	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err := svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{longTitle}})
	if err == nil || !strings.Contains(err.Error(), "max 200") {
		t.Fatalf("long title must fail with the cap in the message, got %v", err)
	}

	mustUpdate(t, svc, workspace, createOp("a", nil))
	view = mustRead(t, svc, workspace, domain.RoadmapQuery{})
	_, err = svc.Update(t.Context(), workspace, domain.UpdateRoadmapParams{Version: view.Version, Ops: []domain.RoadmapOp{
		{Type: domain.RoadmapOpArchive, ID: "a", Summary: strings.Repeat("x", 501)},
	}})
	if err == nil || !strings.Contains(err.Error(), "max 500") {
		t.Fatalf("long summary must fail with the cap in the message, got %v", err)
	}
}

func TestTicketResolution(t *testing.T) {
	t.Parallel()
	concluded := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeTicketService{
		board: domain.TicketBoard{
			Done: []domain.TicketSummary{{
				ID:              "2026-07-01-1000-shipped",
				Status:          domain.TicketStatusDone,
				Title:           "Shipped ticket",
				Repo:            "daemon",
				AdditionalRepos: []string{"shared"},
				HasConclusion:   true,
			}},
		},
		tickets: map[string]domain.Ticket{
			"2026-07-01-1000-shipped": {
				TicketSummary: domain.TicketSummary{ID: "2026-07-01-1000-shipped"},
				Conclusion: &domain.TicketConclusion{
					StartedAt:   concluded,
					ConcludedAt: concluded,
					Outcome:     domain.TicketOutcomeCompleted,
				},
			},
		},
	}
	svc := NewService(fake)
	workspace := t.TempDir()

	mustUpdate(t, svc, workspace,
		createOp("goal", nil),
		domain.RoadmapOp{Type: domain.RoadmapOpLinkTicket, ID: "goal", TicketID: "2026-07-01-1000-shipped"},
		domain.RoadmapOp{Type: domain.RoadmapOpLinkTicket, ID: "goal", TicketID: "2026-01-01-0000-missing"},
	)

	view := mustRead(t, svc, workspace, domain.RoadmapQuery{})
	if len(view.Tickets) != 1 {
		t.Fatalf("tickets = %#v", view.Tickets)
	}
	info := view.Tickets[0]
	if info.ID != "2026-07-01-1000-shipped" || info.Status != domain.TicketStatusDone ||
		info.Repo != "daemon" || !info.HasConclusion || info.ConclusionOutcome != domain.TicketOutcomeCompleted {
		t.Fatalf("info = %#v", info)
	}
	if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], "2026-01-01-0000-missing") {
		t.Fatalf("warnings = %#v", view.Warnings)
	}
}
