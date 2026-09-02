package roadmapfs

import (
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func validState() state {
	return state{
		current: domain.Roadmap{SchemaVersion: 1, Title: "T", Items: []domain.RoadmapItem{
			item("a", nil, 10),
			item("b", strPtr("a"), 10),
			item("c", strPtr("a"), 20),
		}},
		archive: domain.RoadmapArchive{SchemaVersion: 1, Entries: []domain.RoadmapArchiveEntry{{
			RootID:        "old",
			ArchivedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			OriginalOrder: 10,
			Items: []domain.RoadmapItem{
				item("old", nil, 10),
				item("old-child", strPtr("old"), 10),
			},
		}}},
	}
}

func TestValidateState(t *testing.T) {
	t.Parallel()

	if err := validateState(validState()); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(st *state)
		wantSub string
	}{
		{
			name:    "duplicate id within current",
			mutate:  func(st *state) { st.current.Items = append(st.current.Items, item("a", nil, 30)) },
			wantSub: "duplicate item ID",
		},
		{
			name: "duplicate id across current and archive",
			mutate: func(st *state) {
				st.archive.Entries[0].Items[1].ID = "b"
				st.archive.Entries[0].Items[1].ParentID = strPtr("old")
			},
			wantSub: "duplicate item ID",
		},
		{
			name:    "bad id format",
			mutate:  func(st *state) { st.current.Items[0].ID = "Bad_ID" },
			wantSub: "kebab-case",
		},
		{
			name:    "bad kind",
			mutate:  func(st *state) { st.current.Items[0].Kind = "epic" },
			wantSub: "invalid kind",
		},
		{
			name:    "bad status",
			mutate:  func(st *state) { st.current.Items[0].Status = "started" },
			wantSub: "invalid status",
		},
		{
			name:    "empty title",
			mutate:  func(st *state) { st.current.Items[0].Title = "  " },
			wantSub: "empty title",
		},
		{
			name:    "empty outcome",
			mutate:  func(st *state) { st.current.Items[0].Outcome = "" },
			wantSub: "empty outcome",
		},
		{
			name:    "dangling parent",
			mutate:  func(st *state) { st.current.Items[1].ParentID = strPtr("ghost") },
			wantSub: "missing parent",
		},
		{
			name:    "self parent",
			mutate:  func(st *state) { st.current.Items[0].ParentID = strPtr("a") },
			wantSub: "its own parent",
		},
		{
			name: "parent cycle",
			mutate: func(st *state) {
				// a -> b and b -> a (parents), keeping c out of it.
				st.current.Items[0].ParentID = strPtr("b")
			},
			wantSub: "parent cycle",
		},
		{
			name:    "dangling dependency",
			mutate:  func(st *state) { st.current.Items[1].DependsOn = []string{"ghost"} },
			wantSub: "not a current roadmap item",
		},
		{
			name:    "self dependency",
			mutate:  func(st *state) { st.current.Items[1].DependsOn = []string{"b"} },
			wantSub: "depends on itself",
		},
		{
			name:    "duplicate dependency",
			mutate:  func(st *state) { st.current.Items[1].DependsOn = []string{"c", "c"} },
			wantSub: "more than once",
		},
		{
			name: "dependency cycle",
			mutate: func(st *state) {
				st.current.Items[1].DependsOn = []string{"c"}
				st.current.Items[2].DependsOn = []string{"b"}
			},
			wantSub: "dependency cycle",
		},
		{
			name:    "duplicate ticket link",
			mutate:  func(st *state) { st.current.Items[0].Tickets = []string{"2026-01-01-0000-x", "2026-01-01-0000-x"} },
			wantSub: "more than once",
		},
		{
			name:    "malformed ticket id",
			mutate:  func(st *state) { st.current.Items[0].Tickets = []string{"not-a-ticket"} },
			wantSub: `^\d{4}-\d{2}-\d{2}-\d{4}-[a-z0-9-]+$`,
		},
		{
			name:    "empty success criterion",
			mutate:  func(st *state) { st.current.Items[0].SuccessCriteria = []string{"ok", "  "} },
			wantSub: "empty success criterion at position 1",
		},
		{
			name:    "duplicate success criterion",
			mutate:  func(st *state) { st.current.Items[0].SuccessCriteria = []string{"same", "same"} },
			wantSub: "more than once",
		},
		{
			name:    "title too long",
			mutate:  func(st *state) { st.current.Items[0].Title = strings.Repeat("x", 201) },
			wantSub: "max 200",
		},
		{
			name:    "outcome too long",
			mutate:  func(st *state) { st.current.Items[0].Outcome = strings.Repeat("x", 2001) },
			wantSub: "max 2000",
		},
		{
			name:    "roadmap title too long",
			mutate:  func(st *state) { st.current.Title = strings.Repeat("x", 201) },
			wantSub: "max 200",
		},
		{
			name:    "archive summary too long",
			mutate:  func(st *state) { st.archive.Entries[0].Summary = strings.Repeat("x", 501) },
			wantSub: "max 500",
		},
		{
			name: "kind nesting violation",
			mutate: func(st *state) {
				st.current.Items[0].Kind = domain.RoadmapItemMilestone // parent of b
				st.current.Items[1].Kind = domain.RoadmapItemGoal
			},
			wantSub: "allowed nesting is goal > initiative > milestone",
		},
		{
			name: "dependency on ancestor",
			mutate: func(st *state) {
				st.current.Items[1].DependsOn = []string{"a"} // b's parent is a
			},
			wantSub: "depends on its ancestor",
		},
		{
			name: "dependency on descendant",
			mutate: func(st *state) {
				st.current.Items[0].DependsOn = []string{"b"}
			},
			wantSub: "depends on its own descendant",
		},
		{
			name:    "duplicate sibling order",
			mutate:  func(st *state) { st.current.Items[2].Order = 10 },
			wantSub: "share order",
		},
		{
			name:    "archive entry missing root",
			mutate:  func(st *state) { st.archive.Entries[0].RootID = "ghost" },
			wantSub: "does not contain its root",
		},
		{
			name:    "archive root with parent",
			mutate:  func(st *state) { st.archive.Entries[0].Items[0].ParentID = strPtr("old-child") },
			wantSub: "root item must have a null parent_id",
		},
		{
			name: "archive item outside subtree",
			mutate: func(st *state) {
				st.archive.Entries[0].Items[1].ParentID = strPtr("elsewhere")
			},
			wantSub: "outside the snapshot",
		},
		{
			name: "archive extra root",
			mutate: func(st *state) {
				st.archive.Entries[0].Items[1].ParentID = nil
			},
			wantSub: "has no parent but is not the root",
		},
		{
			name:    "archive missing timestamp",
			mutate:  func(st *state) { st.archive.Entries[0].ArchivedAt = time.Time{} },
			wantSub: "archived_at",
		},
		{
			name:    "wrong current schema version",
			mutate:  func(st *state) { st.current.SchemaVersion = 2 },
			wantSub: "schema_version",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := validState()
			tc.mutate(&st)
			err := validateState(st)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
