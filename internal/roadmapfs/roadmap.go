// Package roadmapfs stores each architect's structured roadmap as a pair of
// YAML files inside the architect workspace: roadmap/current.yaml (the live
// plan) and roadmap/archive.yaml (recoverable archived subtrees). The pair is
// one logical document for concurrency: a single opaque version token covers
// both files, and every update validates the complete proposed state before
// writing either file atomically.
package roadmapfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/hiveryn/daemon/internal/domain"
)

const (
	roadmapDirName  = "roadmap"
	currentFileName = "current.yaml"
	archiveFileName = "archive.yaml"

	viewCurrent = "current"
	viewArchive = "archive"
)

type Service struct {
	tickets domain.TicketService

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewService(tickets domain.TicketService) *Service {
	return &Service{tickets: tickets, locks: make(map[string]*sync.Mutex)}
}

// workspaceLock returns the per-workspace mutex serializing the
// read→validate→write cycle so in-process writers cannot interleave between
// the version check and the renames.
func (s *Service) workspaceLock(architectPath string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[architectPath]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[architectPath] = lock
	}
	return lock
}

// state is the in-memory form of the current+archive pair.
type state struct {
	current domain.Roadmap
	archive domain.RoadmapArchive
}

func roadmapDir(architectPath string) string {
	return filepath.Join(architectPath, roadmapDirName)
}

// hashPair computes the opaque version token over the raw bytes of both
// files. Length prefixes remove boundary ambiguity; a missing file
// contributes empty bytes so the empty roadmap has a stable token.
func hashPair(currentBytes, archiveBytes []byte) string {
	h := sha256.New()
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(currentBytes)))
	h.Write(lenBuf[:])
	h.Write(currentBytes)
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(archiveBytes)))
	h.Write(lenBuf[:])
	h.Write(archiveBytes)
	return hex.EncodeToString(h.Sum(nil))
}

// strictDecodeYAML decodes data rejecting unknown keys, so a misspelled or
// unsupported key fails loudly instead of silently vanishing on the next
// write. It also rejects multi-document files (e.g. a merge-concatenated
// pair) — silently loading only the first document would destroy the rest on
// the next write.
func strictDecodeYAML(data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("file contains no YAML document; delete it or restore valid content")
		}
		return fmt.Errorf("invalid YAML (unknown or misspelled keys are rejected): %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("file contains more than one YAML document (likely a bad merge); keep exactly one document")
	}
	return nil
}

// readOptionalFile reads path, treating a missing file as empty bytes.
func readOptionalFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	return data, nil
}

// loadState reads both files. Missing files yield a valid empty roadmap.
// Parse errors and unsupported schema versions are validation errors naming
// the file. Warnings report duplicate IDs spanning current and archive
// (the signature of an interrupted write; the files are git-tracked YAML and
// hand-repairable).
func (s *Service) loadState(architectPath string) (state, string, []string, error) {
	if architectPath == "" {
		return state{}, "", nil, &domain.ValidationError{Field: "architectPath", Message: "is required"}
	}
	dir := roadmapDir(architectPath)
	currentBytes, err := readOptionalFile(filepath.Join(dir, currentFileName))
	if err != nil {
		return state{}, "", nil, err
	}
	archiveBytes, err := readOptionalFile(filepath.Join(dir, archiveFileName))
	if err != nil {
		return state{}, "", nil, err
	}

	st := state{
		current: domain.Roadmap{SchemaVersion: domain.RoadmapSchemaVersion},
		archive: domain.RoadmapArchive{SchemaVersion: domain.RoadmapSchemaVersion},
	}
	if len(currentBytes) > 0 {
		if err := strictDecodeYAML(currentBytes, &st.current); err != nil {
			return state{}, "", nil, &domain.ValidationError{
				Field:   "roadmap/" + currentFileName,
				Message: err.Error(),
			}
		}
		if st.current.SchemaVersion != domain.RoadmapSchemaVersion {
			return state{}, "", nil, &domain.ValidationError{
				Field:   "roadmap/" + currentFileName,
				Message: fmt.Sprintf("unsupported schema_version %d (want %d)", st.current.SchemaVersion, domain.RoadmapSchemaVersion),
			}
		}
	}
	if len(archiveBytes) > 0 {
		if err := strictDecodeYAML(archiveBytes, &st.archive); err != nil {
			return state{}, "", nil, &domain.ValidationError{
				Field:   "roadmap/" + archiveFileName,
				Message: err.Error(),
			}
		}
		if st.archive.SchemaVersion != domain.RoadmapSchemaVersion {
			return state{}, "", nil, &domain.ValidationError{
				Field:   "roadmap/" + archiveFileName,
				Message: fmt.Sprintf("unsupported schema_version %d (want %d)", st.archive.SchemaVersion, domain.RoadmapSchemaVersion),
			}
		}
	}
	normalizeState(&st)
	return st, hashPair(currentBytes, archiveBytes), crossFileWarnings(st), nil
}

// crossFileWarnings reports item IDs present in both current and the archive.
func crossFileWarnings(st state) []string {
	warnings := []string{}
	currentIDs := make(map[string]bool, len(st.current.Items))
	for _, item := range st.current.Items {
		currentIDs[item.ID] = true
	}
	for _, entry := range st.archive.Entries {
		for _, item := range entry.Items {
			if currentIDs[item.ID] {
				warnings = append(warnings, fmt.Sprintf(
					"item %q exists in both current.yaml and archive entry %q — likely an interrupted write; repair roadmap/current.yaml or roadmap/archive.yaml by hand (they are git-tracked YAML)",
					item.ID, entry.RootID))
			}
		}
	}
	return warnings
}

// normalizeState puts the state into canonical form: non-nil slices, items in
// DFS order (roots by order, then each subtree; siblings by order, ID as a
// total-order tiebreak), archive entries by archive time then root ID. Two
// identical logical states always marshal to identical bytes.
func normalizeState(st *state) {
	st.current.Items = sortItemsDFS(st.current.Items)
	for i := range st.current.Items {
		normalizeItem(&st.current.Items[i])
	}
	if st.current.Items == nil {
		st.current.Items = []domain.RoadmapItem{}
	}
	sort.SliceStable(st.archive.Entries, func(i, j int) bool {
		a, b := st.archive.Entries[i], st.archive.Entries[j]
		if !a.ArchivedAt.Equal(b.ArchivedAt) {
			return a.ArchivedAt.Before(b.ArchivedAt)
		}
		return a.RootID < b.RootID
	})
	for e := range st.archive.Entries {
		entry := &st.archive.Entries[e]
		entry.ArchivedAt = entry.ArchivedAt.UTC()
		entry.Items = sortItemsDFS(entry.Items)
		for i := range entry.Items {
			normalizeItem(&entry.Items[i])
		}
		if entry.Items == nil {
			entry.Items = []domain.RoadmapItem{}
		}
	}
	if st.archive.Entries == nil {
		st.archive.Entries = []domain.RoadmapArchiveEntry{}
	}
}

func normalizeItem(item *domain.RoadmapItem) {
	if item.ParentID != nil && *item.ParentID == "" {
		item.ParentID = nil
	}
	if item.SuccessCriteria == nil {
		item.SuccessCriteria = []string{}
	}
	if item.Tickets == nil {
		item.Tickets = []string{}
	}
	if item.DependsOn == nil {
		item.DependsOn = []string{}
	}
}

func parentKey(parentID *string) string {
	if parentID == nil {
		return ""
	}
	return *parentID
}

// sortItemsDFS returns items ordered depth-first: roots by (order, id), then
// each root's subtree recursively with the same sibling ordering. Items whose
// parent is missing from the slice are treated as roots so a partial subtree
// (focused reads, archive snapshots mid-build) still sorts deterministically.
func sortItemsDFS(items []domain.RoadmapItem) []domain.RoadmapItem {
	if len(items) == 0 {
		return items
	}
	present := make(map[string]bool, len(items))
	for _, item := range items {
		present[item.ID] = true
	}
	children := make(map[string][]domain.RoadmapItem)
	for _, item := range items {
		key := parentKey(item.ParentID)
		if key != "" && !present[key] {
			key = ""
		}
		children[key] = append(children[key], item)
	}
	for key := range children {
		group := children[key]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].Order != group[j].Order {
				return group[i].Order < group[j].Order
			}
			return group[i].ID < group[j].ID
		})
	}
	out := make([]domain.RoadmapItem, 0, len(items))
	visited := make(map[string]bool, len(items))
	var walk func(key string)
	walk = func(key string) {
		for _, item := range children[key] {
			if visited[item.ID] {
				continue
			}
			visited[item.ID] = true
			out = append(out, item)
			walk(item.ID)
		}
	}
	walk("")
	// Items unreachable from any root (parent cycles in hand-edited files)
	// are appended by ID so serialization stays total even on invalid input.
	if len(out) < len(items) {
		var rest []domain.RoadmapItem
		for _, item := range items {
			if !visited[item.ID] {
				rest = append(rest, item)
			}
		}
		sort.SliceStable(rest, func(i, j int) bool { return rest[i].ID < rest[j].ID })
		out = append(out, rest...)
	}
	return out
}

// storeState normalizes, marshals, and atomically replaces both files: both
// temp files are fully written and fsynced first, then renamed one after the
// other. The rename order follows the data direction so a crash between the
// two renames can duplicate a subtree across the pair (surfaced as a read
// warning) but never lose pre-existing items: archives rename archive.yaml
// first (the subtree lands there before it leaves current.yaml), restores
// rename current.yaml first (the subtree lands there before its archive entry
// disappears). Mixing both directions in one batch is rejected upstream.
// Returns the new version token computed from the written bytes.
func storeState(architectPath string, st state, renameCurrentFirst bool) (string, error) {
	normalizeState(&st)
	currentBytes, err := yaml.Marshal(st.current)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", currentFileName, err)
	}
	archiveBytes, err := yaml.Marshal(st.archive)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", archiveFileName, err)
	}

	dir := roadmapDir(architectPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %q: %w", dir, err)
	}
	archiveTmp, err := writeTemp(dir, archiveBytes)
	if err != nil {
		return "", err
	}
	currentTmp, err := writeTemp(dir, currentBytes)
	if err != nil {
		_ = os.Remove(archiveTmp)
		return "", err
	}
	renames := []struct{ tmp, name string }{
		{archiveTmp, archiveFileName},
		{currentTmp, currentFileName},
	}
	if renameCurrentFirst {
		renames[0], renames[1] = renames[1], renames[0]
	}
	for i, r := range renames {
		if err := os.Rename(r.tmp, filepath.Join(dir, r.name)); err != nil {
			for _, rest := range renames[i:] {
				_ = os.Remove(rest.tmp)
			}
			return "", fmt.Errorf("rename %q to %s: %w", r.tmp, r.name, err)
		}
	}
	return hashPair(currentBytes, archiveBytes), nil
}

// writeTemp writes data to a new temp file in dir (0600, fsynced) and returns
// its path.
func writeTemp(dir string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".hiveryn-roadmap-*.yaml.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	fail := func(step string, err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("%s temp file %q: %w", step, tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail("chmod", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("close temp file %q: %w", tmpName, err)
	}
	return tmpName, nil
}

// Read returns the requested roadmap view. See domain.RoadmapQuery for the
// view/id/depth semantics.
func (s *Service) Read(ctx context.Context, architectPath string, query domain.RoadmapQuery) (domain.RoadmapView, error) {
	view := query.View
	if view == "" {
		view = viewCurrent
	}
	switch view {
	case viewCurrent, viewArchive:
	default:
		return domain.RoadmapView{}, &domain.ValidationError{Field: "view", Message: "must be current or archive"}
	}
	if query.Depth != nil {
		if view == viewArchive {
			return domain.RoadmapView{}, &domain.ValidationError{Field: "depth", Message: "applies to the current view only"}
		}
		if *query.Depth < 0 {
			return domain.RoadmapView{}, &domain.ValidationError{Field: "depth", Message: "must be >= 0"}
		}
	}

	// Take the workspace lock so a read cannot observe a torn pair between
	// storeState's two renames.
	lock := s.workspaceLock(architectPath)
	lock.Lock()
	st, version, warnings, err := s.loadState(architectPath)
	lock.Unlock()
	if err != nil {
		return domain.RoadmapView{}, err
	}
	result := domain.RoadmapView{
		View:     view,
		Version:  version,
		Items:    []domain.RoadmapItem{},
		Tickets:  []domain.RoadmapTicketInfo{},
		Warnings: warnings,
	}

	switch view {
	case viewCurrent:
		result.Title = st.current.Title
		items := st.current.Items
		if query.ID != "" {
			if !hasItem(items, query.ID) {
				return domain.RoadmapView{}, &domain.NotFoundError{Resource: "roadmap item", ID: query.ID}
			}
			items = subtreeItems(items, query.ID, query.Depth)
		} else if query.Depth != nil {
			items = depthLimitedItems(items, query.Depth)
		}
		result.Items = items
		tickets, ticketWarnings := s.resolveTickets(ctx, architectPath, items)
		result.Tickets = tickets
		result.Warnings = append(result.Warnings, ticketWarnings...)
	case viewArchive:
		if query.ID == "" {
			summaries := make([]domain.RoadmapArchiveEntrySummary, 0, len(st.archive.Entries))
			for _, entry := range st.archive.Entries {
				summaries = append(summaries, summarizeEntry(entry))
			}
			result.ArchiveEntries = summaries
			return result, nil
		}
		entry, ok := findEntry(st.archive, query.ID)
		if !ok {
			return domain.RoadmapView{}, &domain.NotFoundError{Resource: "roadmap archive entry", ID: query.ID}
		}
		result.ArchiveEntry = &entry
		tickets, ticketWarnings := s.resolveTickets(ctx, architectPath, entry.Items)
		result.Tickets = tickets
		result.Warnings = append(result.Warnings, ticketWarnings...)
	}
	return result, nil
}

func hasItem(items []domain.RoadmapItem, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func findEntry(archive domain.RoadmapArchive, rootID string) (domain.RoadmapArchiveEntry, bool) {
	for _, entry := range archive.Entries {
		if entry.RootID == rootID {
			return entry, true
		}
	}
	return domain.RoadmapArchiveEntry{}, false
}

func summarizeEntry(entry domain.RoadmapArchiveEntry) domain.RoadmapArchiveEntrySummary {
	summary := domain.RoadmapArchiveEntrySummary{
		RootID:     entry.RootID,
		ArchivedAt: entry.ArchivedAt,
		Summary:    entry.Summary,
		ItemCount:  len(entry.Items),
	}
	for _, item := range entry.Items {
		if item.ID == entry.RootID {
			summary.RootTitle = item.Title
			summary.RootKind = item.Kind
			break
		}
	}
	return summary
}

// subtreeItems returns the item with rootID plus descendants up to depth
// levels below it (nil = unlimited), in DFS order.
func subtreeItems(items []domain.RoadmapItem, rootID string, depth *int) []domain.RoadmapItem {
	byParent := childIndex(items)
	byID := make(map[string]domain.RoadmapItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	out := []domain.RoadmapItem{byID[rootID]}
	var walk func(id string, level int)
	walk = func(id string, level int) {
		if depth != nil && level >= *depth {
			return
		}
		for _, child := range byParent[id] {
			out = append(out, child)
			walk(child.ID, level+1)
		}
	}
	walk(rootID, 0)
	return out
}

// depthLimitedItems keeps items whose depth from the roots is at most depth
// (0 = roots only), in DFS order.
func depthLimitedItems(items []domain.RoadmapItem, depth *int) []domain.RoadmapItem {
	byParent := childIndex(items)
	out := []domain.RoadmapItem{}
	var walk func(parent string, level int)
	walk = func(parent string, level int) {
		if depth != nil && level > *depth {
			return
		}
		for _, child := range byParent[parent] {
			out = append(out, child)
			walk(child.ID, level+1)
		}
	}
	walk("", 0)
	return out
}

// childIndex groups items by parent (sorted by order then ID), keyed by
// parent ID ("" for roots).
func childIndex(items []domain.RoadmapItem) map[string][]domain.RoadmapItem {
	byParent := make(map[string][]domain.RoadmapItem)
	for _, item := range items {
		byParent[parentKey(item.ParentID)] = append(byParent[parentKey(item.ParentID)], item)
	}
	for key := range byParent {
		group := byParent[key]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].Order != group[j].Order {
				return group[i].Order < group[j].Order
			}
			return group[i].ID < group[j].ID
		})
	}
	return byParent
}
