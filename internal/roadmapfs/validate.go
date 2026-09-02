package roadmapfs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hiveryn/daemon/internal/domain"
)

var (
	itemIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// ticketIDPattern is the shape of board ticket IDs
	// (date-time prefix + slug), e.g. 2026-08-20-1127-show-concluded-diffs.
	ticketIDPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{4}-[a-z0-9-]+$`)
)

const (
	maxItemIDLength  = 64
	maxTitleLength   = 200
	maxOutcomeLength = 2000
	maxSummaryLength = 500

	ticketIDShapeHint = `must match the board ticket ID shape ^\d{4}-\d{2}-\d{2}-\d{4}-[a-z0-9-]+$ (e.g. 2026-08-20-1127-show-concluded-diffs); copy the ID from listTickets`
	kindNestingHint   = "allowed nesting is goal > initiative > milestone (an item may also nest under the same kind); change the kind or pick a different parent"
)

// kindRank orders item kinds for hierarchy validation: a child's kind must
// rank equal or below its parent's.
func kindRank(kind domain.RoadmapItemKind) int {
	switch kind {
	case domain.RoadmapItemGoal:
		return 0
	case domain.RoadmapItemInitiative:
		return 1
	case domain.RoadmapItemMilestone:
		return 2
	default:
		return -1
	}
}

// validateState checks every invariant of the complete proposed state before
// anything is written. The op engine keeps the state valid by construction,
// so a failure here usually means a hand-edited file was loaded.
func validateState(st state) error {
	if st.current.SchemaVersion != domain.RoadmapSchemaVersion {
		return validationErr(currentFileName, "unsupported schema_version %d (want %d)", st.current.SchemaVersion, domain.RoadmapSchemaVersion)
	}
	if st.archive.SchemaVersion != domain.RoadmapSchemaVersion {
		return validationErr(archiveFileName, "unsupported schema_version %d (want %d)", st.archive.SchemaVersion, domain.RoadmapSchemaVersion)
	}
	if utf8.RuneCountInString(st.current.Title) > maxTitleLength {
		return validationErr("title", "roadmap title is %d characters, max %d; shorten it", utf8.RuneCountInString(st.current.Title), maxTitleLength)
	}

	// Per-item field validity plus global ID uniqueness across current items
	// and every archive entry's snapshot.
	seen := make(map[string]string) // id -> location description
	registerItem := func(item domain.RoadmapItem, location string) error {
		if err := validateItemFields(item, location); err != nil {
			return err
		}
		if prior, dup := seen[item.ID]; dup {
			return validationErr("items", "duplicate item ID %q (in %s and %s); IDs must be unique across current and archived items", item.ID, prior, location)
		}
		seen[item.ID] = location
		return nil
	}
	for _, item := range st.current.Items {
		if err := registerItem(item, currentFileName); err != nil {
			return err
		}
	}
	for _, entry := range st.archive.Entries {
		for _, item := range entry.Items {
			if err := registerItem(item, fmt.Sprintf("archive entry %q", entry.RootID)); err != nil {
				return err
			}
		}
	}

	if err := validateCurrentGraph(st.current.Items); err != nil {
		return err
	}
	for _, entry := range st.archive.Entries {
		if err := validateArchiveEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func validationErr(field, format string, args ...any) error {
	return &domain.ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

func validateItemFields(item domain.RoadmapItem, location string) error {
	if !itemIDPattern.MatchString(item.ID) || len(item.ID) > maxItemIDLength {
		return validationErr("id", "item ID %q (in %s) must be kebab-case ([a-z0-9]+(-[a-z0-9]+)*), at most %d characters", item.ID, location, maxItemIDLength)
	}
	if !item.Kind.Valid() {
		return validationErr("kind", "item %q has invalid kind %q (want goal, initiative, or milestone)", item.ID, item.Kind)
	}
	if !item.Status.Valid() {
		return validationErr("status", "item %q has invalid status %q (want planned, active, blocked, or done)", item.ID, item.Status)
	}
	if strings.TrimSpace(item.Title) == "" {
		return validationErr("title", "item %q has an empty title", item.ID)
	}
	if utf8.RuneCountInString(item.Title) > maxTitleLength {
		return validationErr("title", "item %q title is %d characters, max %d; shorten it", item.ID, utf8.RuneCountInString(item.Title), maxTitleLength)
	}
	if strings.TrimSpace(item.Outcome) == "" {
		return validationErr("outcome", "item %q has an empty outcome", item.ID)
	}
	if utf8.RuneCountInString(item.Outcome) > maxOutcomeLength {
		return validationErr("outcome", "item %q outcome is %d characters, max %d; shorten it (it is an intended result, not an implementation checklist)", item.ID, utf8.RuneCountInString(item.Outcome), maxOutcomeLength)
	}
	for i, criterion := range item.SuccessCriteria {
		if strings.TrimSpace(criterion) == "" {
			return validationErr("success_criteria", "item %q has an empty success criterion at position %d; remove it or provide text", item.ID, i)
		}
	}
	if dup := firstDuplicate(item.SuccessCriteria); dup != "" {
		return validationErr("success_criteria", "item %q lists success criterion %q more than once; remove the duplicate", item.ID, dup)
	}
	if dup := firstDuplicate(item.Tickets); dup != "" {
		return validationErr("tickets", "item %q links ticket %q more than once", item.ID, dup)
	}
	for _, ticketID := range item.Tickets {
		if !ticketIDPattern.MatchString(ticketID) {
			return validationErr("tickets", "item %q links invalid ticket ID %q; it %s", item.ID, ticketID, ticketIDShapeHint)
		}
	}
	if dup := firstDuplicate(item.DependsOn); dup != "" {
		return validationErr("depends_on", "item %q lists dependency %q more than once", item.ID, dup)
	}
	return nil
}

func firstDuplicate(values []string) string {
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if seen[v] {
			return v
		}
		seen[v] = true
	}
	return ""
}

// validateCurrentGraph checks parent references, parent cycles, dependency
// references, dependency cycles, and sibling order uniqueness for the current
// items.
func validateCurrentGraph(items []domain.RoadmapItem) error {
	byID := make(map[string]domain.RoadmapItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}

	for _, item := range items {
		if item.ParentID == nil {
			continue
		}
		parent := *item.ParentID
		if parent == item.ID {
			return validationErr("parent_id", "item %q is its own parent", item.ID)
		}
		parentItem, ok := byID[parent]
		if !ok {
			return validationErr("parent_id", "item %q references missing parent %q", item.ID, parent)
		}
		if err := validateKindNesting(parentItem, item); err != nil {
			return err
		}
	}
	if cycle := findCycle(items, func(item domain.RoadmapItem) []string {
		if item.ParentID == nil {
			return nil
		}
		return []string{*item.ParentID}
	}); cycle != nil {
		return validationErr("parent_id", "parent cycle: %s", strings.Join(cycle, " -> "))
	}

	for _, item := range items {
		ancestors := ancestorSet(byID, item)
		for _, dep := range item.DependsOn {
			if dep == item.ID {
				return validationErr("depends_on", "item %q depends on itself", item.ID)
			}
			depItem, ok := byID[dep]
			if !ok {
				return validationErr("depends_on", "item %q depends on %q, which is not a current roadmap item", item.ID, dep)
			}
			if ancestors[dep] {
				return validationErr("depends_on", "item %q depends on its ancestor %q; the parent chain already implies that ordering — remove the dependency", item.ID, dep)
			}
			if ancestorSet(byID, depItem)[item.ID] {
				return validationErr("depends_on", "item %q depends on its own descendant %q; the parent chain already implies that ordering — remove the dependency", item.ID, dep)
			}
		}
	}
	if cycle := findCycle(items, func(item domain.RoadmapItem) []string {
		return item.DependsOn
	}); cycle != nil {
		return validationErr("depends_on", "dependency cycle: %s", strings.Join(cycle, " -> "))
	}

	return validateSiblingOrders(items)
}

// validateKindNesting enforces the hierarchy order goal > initiative >
// milestone: a child's kind must rank equal to or below its parent's.
func validateKindNesting(parent, child domain.RoadmapItem) error {
	if kindRank(child.Kind) < kindRank(parent.Kind) {
		return validationErr("parent_id", "item %q (%s) cannot nest under %q (%s); %s", child.ID, child.Kind, parent.ID, parent.Kind, kindNestingHint)
	}
	return nil
}

// ancestorSet returns the IDs on item's parent chain. A visited guard keeps
// it terminating even on a (separately rejected) parent cycle.
func ancestorSet(byID map[string]domain.RoadmapItem, item domain.RoadmapItem) map[string]bool {
	out := map[string]bool{}
	current := item
	for current.ParentID != nil {
		parent, ok := byID[*current.ParentID]
		if !ok || out[parent.ID] {
			break
		}
		out[parent.ID] = true
		current = parent
	}
	return out
}

func validateSiblingOrders(items []domain.RoadmapItem) error {
	type slot struct {
		order int
		id    string
	}
	groups := make(map[string][]slot)
	for _, item := range items {
		key := parentKey(item.ParentID)
		groups[key] = append(groups[key], slot{order: item.Order, id: item.ID})
	}
	for key, group := range groups {
		byOrder := make(map[int]string, len(group))
		for _, s := range group {
			if other, dup := byOrder[s.order]; dup {
				where := "top-level items"
				if key != "" {
					where = fmt.Sprintf("children of %q", key)
				}
				return validationErr("order", "items %q and %q (%s) share order %d; sibling orders must be unique", other, s.id, where, s.order)
			}
			byOrder[s.order] = s.id
		}
	}
	return nil
}

// validateArchiveEntry checks that an entry is exactly one coherent subtree
// rooted at RootID.
func validateArchiveEntry(entry domain.RoadmapArchiveEntry) error {
	if strings.TrimSpace(entry.RootID) == "" {
		return validationErr("archive", "archive entry has an empty root_id")
	}
	if entry.ArchivedAt.IsZero() {
		return validationErr("archive", "archive entry %q has no archived_at timestamp", entry.RootID)
	}
	byID := make(map[string]domain.RoadmapItem, len(entry.Items))
	for _, item := range entry.Items {
		if _, dup := byID[item.ID]; dup {
			return validationErr("archive", "archive entry %q contains item %q more than once", entry.RootID, item.ID)
		}
		byID[item.ID] = item
	}
	root, ok := byID[entry.RootID]
	if !ok {
		return validationErr("archive", "archive entry %q does not contain its root item", entry.RootID)
	}
	if root.ParentID != nil {
		return validationErr("archive", "archive entry %q: root item must have a null parent_id inside the snapshot", entry.RootID)
	}
	if utf8.RuneCountInString(entry.Summary) > maxSummaryLength {
		return validationErr("archive", "archive entry %q summary is %d characters, max %d; shorten it", entry.RootID, utf8.RuneCountInString(entry.Summary), maxSummaryLength)
	}
	for _, item := range entry.Items {
		if item.ID == entry.RootID {
			continue
		}
		if item.ParentID == nil {
			return validationErr("archive", "archive entry %q: item %q has no parent but is not the root", entry.RootID, item.ID)
		}
		parentItem, ok := byID[*item.ParentID]
		if !ok {
			return validationErr("archive", "archive entry %q: item %q references parent %q outside the snapshot", entry.RootID, item.ID, *item.ParentID)
		}
		if err := validateKindNesting(parentItem, item); err != nil {
			return validationErr("archive", "archive entry %q: %s", entry.RootID, err.Error())
		}
	}
	if cycle := findCycle(entry.Items, func(item domain.RoadmapItem) []string {
		if item.ParentID == nil {
			return nil
		}
		return []string{*item.ParentID}
	}); cycle != nil {
		return validationErr("archive", "archive entry %q: parent cycle: %s", entry.RootID, strings.Join(cycle, " -> "))
	}
	// Connectivity to the root follows from the checks above: every non-root
	// item has an in-snapshot parent, only the root has a null parent, and
	// there are no cycles, so every parent chain terminates at the root.
	if err := validateSiblingOrders(entry.Items); err != nil {
		return validationErr("archive", "archive entry %q: %s", entry.RootID, err.Error())
	}
	return nil
}

// findCycle runs an iterative tri-color DFS over the item graph defined by
// edges and returns one cycle path (ending where it started), or nil. Edges
// pointing at IDs outside the item set are ignored (reference validity is
// checked separately).
func findCycle(items []domain.RoadmapItem, edges func(domain.RoadmapItem) []string) []string {
	byID := make(map[string]domain.RoadmapItem, len(items))
	ids := make([]string, 0, len(items))
	for _, item := range items {
		byID[item.ID] = item
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(items))
	parent := make(map[string]string)

	for _, start := range ids {
		if color[start] != white {
			continue
		}
		type frame struct {
			id   string
			next int
		}
		stack := []frame{{id: start}}
		color[start] = gray
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			targets := edges(byID[top.id])
			advanced := false
			for top.next < len(targets) {
				target := targets[top.next]
				top.next++
				if _, ok := byID[target]; !ok {
					continue
				}
				switch color[target] {
				case white:
					color[target] = gray
					parent[target] = top.id
					stack = append(stack, frame{id: target})
					advanced = true
				case gray:
					// Found a cycle: walk back from top.id to target, then
					// reverse so the path reads in edge direction and ends
					// where it starts (e.g. a -> b -> c -> a).
					path := []string{target}
					for at := top.id; ; at = parent[at] {
						path = append(path, at)
						if at == target {
							break
						}
					}
					for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
						path[i], path[j] = path[j], path[i]
					}
					return path
				case black:
				}
				if advanced {
					break
				}
			}
			if !advanced {
				color[top.id] = black
				stack = stack[:len(stack)-1]
			}
		}
	}
	return nil
}
