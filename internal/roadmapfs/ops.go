package roadmapfs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hiveryn/daemon/internal/domain"
)

// Update applies an ordered batch of operations atomically: version check,
// sequential op application, full final-state validation, then one atomic
// write of the pair. The first failing op aborts the batch and nothing is
// written.
func (s *Service) Update(ctx context.Context, architectPath string, params domain.UpdateRoadmapParams) (domain.RoadmapUpdateResult, error) {
	lock := s.workspaceLock(architectPath)
	lock.Lock()
	defer lock.Unlock()

	st, version, loadWarnings, err := s.loadState(architectPath)
	if err != nil {
		return domain.RoadmapUpdateResult{}, err
	}
	if strings.TrimSpace(params.Version) == "" {
		return domain.RoadmapUpdateResult{}, &domain.ValidationError{Field: "version", Message: "is required; read the roadmap first"}
	}
	if !strings.EqualFold(params.Version, version) {
		return domain.RoadmapUpdateResult{}, &domain.ConflictError{
			Resource: "roadmap",
			Field:    "version",
			Message:  "roadmap changed since you read it (version mismatch); re-read with readRoadmap and retry",
		}
	}
	if params.Title == nil && len(params.Ops) == 0 {
		return domain.RoadmapUpdateResult{}, &domain.ValidationError{Field: "ops", Message: "nothing to do: provide at least one operation or a title"}
	}
	// Archive and restore move data in opposite directions across the file
	// pair; a fixed rename order can only make one direction crash-safe, so a
	// batch may not mix them (see storeState).
	hasArchive, hasRestore := false, false
	for _, op := range params.Ops {
		switch op.Type {
		case domain.RoadmapOpArchive:
			hasArchive = true
		case domain.RoadmapOpRestore:
			hasRestore = true
		}
	}
	if hasArchive && hasRestore {
		return domain.RoadmapUpdateResult{}, &domain.ValidationError{Field: "ops", Message: "archive and restore cannot be combined in one batch; send them as separate updates"}
	}
	if params.Title != nil {
		title := strings.TrimSpace(*params.Title)
		if title == "" {
			return domain.RoadmapUpdateResult{}, &domain.ValidationError{Field: "title", Message: "must be non-empty when provided"}
		}
		if utf8.RuneCountInString(title) > maxTitleLength {
			return domain.RoadmapUpdateResult{}, &domain.ValidationError{Field: "title", Message: fmt.Sprintf("is %d characters, max %d; shorten it", utf8.RuneCountInString(title), maxTitleLength)}
		}
		st.current.Title = title
	}

	eng := &engine{st: &st, now: time.Now().UTC().Truncate(time.Second), affected: make(map[string]bool)}
	applied := make([]domain.RoadmapOpResult, 0, len(params.Ops))
	for i, op := range params.Ops {
		result, err := eng.apply(op)
		if err != nil {
			return domain.RoadmapUpdateResult{}, opError(i, op.Type, err)
		}
		result.Index = i
		result.Type = op.Type
		applied = append(applied, result)
		eng.affected[result.ItemID] = true
	}

	if err := validateState(st); err != nil {
		return domain.RoadmapUpdateResult{}, err
	}

	newVersion, err := storeState(architectPath, st, hasRestore)
	if err != nil {
		return domain.RoadmapUpdateResult{}, err
	}

	// Ticket resolution is display-only enrichment: it runs after the write
	// and degrades to warnings, never failing the committed update.
	tickets, ticketWarnings := s.resolveTickets(ctx, architectPath, collectAffectedItems(st, eng.affected))
	warnings := append([]string{}, loadWarnings...)
	warnings = append(warnings, ticketWarnings...)
	return domain.RoadmapUpdateResult{
		Version:  newVersion,
		Applied:  applied,
		Tickets:  tickets,
		Warnings: warnings,
	}, nil
}

// opError prefixes an op-level failure with its batch index so the caller
// can see exactly which operation failed. NotFound errors pass through
// unchanged — their resource+ID is already precise.
func opError(index int, opType domain.RoadmapOpType, err error) error {
	prefix := fmt.Sprintf("ops[%d] (%s)", index, opType)
	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		return &domain.ValidationError{Field: prefix, Message: joinFieldMessage(validationErr.Field, validationErr.Message)}
	}
	var conflictErr *domain.ConflictError
	if errors.As(err, &conflictErr) {
		return &domain.ConflictError{Resource: conflictErr.Resource, Field: prefix, Message: joinFieldMessage(conflictErr.Field, conflictErr.Message)}
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func joinFieldMessage(field, message string) string {
	if field == "" {
		return message
	}
	return field + " " + message
}

func collectAffectedItems(st state, affected map[string]bool) []domain.RoadmapItem {
	items := []domain.RoadmapItem{}
	for _, item := range st.current.Items {
		if affected[item.ID] {
			items = append(items, item)
		}
	}
	for _, entry := range st.archive.Entries {
		if !affected[entry.RootID] {
			continue
		}
		items = append(items, entry.Items...)
	}
	return items
}

// engine applies one operation at a time to the in-memory state. Roadmaps
// are small, so helpers rescan the item slice instead of maintaining indexes.
// affected accumulates the item IDs each op touched, for ticket resolution.
type engine struct {
	st       *state
	now      time.Time
	affected map[string]bool
}

func (e *engine) apply(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	if !op.Type.Valid() {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "type", Message: fmt.Sprintf("unknown operation type %q", op.Type)}
	}
	if strings.TrimSpace(op.ID) == "" {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "id", Message: "is required"}
	}
	switch op.Type {
	case domain.RoadmapOpCreate:
		return e.create(op)
	case domain.RoadmapOpUpdate:
		return e.update(op)
	case domain.RoadmapOpMove:
		return e.move(op)
	case domain.RoadmapOpLinkTicket:
		return e.linkTicket(op)
	case domain.RoadmapOpUnlinkTicket:
		return e.unlinkTicket(op)
	case domain.RoadmapOpArchive:
		return e.archiveSubtree(op)
	case domain.RoadmapOpRestore:
		return e.restore(op)
	default:
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "type", Message: fmt.Sprintf("unhandled operation type %q", op.Type)}
	}
}

func (e *engine) itemIndex(id string) int {
	for i := range e.st.current.Items {
		if e.st.current.Items[i].ID == id {
			return i
		}
	}
	return -1
}

// idInUse reports whether id names a current item or any archived item.
func (e *engine) idInUse(id string) (string, bool) {
	if e.itemIndex(id) >= 0 {
		return "the current roadmap", true
	}
	for _, entry := range e.st.archive.Entries {
		for _, item := range entry.Items {
			if item.ID == id {
				return fmt.Sprintf("archive entry %q", entry.RootID), true
			}
		}
	}
	return "", false
}

// normalizeParentArg maps an op's parent argument to the stored form:
// nil or empty string mean top-level (nil).
func normalizeParentArg(parentID *string) *string {
	if parentID == nil || strings.TrimSpace(*parentID) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*parentID)
	return &trimmed
}

// checkDependencies validates a replacement depends_on list for itemID
// (which must already be in the current items): deduplicated
// (order-preserving), no self-dependency, every target a current item, no
// dependency on the item's own ancestors or descendants (the parent chain
// already implies that ordering). Cycle detection happens in final-state
// validation.
func (e *engine) checkDependencies(itemID string, deps []string) ([]string, error) {
	out := dedupeStrings(deps)
	ancestors := e.ancestorIDs(itemID)
	descendants := e.descendantIDs(itemID)
	for _, dep := range out {
		if dep == itemID {
			return nil, &domain.ValidationError{Field: "depends_on", Message: fmt.Sprintf("item %q cannot depend on itself", itemID)}
		}
		if e.itemIndex(dep) < 0 {
			return nil, &domain.ValidationError{Field: "depends_on", Message: fmt.Sprintf("%q is not a current roadmap item", dep)}
		}
		if ancestors[dep] {
			return nil, &domain.ValidationError{Field: "depends_on", Message: fmt.Sprintf("%q is an ancestor of %q; the parent chain already implies that ordering — remove the dependency", dep, itemID)}
		}
		if descendants[dep] {
			return nil, &domain.ValidationError{Field: "depends_on", Message: fmt.Sprintf("%q is a descendant of %q; the parent chain already implies that ordering — remove the dependency", dep, itemID)}
		}
	}
	return out, nil
}

// ancestorIDs returns the IDs on the item's parent chain among the current
// items. A visited guard keeps it terminating even mid-batch.
func (e *engine) ancestorIDs(itemID string) map[string]bool {
	out := map[string]bool{}
	idx := e.itemIndex(itemID)
	for idx >= 0 {
		parent := e.st.current.Items[idx].ParentID
		if parent == nil || out[*parent] {
			break
		}
		out[*parent] = true
		idx = e.itemIndex(*parent)
	}
	return out
}

// checkKindNesting rejects a child kind ranking above its parent's kind
// (hierarchy order: goal > initiative > milestone).
func checkKindNesting(parentID string, parentKind domain.RoadmapItemKind, childID string, childKind domain.RoadmapItemKind) error {
	if kindRank(childKind) < kindRank(parentKind) {
		return &domain.ValidationError{
			Field:   "parent_id",
			Message: fmt.Sprintf("item %q (%s) cannot nest under %q (%s); %s", childID, childKind, parentID, parentKind, kindNestingHint),
		}
	}
	return nil
}

func dedupeStrings(values []string) []string {
	out := []string{}
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// placeInGroup positions the item (already parented) within its sibling
// group and renumbers the group to 10, 20, 30… A requested order value
// places the item before any sibling with an equal or higher order; omitted
// order appends.
func (e *engine) placeInGroup(id string, requestedOrder *int) {
	idx := e.itemIndex(id)
	item := &e.st.current.Items[idx]
	siblings := []int{}
	for i := range e.st.current.Items {
		if i == idx {
			continue
		}
		if parentKey(e.st.current.Items[i].ParentID) == parentKey(item.ParentID) {
			siblings = append(siblings, i)
		}
	}
	sort.SliceStable(siblings, func(a, b int) bool {
		ia, ib := e.st.current.Items[siblings[a]], e.st.current.Items[siblings[b]]
		if ia.Order != ib.Order {
			return ia.Order < ib.Order
		}
		return ia.ID < ib.ID
	})
	pos := len(siblings)
	if requestedOrder != nil {
		pos = 0
		for _, i := range siblings {
			if e.st.current.Items[i].Order < *requestedOrder {
				pos++
			}
		}
	}
	sequence := make([]int, 0, len(siblings)+1)
	sequence = append(sequence, siblings[:pos]...)
	sequence = append(sequence, idx)
	sequence = append(sequence, siblings[pos:]...)
	for seq, i := range sequence {
		e.st.current.Items[i].Order = (seq + 1) * 10
	}
}

// renumberGroup renumbers the sibling group under parentID to 10, 20, 30…
// preserving relative order.
func (e *engine) renumberGroup(parentID *string) {
	group := []int{}
	for i := range e.st.current.Items {
		if parentKey(e.st.current.Items[i].ParentID) == parentKey(parentID) {
			group = append(group, i)
		}
	}
	sort.SliceStable(group, func(a, b int) bool {
		ia, ib := e.st.current.Items[group[a]], e.st.current.Items[group[b]]
		if ia.Order != ib.Order {
			return ia.Order < ib.Order
		}
		return ia.ID < ib.ID
	})
	for seq, i := range group {
		e.st.current.Items[i].Order = (seq + 1) * 10
	}
}

// descendantIDs returns the IDs of rootID's transitive children among the
// current items (excluding rootID itself).
func (e *engine) descendantIDs(rootID string) map[string]bool {
	byParent := make(map[string][]string)
	for _, item := range e.st.current.Items {
		key := parentKey(item.ParentID)
		byParent[key] = append(byParent[key], item.ID)
	}
	out := make(map[string]bool)
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range byParent[id] {
			if !out[child] {
				out[child] = true
				queue = append(queue, child)
			}
		}
	}
	return out
}

func (e *engine) create(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	if !itemIDPattern.MatchString(op.ID) || len(op.ID) > maxItemIDLength {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "id", Message: fmt.Sprintf("%q must be kebab-case ([a-z0-9]+(-[a-z0-9]+)*), at most %d characters", op.ID, maxItemIDLength)}
	}
	if location, used := e.idInUse(op.ID); used {
		return domain.RoadmapOpResult{}, &domain.ConflictError{Resource: "roadmap item", Field: "id", Message: fmt.Sprintf("%q is already used in %s; item IDs stay reserved even after archiving", op.ID, location)}
	}
	if !op.Kind.Valid() {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "kind", Message: "must be goal, initiative, or milestone"}
	}
	if op.Title == nil || strings.TrimSpace(*op.Title) == "" {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "title", Message: "is required and must be non-empty"}
	}
	title := strings.TrimSpace(*op.Title)
	if utf8.RuneCountInString(title) > maxTitleLength {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "title", Message: fmt.Sprintf("is %d characters, max %d; shorten it", utf8.RuneCountInString(title), maxTitleLength)}
	}
	if op.Outcome == nil || strings.TrimSpace(*op.Outcome) == "" {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "outcome", Message: "is required and must be non-empty"}
	}
	outcome := strings.TrimSpace(*op.Outcome)
	if utf8.RuneCountInString(outcome) > maxOutcomeLength {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "outcome", Message: fmt.Sprintf("is %d characters, max %d; shorten it (it is an intended result, not an implementation checklist)", utf8.RuneCountInString(outcome), maxOutcomeLength)}
	}
	status := domain.RoadmapStatusPlanned
	if op.Status != nil {
		status = *op.Status
		if !status.Valid() {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "status", Message: "must be planned, active, blocked, or done"}
		}
	}
	parent := normalizeParentArg(op.ParentID)
	if parent != nil {
		parentIdx := e.itemIndex(*parent)
		if parentIdx < 0 {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "parent_id", Message: fmt.Sprintf("%q is not a current roadmap item", *parent)}
		}
		if err := checkKindNesting(*parent, e.st.current.Items[parentIdx].Kind, op.ID, op.Kind); err != nil {
			return domain.RoadmapOpResult{}, err
		}
	}
	item := domain.RoadmapItem{
		ID:              op.ID,
		Kind:            op.Kind,
		Title:           title,
		Status:          status,
		Outcome:         outcome,
		ParentID:        parent,
		SuccessCriteria: []string{},
		Tickets:         []string{},
		DependsOn:       []string{},
	}
	if op.SuccessCriteria != nil {
		item.SuccessCriteria = append([]string{}, *op.SuccessCriteria...)
	}
	// Append before checking dependencies so the ancestor/descendant checks
	// see the new item's position in the hierarchy.
	e.st.current.Items = append(e.st.current.Items, item)
	if op.DependsOn != nil {
		deps, err := e.checkDependencies(op.ID, *op.DependsOn)
		if err != nil {
			return domain.RoadmapOpResult{}, err
		}
		e.st.current.Items[e.itemIndex(op.ID)].DependsOn = deps
	}
	e.placeInGroup(op.ID, op.Order)
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("created %s under %s", op.Kind, describeParent(parent))}, nil
}

func describeParent(parentID *string) string {
	if parentID == nil {
		return "the top level"
	}
	return fmt.Sprintf("%q", *parentID)
}

func (e *engine) update(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	idx := e.itemIndex(op.ID)
	if idx < 0 {
		return domain.RoadmapOpResult{}, &domain.NotFoundError{Resource: "roadmap item", ID: op.ID}
	}
	item := &e.st.current.Items[idx]
	changed := []string{}
	if op.Title != nil {
		title := strings.TrimSpace(*op.Title)
		if title == "" {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "title", Message: "must be non-empty"}
		}
		if utf8.RuneCountInString(title) > maxTitleLength {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "title", Message: fmt.Sprintf("is %d characters, max %d; shorten it", utf8.RuneCountInString(title), maxTitleLength)}
		}
		item.Title = title
		changed = append(changed, "title")
	}
	if op.Status != nil {
		if !op.Status.Valid() {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "status", Message: "must be planned, active, blocked, or done"}
		}
		item.Status = *op.Status
		changed = append(changed, "status")
	}
	if op.Outcome != nil {
		outcome := strings.TrimSpace(*op.Outcome)
		if outcome == "" {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "outcome", Message: "must be non-empty"}
		}
		if utf8.RuneCountInString(outcome) > maxOutcomeLength {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "outcome", Message: fmt.Sprintf("is %d characters, max %d; shorten it (it is an intended result, not an implementation checklist)", utf8.RuneCountInString(outcome), maxOutcomeLength)}
		}
		item.Outcome = outcome
		changed = append(changed, "outcome")
	}
	if op.SuccessCriteria != nil {
		item.SuccessCriteria = append([]string{}, *op.SuccessCriteria...)
		changed = append(changed, "success_criteria")
	}
	if op.DependsOn != nil {
		deps, err := e.checkDependencies(op.ID, *op.DependsOn)
		if err != nil {
			return domain.RoadmapOpResult{}, err
		}
		item.DependsOn = deps
		changed = append(changed, "depends_on")
	}
	if len(changed) == 0 {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "update", Message: "no fields to update; provide title, status, outcome, success_criteria, or depends_on"}
	}
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: "updated " + strings.Join(changed, ", ")}, nil
}

func (e *engine) move(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	idx := e.itemIndex(op.ID)
	if idx < 0 {
		return domain.RoadmapOpResult{}, &domain.NotFoundError{Resource: "roadmap item", ID: op.ID}
	}
	// ParentID tri-state: omitted keeps the current parent (pure reorder),
	// explicit empty string moves to the top level.
	parent := e.st.current.Items[idx].ParentID
	if op.ParentID != nil {
		parent = normalizeParentArg(op.ParentID)
	}
	if parent != nil {
		if *parent == op.ID {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "parent_id", Message: fmt.Sprintf("cannot move %q under itself", op.ID)}
		}
		if e.itemIndex(*parent) < 0 {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "parent_id", Message: fmt.Sprintf("%q is not a current roadmap item", *parent)}
		}
		if e.descendantIDs(op.ID)[*parent] {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "parent_id", Message: fmt.Sprintf("cannot move %q under its own descendant %q", op.ID, *parent)}
		}
		parentIdx := e.itemIndex(*parent)
		if err := checkKindNesting(*parent, e.st.current.Items[parentIdx].Kind, op.ID, e.st.current.Items[idx].Kind); err != nil {
			return domain.RoadmapOpResult{}, err
		}
	}
	oldParent := e.st.current.Items[idx].ParentID
	e.st.current.Items[idx].ParentID = parent
	if parentKey(oldParent) != parentKey(parent) {
		e.renumberGroup(oldParent)
	}
	e.placeInGroup(op.ID, op.Order)
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("moved under %s", describeParent(parent))}, nil
}

func (e *engine) linkTicket(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	idx := e.itemIndex(op.ID)
	if idx < 0 {
		return domain.RoadmapOpResult{}, &domain.NotFoundError{Resource: "roadmap item", ID: op.ID}
	}
	ticketID := strings.TrimSpace(op.TicketID)
	if ticketID == "" {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "ticket_id", Message: "is required"}
	}
	if !ticketIDPattern.MatchString(ticketID) {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "ticket_id", Message: fmt.Sprintf("%q %s", ticketID, ticketIDShapeHint)}
	}
	item := &e.st.current.Items[idx]
	if slices.Contains(item.Tickets, ticketID) {
		return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("ticket %q already linked (no-op)", ticketID)}, nil
	}
	item.Tickets = append(item.Tickets, ticketID)
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("linked ticket %q", ticketID)}, nil
}

func (e *engine) unlinkTicket(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	idx := e.itemIndex(op.ID)
	if idx < 0 {
		return domain.RoadmapOpResult{}, &domain.NotFoundError{Resource: "roadmap item", ID: op.ID}
	}
	ticketID := strings.TrimSpace(op.TicketID)
	if ticketID == "" {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "ticket_id", Message: "is required"}
	}
	item := &e.st.current.Items[idx]
	kept := item.Tickets[:0]
	removed := false
	for _, existing := range item.Tickets {
		if existing == ticketID {
			removed = true
			continue
		}
		kept = append(kept, existing)
	}
	item.Tickets = kept
	if !removed {
		return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("ticket %q was not linked (no-op)", ticketID)}, nil
	}
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("unlinked ticket %q", ticketID)}, nil
}

func (e *engine) archiveSubtree(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	idx := e.itemIndex(op.ID)
	if idx < 0 {
		return domain.RoadmapOpResult{}, &domain.NotFoundError{Resource: "roadmap item", ID: op.ID}
	}
	subtree := e.descendantIDs(op.ID)
	subtree[op.ID] = true

	// Remaining items must not keep parent or dependency links into the
	// removed subtree.
	conflicts := []string{}
	for _, item := range e.st.current.Items {
		if subtree[item.ID] {
			continue
		}
		if item.ParentID != nil && subtree[*item.ParentID] {
			conflicts = append(conflicts, fmt.Sprintf("%q has parent %q", item.ID, *item.ParentID))
		}
		for _, dep := range item.DependsOn {
			if subtree[dep] {
				conflicts = append(conflicts, fmt.Sprintf("%q depends on %q", item.ID, dep))
			}
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return domain.RoadmapOpResult{}, &domain.ConflictError{
			Resource: "roadmap item",
			Field:    "archive",
			Message: fmt.Sprintf("cannot archive %q: remaining items still link into the subtree (%s); move or unlink them, or archive them first",
				op.ID, strings.Join(conflicts, "; ")),
		}
	}

	summary := strings.TrimSpace(op.Summary)
	if utf8.RuneCountInString(summary) > maxSummaryLength {
		return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "summary", Message: fmt.Sprintf("is %d characters, max %d; shorten it", utf8.RuneCountInString(summary), maxSummaryLength)}
	}
	root := e.st.current.Items[idx]
	entry := domain.RoadmapArchiveEntry{
		RootID:           op.ID,
		ArchivedAt:       e.now,
		Summary:          summary,
		OriginalParentID: root.ParentID,
		OriginalOrder:    root.Order,
	}
	snapshot := []domain.RoadmapItem{}
	remaining := []domain.RoadmapItem{}
	for _, item := range e.st.current.Items {
		if subtree[item.ID] {
			snapshot = append(snapshot, item)
		} else {
			remaining = append(remaining, item)
		}
	}
	for i := range snapshot {
		if snapshot[i].ID == op.ID {
			snapshot[i].ParentID = nil
		}
	}
	entry.Items = sortItemsDFS(snapshot)
	e.st.archive.Entries = append(e.st.archive.Entries, entry)
	e.st.current.Items = remaining
	e.renumberGroup(root.ParentID)
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("archived %d item(s)", len(entry.Items))}, nil
}

func (e *engine) restore(op domain.RoadmapOp) (domain.RoadmapOpResult, error) {
	entryIdx := -1
	for i := range e.st.archive.Entries {
		if e.st.archive.Entries[i].RootID == op.ID {
			entryIdx = i
			break
		}
	}
	if entryIdx < 0 {
		return domain.RoadmapOpResult{}, &domain.NotFoundError{Resource: "roadmap archive entry", ID: op.ID}
	}
	entry := e.st.archive.Entries[entryIdx]

	parent := entry.OriginalParentID
	if op.ParentID != nil {
		parent = normalizeParentArg(op.ParentID)
	}
	if parent != nil && e.itemIndex(*parent) < 0 {
		if op.ParentID != nil {
			return domain.RoadmapOpResult{}, &domain.ValidationError{Field: "parent_id", Message: fmt.Sprintf("%q is not a current roadmap item", *parent)}
		}
		return domain.RoadmapOpResult{}, &domain.ValidationError{
			Field:   "parent_id",
			Message: fmt.Sprintf("original parent %q no longer exists in the current roadmap; pass parent_id on the restore op (empty string for top-level)", *parent),
		}
	}
	if parent != nil {
		var rootKind domain.RoadmapItemKind
		for _, item := range entry.Items {
			if item.ID == entry.RootID {
				rootKind = item.Kind
				break
			}
		}
		parentIdx := e.itemIndex(*parent)
		if err := checkKindNesting(*parent, e.st.current.Items[parentIdx].Kind, entry.RootID, rootKind); err != nil {
			return domain.RoadmapOpResult{}, err
		}
	}

	snapshotIDs := make(map[string]bool, len(entry.Items))
	for _, item := range entry.Items {
		if e.itemIndex(item.ID) >= 0 {
			return domain.RoadmapOpResult{}, &domain.ConflictError{
				Resource: "roadmap item",
				Field:    "id",
				Message:  fmt.Sprintf("cannot restore %q: item %q already exists in the current roadmap", op.ID, item.ID),
			}
		}
		snapshotIDs[item.ID] = true
	}
	unresolved := []string{}
	for _, item := range entry.Items {
		for _, dep := range item.DependsOn {
			if !snapshotIDs[dep] && e.itemIndex(dep) < 0 {
				unresolved = append(unresolved, fmt.Sprintf("%q depends on %q", item.ID, dep))
			}
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return domain.RoadmapOpResult{}, &domain.ValidationError{
			Field: "depends_on",
			Message: fmt.Sprintf("cannot restore %q: dependencies are not in the current roadmap or the restored subtree (%s); restore or recreate them first, or update the archived items after restoring elsewhere",
				op.ID, strings.Join(unresolved, "; ")),
		}
	}

	restored := make([]domain.RoadmapItem, len(entry.Items))
	copy(restored, entry.Items)
	for i := range restored {
		if restored[i].ID == op.ID {
			restored[i].ParentID = parent
		}
		// The whole snapshot returns to current; mark every item affected so
		// descendants' ticket links are resolved (and their warnings kept).
		e.affected[restored[i].ID] = true
	}
	e.st.current.Items = append(e.st.current.Items, restored...)
	e.st.archive.Entries = append(e.st.archive.Entries[:entryIdx], e.st.archive.Entries[entryIdx+1:]...)

	order := op.Order
	if order == nil {
		originalOrder := entry.OriginalOrder
		order = &originalOrder
	}
	e.placeInGroup(op.ID, order)
	return domain.RoadmapOpResult{ItemID: op.ID, Detail: fmt.Sprintf("restored %d item(s) under %s", len(restored), describeParent(parent))}, nil
}
