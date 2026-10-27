package architectfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/contextref"
	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

const (
	ticketsDirName     = "tickets"
	ticketFileName     = "ticket.md"
	conclusionFileName = "conclusion.md"

	warningDoneWithoutConclusion = "DONE_WITHOUT_CONCLUSION"
	warningConclusionOutsideDone = "CONCLUSION_OUTSIDE_DONE"
	warningBrokenReference       = "BROKEN_REFERENCE"
	// WarningInvalidReference is an entry that is not a valid reference for
	// this architect (syntax, unknown repo key, duplicate, symlink escape).
	// warningMissingReference is confirmed absence where the target lives;
	// warningUnverifiedReference a target that could not be inspected there
	// (unreachable machine, permission), whose existence is unknown.
	WarningInvalidReference    = "INVALID_REFERENCE"
	warningInvalidReference    = WarningInvalidReference
	warningMissingReference    = "MISSING_REFERENCE"
	warningUnverifiedReference = "UNVERIFIED_REFERENCE"
)

// TicketService owns ticket files in an architect workspace. Each context
// reference is resolved where its own target lives — the workspace on this
// machine, a repo on its configured machine — as located by locate.
type TicketService struct {
	protection    *Protection
	locate        ScopeLocator
	inspectRemote remoteInspector
	now           func() time.Time
	waitBudget    time.Duration
	cache         remoteCache

	inflightMu        sync.Mutex
	inflight          map[string]*inspection
	background        sync.WaitGroup
	referencesChanged func(architectPath string)
}

func NewTicketService(locate ScopeLocator) *TicketService {
	return &TicketService{locate: locate, inspectRemote: inspectRemoteItems, now: time.Now, waitBudget: remoteWaitBudget}
}

func (s *TicketService) ListTickets(ctx context.Context, architectPath string) (domain.TicketBoard, error) {
	return s.listBoard(ctx, architectPath, func(*ticketEntry) bool { return true })
}

// SearchTickets keeps the tickets whose title, body or conclusion text
// contains query, compared case-insensitively. Matches stay in their status
// column in listing order; nothing is ranked. A blank query is no filter.
func (s *TicketService) SearchTickets(ctx context.Context, architectPath, query string) (domain.TicketBoard, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return s.ListTickets(ctx, architectPath)
	}
	return s.listBoard(ctx, architectPath, func(entry *ticketEntry) bool { return entry.matches(needle) })
}

// listBoard scans the workspace and builds the board from the entries keep
// accepts, resolving references for those only.
func (s *TicketService) listBoard(ctx context.Context, architectPath string, keep func(*ticketEntry) bool) (domain.TicketBoard, error) {
	entries, err := scanTickets(architectPath)
	if err != nil {
		return domain.TicketBoard{}, err
	}
	if s.protection != nil {
		_, diags, e := s.protection.Inspect(ctx, architectPath)
		if e != nil {
			return domain.TicketBoard{}, e
		}
		for status, items := range entries {
			for i := range items {
				for _, d := range diags {
					if strings.Contains(d.Path, "/"+items[i].id+"/") {
						items[i].warnings = append(items[i].warnings, domain.TicketWarning{Code: d.Code, Message: d.Path + ": " + d.Message})
					}
				}
			}
			entries[status] = items
		}
	}
	kept := map[domain.TicketStatus][]*ticketEntry{}
	var targets []*ticketEntry
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		kept[status] = make([]*ticketEntry, 0, len(entries[status]))
		for i := range entries[status] {
			entry := &entries[status][i]
			if !keep(entry) {
				continue
			}
			kept[status] = append(kept[status], entry)
			targets = append(targets, entry)
		}
	}
	s.resolveReferences(ctx, architectPath, entries, targets)

	board := domain.TicketBoard{
		Backlog:  make([]domain.TicketSummary, 0, len(kept[domain.TicketStatusBacklog])),
		Progress: make([]domain.TicketSummary, 0, len(kept[domain.TicketStatusProgress])),
		Done:     make([]domain.TicketSummary, 0, len(kept[domain.TicketStatusDone])),
	}
	for _, item := range kept[domain.TicketStatusBacklog] {
		board.Backlog = append(board.Backlog, item.summary())
	}
	for _, item := range kept[domain.TicketStatusProgress] {
		board.Progress = append(board.Progress, item.summary())
	}
	for _, item := range kept[domain.TicketStatusDone] {
		board.Done = append(board.Done, item.summary())
	}
	return board, nil
}

// matches reports whether needle (already lower-cased and trimmed) occurs in
// the ticket's title, body or conclusion body. A ticket without a conclusion
// is matched on title and body alone.
func (e *ticketEntry) matches(needle string) bool {
	if strings.Contains(strings.ToLower(e.metadata.Title), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(e.document.Body), needle) {
		return true
	}
	return e.conclusion != nil && strings.Contains(strings.ToLower(e.conclusion.Body), needle)
}

func (s *TicketService) GetTicket(ctx context.Context, architectPath, id string) (domain.Ticket, error) {
	ticket, err := s.loadTicket(ctx, architectPath, id)
	if err != nil {
		return ticket, err
	}
	if s.protection != nil {
		_, diags, e := s.protection.Inspect(ctx, architectPath)
		if e != nil {
			return domain.Ticket{}, e
		}
		for _, d := range diags {
			if strings.Contains(d.Path, "/"+id+"/") {
				ticket.Warnings = append(ticket.Warnings, domain.TicketWarning{Code: d.Code, Message: d.Path + ": " + d.Message})
			}
		}
	}
	return ticket, nil
}

// loadTicket reads one ticket with its references resolved. Only this
// ticket's paths are inspected; the rest of the board supplies ticket IDs.
func (s *TicketService) loadTicket(ctx context.Context, architectPath, id string) (domain.Ticket, error) {
	entries, err := scanTickets(architectPath)
	if err != nil {
		return domain.Ticket{}, err
	}
	entry, err := findTicketEntry(entries, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	s.resolveReferences(ctx, architectPath, entries, []*ticketEntry{&entry})
	return entry.ticket(), nil
}

func (s *TicketService) createTicket(ctx context.Context, architectPath string, params domain.CreateTicketParams) (domain.Ticket, error) {
	if strings.TrimSpace(params.Repo) == "" {
		return domain.Ticket{}, &domain.ValidationError{Field: "repo", Message: "is required"}
	}
	if strings.TrimSpace(params.Title) == "" {
		return domain.Ticket{}, &domain.ValidationError{Field: "title", Message: "is required"}
	}

	now := params.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	references, err := normalizeReferences(architectPath, params.References)
	if err != nil {
		return domain.Ticket{}, err
	}

	status := domain.TicketStatusBacklog
	id, err := nextTicketID(architectPath, now, params.Title)
	if err != nil {
		return domain.Ticket{}, err
	}
	dir := ticketDir(architectPath, status, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return domain.Ticket{}, fmt.Errorf("create ticket directory: %w", err)
	}

	doc := newTicketDocument(ticketMetadata{
		Title:           strings.TrimSpace(params.Title),
		Repo:            strings.TrimSpace(params.Repo),
		AdditionalRepos: normalizeRepoKeys(params.AdditionalRepos),
		Created:         &now,
		Updated:         &now,
		References:      references,
	}, params.Body)
	if err := writeMarkdownDocument(filepath.Join(dir, ticketFileName), doc); err != nil {
		return domain.Ticket{}, err
	}

	return s.loadTicket(ctx, architectPath, id)
}

func (s *TicketService) editTicket(ctx context.Context, architectPath, id string, params domain.EditTicketParams) (domain.Ticket, error) {
	if params.OldString == "" {
		return domain.Ticket{}, &domain.ValidationError{Field: "oldString", Message: "cannot be empty"}
	}
	if params.OldString == params.NewString {
		return domain.Ticket{}, &domain.ValidationError{Field: "newString", Message: "must differ from oldString"}
	}

	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	if entry.status != domain.TicketStatusBacklog {
		return domain.Ticket{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog to edit"}
	}

	matches := findBodyEditMatches(entry.document.Body, params.OldString)
	if len(matches) == 0 {
		return domain.Ticket{}, &domain.ValidationError{Field: "oldString", Message: "could not find the target text in the ticket body"}
	}
	if len(matches) > 1 && !params.ReplaceAll {
		return domain.Ticket{}, &domain.ValidationError{Field: "oldString", Message: "matched multiple locations; use replaceAll=true or provide more surrounding context"}
	}

	entry.document.Body = applyBodyEditMatches(entry.document.Body, matches, params.NewString, params.ReplaceAll)
	now := params.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	setNodeTime(entry.document.Metadata, "updated_at", now)

	if err := writeMarkdownDocument(filepath.Join(entry.dir, ticketFileName), entry.document); err != nil {
		return domain.Ticket{}, err
	}

	return s.loadTicket(ctx, architectPath, id)
}

func (s *TicketService) updateTicketMetadata(ctx context.Context, architectPath, id string, params domain.UpdateTicketMetadataParams) (domain.Ticket, error) {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	if entry.status != domain.TicketStatusBacklog {
		return domain.Ticket{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog to update metadata"}
	}

	if params.Title != nil {
		title := strings.TrimSpace(*params.Title)
		if title == "" {
			return domain.Ticket{}, &domain.ValidationError{Field: "title", Message: "is required"}
		}
		setNodeString(entry.document.Metadata, "title", title)
	}

	if params.Repo != nil {
		repo := strings.TrimSpace(*params.Repo)
		if repo == "" {
			removeMappingValue(entry.document.Metadata, "repo")
		} else {
			setNodeString(entry.document.Metadata, "repo", repo)
		}
	}
	if params.AdditionalRepos != nil {
		setNodeStrings(entry.document.Metadata, "additional_repos", normalizeRepoKeys(*params.AdditionalRepos))
	}

	if params.References != nil {
		references, err := normalizeReferences(architectPath, *params.References)
		if err != nil {
			return domain.Ticket{}, err
		}
		setNodeStrings(entry.document.Metadata, "references", references)
	}

	now := params.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	setNodeTime(entry.document.Metadata, "updated_at", now)

	if err := writeMarkdownDocument(filepath.Join(entry.dir, ticketFileName), entry.document); err != nil {
		return domain.Ticket{}, err
	}

	return s.loadTicket(ctx, architectPath, id)
}

func (s *TicketService) deleteTicket(_ context.Context, architectPath, id string) error {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return err
	}
	if entry.status != domain.TicketStatusBacklog {
		return &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog to delete"}
	}
	if err := os.RemoveAll(entry.dir); err != nil {
		return fmt.Errorf("remove ticket directory: %w", err)
	}
	return nil
}

func (s *TicketService) concludeTicket(ctx context.Context, architectPath, id string, conclusion domain.TicketConclusion) (domain.Ticket, error) {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	if entry.status != domain.TicketStatusProgress && entry.status != domain.TicketStatusBacklog {
		return domain.Ticket{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog or progress to conclude"}
	}

	doc := newConclusionDocument(conclusion)
	if err := writeMarkdownDocument(filepath.Join(entry.dir, conclusionFileName), doc); err != nil {
		return domain.Ticket{}, err
	}

	now := time.Now().UTC()
	setNodeTime(entry.document.Metadata, "updated_at", now)
	if err := writeMarkdownDocument(filepath.Join(entry.dir, ticketFileName), entry.document); err != nil {
		return domain.Ticket{}, err
	}

	moved, err := s.moveTicket(ctx, architectPath, id, domain.MoveTicketParams{To: domain.TicketStatusDone})
	if err != nil {
		return domain.Ticket{}, err
	}
	return moved, nil
}

func newConclusionDocument(conclusion domain.TicketConclusion) MarkdownDocument {
	meta := newMappingNode()
	setNodeTime(meta, "started_at", conclusion.StartedAt)
	setNodeTime(meta, "concluded_at", conclusion.ConcludedAt)
	if conclusion.Agent != "" {
		setNodeString(meta, "agent", conclusion.Agent)
	}
	if conclusion.Profile != "" {
		setNodeString(meta, "profile", conclusion.Profile)
	}
	setNodeString(meta, "outcome", string(conclusion.Outcome))
	if conclusion.RejectionReason != "" {
		setNodeString(meta, "rejection_reason", conclusion.RejectionReason)
	}
	if len(conclusion.Commits) > 0 {
		setNodeCommitRefs(meta, "commits", conclusion.Commits)
	}
	return MarkdownDocument{Metadata: meta, Body: conclusion.Body}
}

func setNodeCommitRefs(node *yaml.Node, key string, commits []domain.CommitRef) {
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, commit := range commits {
		entry := newMappingNode()
		setNodeString(entry, "sha", commit.SHA)
		setNodeString(entry, "repo", commit.Repo)
		sequence.Content = append(sequence.Content, entry)
	}
	setMappingValue(node, key, sequence)
}

func (s *TicketService) moveTicket(ctx context.Context, architectPath, id string, params domain.MoveTicketParams) (domain.Ticket, error) {
	if !params.To.Valid() {
		return domain.Ticket{}, &domain.ValidationError{Field: "to", Message: "must be backlog, progress, or done"}
	}

	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	if entry.status == params.To {
		return s.loadTicket(ctx, architectPath, id)
	}

	targetDir := ticketDir(architectPath, params.To, id)
	if _, err := os.Stat(targetDir); err == nil {
		return domain.Ticket{}, &domain.ConflictError{Resource: "ticket", Field: "id", Message: id + " already exists in " + string(params.To)}
	} else if !os.IsNotExist(err) {
		return domain.Ticket{}, fmt.Errorf("stat target ticket directory: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetDir), 0o755); err != nil {
		return domain.Ticket{}, fmt.Errorf("create target status directory: %w", err)
	}
	if err := os.Rename(entry.dir, targetDir); err != nil {
		return domain.Ticket{}, fmt.Errorf("move ticket directory: %w", err)
	}

	return s.loadTicket(ctx, architectPath, id)
}

type ticketEntry struct {
	id                 string
	status             domain.TicketStatus
	dir                string
	document           MarkdownDocument
	metadata           ticketMetadata
	conclusion         *domain.TicketConclusion
	warnings           []domain.TicketWarning
	resolvedReferences []domain.TicketReference
}

func (e ticketEntry) summary() domain.TicketSummary {
	return domain.TicketSummary{
		ID:                 e.id,
		Status:             e.status,
		Title:              e.metadata.Title,
		Repo:               e.metadata.Repo,
		AdditionalRepos:    nonNilStrings(e.metadata.AdditionalRepos),
		Created:            e.metadata.Created,
		Updated:            e.metadata.Updated,
		References:         nonNilStrings(e.metadata.References),
		ResolvedReferences: append([]domain.TicketReference{}, e.resolvedReferences...),
		HasConclusion:      e.conclusion != nil,
		Warnings:           cloneWarnings(e.warnings),
	}

}

func (e ticketEntry) ticket() domain.Ticket {
	t := domain.Ticket{
		TicketSummary: e.summary(),
		Body:          e.document.Body,
	}
	if e.conclusion != nil {
		conclusion := *e.conclusion
		conclusion.Commits = nonNilCommitRefs(conclusion.Commits)
		t.Conclusion = &conclusion
	}
	return t
}

var ticketIDPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{4}-.+$`)

func scanTickets(architectPath string) (map[domain.TicketStatus][]ticketEntry, error) {
	entries := map[domain.TicketStatus][]ticketEntry{
		domain.TicketStatusBacklog:  {},
		domain.TicketStatusProgress: {},
		domain.TicketStatusDone:     {},
	}
	seen := map[string]domain.TicketStatus{}
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		statusEntries, err := scanStatusTickets(architectPath, status, seen)
		if err != nil {
			return nil, err
		}
		entries[status] = statusEntries
	}
	cfg, configErr := config.ValidateArchitectConfig(architectPath, filepath.Base(architectPath))
	counts := map[string]int{}
	for _, items := range entries {
		for _, item := range items {
			counts[item.id]++
		}
	}
	for status, items := range entries {
		for i := range items {
			if configErr == nil {
				item := &items[i]
				if !ticketIDPattern.MatchString(item.id) {
					item.warnings = append(item.warnings, domain.TicketWarning{Code: "INVALID_TICKET_ID", Message: "Ticket identity must use YYYY-MM-DD-HHMM-readable-slug."})
				}
				seenRepos := map[string]bool{}
				for _, key := range append([]string{item.metadata.Repo}, item.metadata.AdditionalRepos...) {
					_, exists := cfg.Repos[key]
					if item.status == domain.TicketStatusDone {
						if key != "" && !exists {
							item.warnings = append(item.warnings, domain.TicketWarning{Code: "HISTORICAL_SCOPE_RETIRED", Message: "Completed record retains retired repository " + key + "; its original scope is preserved."})
						}
					} else if !exists || seenRepos[key] || cfg.RepoMachines[key] != cfg.RepoMachines[item.metadata.Repo] {
						item.warnings = append(item.warnings, domain.TicketWarning{Code: "INVALID_TICKET_SCOPE", Message: "Repository " + key + " is unknown, duplicated or on a different machine; correct the writable scope using hiveryn.yaml."})
					}
					seenRepos[key] = true
				}
			}
			if counts[items[i].id] > 1 {
				items[i].warnings = append(items[i].warnings, domain.TicketWarning{Code: "INVALID_TICKET", Message: "Duplicate ticket identity " + items[i].id + " across status directories; restore one authoritative folder."})
			}
		}
		entries[status] = items
	}
	return entries, nil
}

func scanStatusTickets(architectPath string, status domain.TicketStatus, seen map[string]domain.TicketStatus) ([]ticketEntry, error) {
	statusDir := filepath.Join(architectPath, ticketsDirName, string(status))
	items, err := os.ReadDir(statusDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ticketEntry{}, nil
		}
		return nil, fmt.Errorf("read status directory %q: %w", statusDir, err)
	}

	entries := make([]ticketEntry, 0, len(items))
	for _, item := range items {
		if !item.IsDir() && item.Type()&os.ModeSymlink == 0 {
			continue
		}
		id := item.Name()
		prior, duplicate := seen[id]
		seen[id] = status
		entry, err := loadTicketEntry(architectPath, status, id)
		if duplicate {
			err = fmt.Errorf("ticket identity %s exists in both %s and %s", id, prior, status)
		}
		if err != nil {
			entry = ticketEntry{id: id, status: status, dir: ticketDir(architectPath, status, id), metadata: ticketMetadata{Title: id}}
			entry.warnings = []domain.TicketWarning{{Code: "INVALID_TICKET", Message: filepath.Join(entry.dir, ticketFileName) + ": " + err.Error()}}
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		left := ticketSortTime(entries[i].metadata.Created, entries[i].id)
		right := ticketSortTime(entries[j].metadata.Created, entries[j].id)
		if left.Equal(right) {
			return entries[i].id > entries[j].id
		}
		return left.After(right)
	})

	return entries, nil
}

func getTicketEntry(architectPath, id string) (ticketEntry, error) {
	entries, err := scanTickets(architectPath)
	if err != nil {
		return ticketEntry{}, err
	}
	entry, err := findTicketEntry(entries, id)
	if err != nil {
		return entry, err
	}
	for _, w := range entry.warnings {
		if w.Code == "INVALID_TICKET" {
			return entry, &domain.ValidationError{Field: "ticket", Message: w.Message}
		}
	}
	return entry, nil
}

func findTicketEntry(entries map[domain.TicketStatus][]ticketEntry, id string) (ticketEntry, error) {
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		for _, entry := range entries[status] {
			if entry.id == id {
				return entry, nil
			}
		}
	}
	return ticketEntry{}, &domain.NotFoundError{Resource: "ticket", ID: id}
}

func loadTicketEntry(architectPath string, status domain.TicketStatus, id string) (ticketEntry, error) {
	dir := ticketDir(architectPath, status, id)
	for _, path := range []string{filepath.Join(architectPath, ticketsDirName), filepath.Join(architectPath, ticketsDirName, string(status)), dir, filepath.Join(dir, ticketFileName)} {
		info, err := os.Lstat(path)
		if err != nil {
			return ticketEntry{}, fmt.Errorf("inspect ticket path %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ticketEntry{}, fmt.Errorf("ticket path %s must not be a symlink", path)
		}
	}

	document, err := ReadMarkdownDocument(filepath.Join(dir, ticketFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return ticketEntry{}, &domain.ValidationError{Field: "ticket.md", Message: "is required for ticket " + id}
		}
		return ticketEntry{}, err
	}

	metadata, err := decodeTicketMetadata(document.Metadata)
	if err != nil {
		return ticketEntry{}, err
	}
	if strings.TrimSpace(metadata.Title) == "" || (status != domain.TicketStatusDone && strings.TrimSpace(metadata.Repo) == "") || metadata.Created == nil || metadata.Updated == nil {
		return ticketEntry{}, fmt.Errorf("title, repo, created_at and updated_at are required in ticket %s", id)
	}
	for _, key := range []string{"created_at", "updated_at"} {
		n := mappingValue(document.Metadata, key)
		if n == nil || !strings.HasSuffix(n.Value, "Z") {
			return ticketEntry{}, fmt.Errorf("%s must be an RFC3339 UTC timestamp ending in Z", key)
		}
	}

	entry := ticketEntry{
		id:       id,
		status:   status,
		dir:      dir,
		document: document,
		metadata: metadata,
	}

	if status == domain.TicketStatusDone && strings.TrimSpace(metadata.Repo) == "" {
		entry.warnings = append(entry.warnings, domain.TicketWarning{Code: "HISTORICAL_SCOPE_UNRECORDED", Message: "This completed historical ticket did not record repository scope. It is preserved without inventing ownership; new and active tickets require a configured repository."})
	}

	conclusionPath := filepath.Join(dir, conclusionFileName)
	if _, err := os.Stat(conclusionPath); err == nil {
		conclusion, err := readConclusion(conclusionPath, metadata.Repo)
		if err != nil {
			return ticketEntry{}, err
		}
		entry.conclusion = conclusion
		if status != domain.TicketStatusDone {
			entry.warnings = append(entry.warnings, domain.TicketWarning{
				Code:    warningConclusionOutsideDone,
				Message: "ticket has conclusion.md outside done",
			})
		}
	} else if err != nil && !os.IsNotExist(err) {
		return ticketEntry{}, fmt.Errorf("stat ticket conclusion %q: %w", conclusionPath, err)
	}

	if status == domain.TicketStatusDone && entry.conclusion == nil {
		entry.warnings = append(entry.warnings, domain.TicketWarning{
			Code:    warningDoneWithoutConclusion,
			Message: "ticket in done does not have conclusion.md",
		})
	}

	return entry, nil
}

func nextTicketID(architectPath string, now time.Time, title string) (string, error) {
	slug := slugify(title)
	if slug == "" {
		slug = "ticket"
	}
	prefix := now.Local().Format("2006-01-02-1504") + "-" + slug
	entries, err := scanTickets(architectPath)
	if err != nil {
		return "", err
	}
	existing := map[string]struct{}{}
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		for _, entry := range entries[status] {
			existing[entry.id] = struct{}{}
		}
	}
	id := prefix
	for suffix := 2; ; suffix++ {
		if _, ok := existing[id]; !ok {
			return id, nil
		}
		id = fmt.Sprintf("%s-%d", prefix, suffix)
	}
}

func ticketDir(architectPath string, status domain.TicketStatus, id string) string {
	return filepath.Join(architectPath, ticketsDirName, string(status), id)
}

func ticketSortTime(created *time.Time, id string) time.Time {
	if created != nil {
		return *created
	}
	if parsed, ok := parseTicketIDTime(id); ok {
		return parsed
	}
	return time.Time{}
}

func parseTicketIDTime(id string) (time.Time, bool) {
	const layout = "2006-01-02-1504"
	if len(id) < len(layout) {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation(layout, id[:len(layout)], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func slugify(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	var b strings.Builder
	lastDash := false
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if lastDash {
			continue
		}
		b.WriteByte('-')
		lastDash = true
	}
	return strings.Trim(b.String(), "-")
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	cloned := make([]string, len(s))
	copy(cloned, s)
	return cloned
}

func normalizeRepoKeys(keys []string) []string {
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, strings.TrimSpace(key))
	}
	sort.Strings(result)
	return result
}

func nonNilCommitRefs(commits []domain.CommitRef) []domain.CommitRef {
	if commits == nil {
		return []domain.CommitRef{}
	}
	cloned := make([]domain.CommitRef, len(commits))
	copy(cloned, commits)
	return cloned
}

func cloneWarnings(warnings []domain.TicketWarning) []domain.TicketWarning {
	cloned := make([]domain.TicketWarning, len(warnings))
	copy(cloned, warnings)
	return cloned
}

type ticketMetadata struct {
	Title           string
	Repo            string
	AdditionalRepos []string
	Created         *time.Time
	Updated         *time.Time
	References      []string
	// ReferenceLines are the frontmatter lines (1-based within the YAML) of
	// References, when decoded from a file.
	ReferenceLines []int
}

func decodeTicketMetadata(node *yaml.Node) (ticketMetadata, error) {
	if node == nil {
		return ticketMetadata{}, nil
	}
	var raw struct {
		Title           string    `yaml:"title"`
		Repo            string    `yaml:"repo"`
		AdditionalRepos []string  `yaml:"additional_repos"`
		Created         time.Time `yaml:"created_at"`
		Updated         time.Time `yaml:"updated_at"`
		References      []string  `yaml:"references"`
	}
	if err := node.Decode(&raw); err != nil {
		return ticketMetadata{}, fmt.Errorf("decode ticket frontmatter: %w", err)
	}
	metadata := ticketMetadata{
		Title:           raw.Title,
		Repo:            raw.Repo,
		AdditionalRepos: append([]string(nil), raw.AdditionalRepos...),
		References:      append([]string(nil), raw.References...),
	}
	if refs := mappingValue(node, "references"); refs != nil && refs.Kind == yaml.SequenceNode {
		for _, item := range refs.Content {
			metadata.ReferenceLines = append(metadata.ReferenceLines, item.Line)
		}
	}
	if !raw.Created.IsZero() {
		created := raw.Created.UTC()
		metadata.Created = &created
	}
	if !raw.Updated.IsZero() {
		updated := raw.Updated.UTC()
		metadata.Updated = &updated
	}
	return metadata, nil
}

func newTicketDocument(metadata ticketMetadata, body string) MarkdownDocument {
	meta := newMappingNode()
	setNodeString(meta, "title", metadata.Title)
	if metadata.Repo != "" {
		setNodeString(meta, "repo", metadata.Repo)
	}
	setNodeStrings(meta, "additional_repos", nonNilStrings(metadata.AdditionalRepos))
	if metadata.Created != nil {
		setNodeTime(meta, "created_at", metadata.Created.UTC())
	}
	if metadata.Updated != nil {
		setNodeTime(meta, "updated_at", metadata.Updated.UTC())
	}
	if metadata.References != nil {
		setNodeStrings(meta, "references", metadata.References)
	}
	return MarkdownDocument{Metadata: meta, Body: body}
}

func readConclusion(path, ticketRepo string) (*domain.TicketConclusion, error) {
	doc, err := ReadMarkdownDocument(path)
	if err != nil {
		return nil, err
	}
	if doc.Metadata == nil {
		return nil, &domain.ValidationError{Field: "conclusion.md", Message: "frontmatter is required"}
	}
	var raw struct {
		StartedAt       time.Time            `yaml:"started_at"`
		ConcludedAt     time.Time            `yaml:"concluded_at"`
		Agent           string               `yaml:"agent"`
		Profile         string               `yaml:"profile"`
		Outcome         domain.TicketOutcome `yaml:"outcome"`
		RejectionReason string               `yaml:"rejection_reason"`
		// Rejected is the pre-outcome-enum bool. It is only consulted below to
		// infer Outcome for conclusion.md files written before this field
		// existed; it is never part of domain.TicketConclusion.
		Rejected bool `yaml:"rejected"`
	}
	if err := doc.Metadata.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode ticket conclusion frontmatter: %w", err)
	}
	commits, err := parseConclusionCommitRefs(doc.Metadata, ticketRepo)
	if err != nil {
		return nil, err
	}
	if raw.StartedAt.IsZero() {
		return nil, &domain.ValidationError{Field: "started_at", Message: "is required in conclusion.md"}
	}
	if raw.ConcludedAt.IsZero() {
		return nil, &domain.ValidationError{Field: "concluded_at", Message: "is required in conclusion.md"}
	}
	outcome := raw.Outcome
	if outcome == "" {
		// Pre-existing file, written before the outcome field existed: infer
		// from the old rejected bool (exploratory didn't exist as a concept
		// yet, so every historical non-rejected conclusion was completed).
		if raw.Rejected {
			outcome = domain.TicketOutcomeRejected
		} else {
			outcome = domain.TicketOutcomeCompleted
		}
	}
	if !outcome.Valid() {
		return nil, &domain.ValidationError{Field: "outcome", Message: "must be one of: completed, exploratory, rejected"}
	}
	if outcome == domain.TicketOutcomeRejected && strings.TrimSpace(raw.RejectionReason) == "" {
		return nil, &domain.ValidationError{Field: "rejection_reason", Message: "is required when outcome is rejected"}
	}
	return &domain.TicketConclusion{
		StartedAt:       raw.StartedAt.UTC(),
		ConcludedAt:     raw.ConcludedAt.UTC(),
		Agent:           raw.Agent,
		Profile:         raw.Profile,
		Outcome:         outcome,
		RejectionReason: raw.RejectionReason,
		Commits:         commits,
		Body:            doc.Body,
	}, nil
}

func parseConclusionCommitRefs(meta *yaml.Node, ticketRepo string) ([]domain.CommitRef, error) {
	commitsNode := mappingValue(meta, "commits")
	if commitsNode == nil {
		return []domain.CommitRef{}, nil
	}
	if commitsNode.Kind != yaml.SequenceNode {
		return nil, &domain.ValidationError{Field: "commits", Message: "must be a YAML sequence in conclusion.md"}
	}

	commits := make([]domain.CommitRef, 0, len(commitsNode.Content))
	for _, item := range commitsNode.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			if strings.TrimSpace(item.Value) == "" {
				return nil, &domain.ValidationError{Field: "commits", Message: "commit sha cannot be empty in conclusion.md"}
			}
			commits = append(commits, domain.CommitRef{SHA: item.Value, Repo: ticketRepo})
		case yaml.MappingNode:
			var commit domain.CommitRef
			if err := item.Decode(&commit); err != nil {
				return nil, fmt.Errorf("decode commit entry in conclusion.md: %w", err)
			}
			if strings.TrimSpace(commit.SHA) == "" {
				return nil, &domain.ValidationError{Field: "commits", Message: "commit sha cannot be empty in conclusion.md"}
			}
			if strings.TrimSpace(commit.Repo) == "" {
				return nil, &domain.ValidationError{Field: "commits", Message: "commit repo cannot be empty in conclusion.md"}
			}
			commits = append(commits, commit)
		default:
			return nil, &domain.ValidationError{Field: "commits", Message: "must contain only strings or {sha, repo} objects in conclusion.md"}
		}
	}
	if commits == nil {
		return []domain.CommitRef{}, nil
	}
	return commits, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func ticketIDs(entries map[domain.TicketStatus][]ticketEntry) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		for _, entry := range entries[status] {
			ids[entry.id] = struct{}{}
		}
	}
	return ids
}

// normalizeReferences validates references written through the API: each is
// trimmed, must parse strictly, must name architect or a repo key of the
// architect's hiveryn.yaml, and must be unique. Every problem is reported at
// once. Existence is not required (a missing target is a warning on read).
func normalizeReferences(architectPath string, references []string) ([]string, error) {
	if references == nil {
		return []string{}, nil
	}
	var repos map[string]string
	var configErr error
	normalized := make([]string, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	var problems []string
	for _, reference := range references {
		reference = strings.TrimSpace(reference)
		parsed, err := domain.ParseReference(reference)
		switch {
		case err != nil:
			problems = append(problems, err.Error())
			continue
		case parsed.Kind == domain.ReferenceKindRepo:
			if repos == nil && configErr == nil {
				var cfg config.ArchitectConfig
				cfg, configErr = config.ValidateArchitectConfig(architectPath, filepath.Base(architectPath))
				repos = cfg.Repos
			}
			if configErr != nil {
				problems = append(problems, fmt.Sprintf("cannot check repo key of %q: %v", reference, configErr))
				continue
			}
			if _, ok := repos[parsed.Owner]; !ok {
				keys := make([]string, 0, len(repos))
				for key := range repos {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				problems = append(problems, (&contextref.UnknownOwnerError{Reference: reference, Owner: parsed.Owner, Known: keys}).Error())
				continue
			}
		}
		if _, ok := seen[reference]; ok {
			problems = append(problems, "duplicate reference "+reference)
			continue
		}
		seen[reference] = struct{}{}
		normalized = append(normalized, reference)
	}
	if len(problems) > 0 {
		return nil, &domain.ValidationError{Field: "references", Message: strings.Join(problems, "; ")}
	}
	return normalized, nil
}

// referenceAt locates reference i for warnings: the ticket.md line when known.
func (e *ticketEntry) referenceAt(i int) string {
	if i < len(e.metadata.ReferenceLines) && e.metadata.ReferenceLines[i] > 0 {
		// The frontmatter starts after the opening "---" line.
		return fmt.Sprintf("ticket.md line %d: ", e.metadata.ReferenceLines[i]+1)
	}
	return ""
}

func (s *TicketService) SetProtection(p *Protection) { s.protection = p }
func (s *TicketService) mutate(ctx context.Context, root string, id string, fn func() error) error {
	if s.protection == nil {
		return fn()
	}
	var allowed []string
	if id != "" {
		for _, status := range []string{"backlog", "progress", "done"} {
			allowed = append(allowed, "tickets/"+status+"/"+id+"/")
		}
	}
	return s.protection.Mutate(ctx, root, allowed, fn)
}

func (s *TicketService) CreateTicket(ctx context.Context, architectPath string, params domain.CreateTicketParams) (ticket domain.Ticket, err error) {
	err = s.mutate(ctx, architectPath, "", func() error { var e error; ticket, e = s.createTicket(ctx, architectPath, params); return e })
	return
}

func (s *TicketService) EditTicket(ctx context.Context, architectPath, id string, params domain.EditTicketParams) (ticket domain.Ticket, err error) {
	err = s.mutate(ctx, architectPath, id, func() error { var e error; ticket, e = s.editTicket(ctx, architectPath, id, params); return e })
	return
}

func (s *TicketService) UpdateTicketMetadata(ctx context.Context, architectPath, id string, params domain.UpdateTicketMetadataParams) (ticket domain.Ticket, err error) {
	err = s.mutate(ctx, architectPath, id, func() error {
		var e error
		ticket, e = s.updateTicketMetadata(ctx, architectPath, id, params)
		return e
	})
	return
}

func (s *TicketService) ConcludeTicket(ctx context.Context, architectPath, id string, conclusion domain.TicketConclusion) (ticket domain.Ticket, err error) {
	err = s.mutate(ctx, architectPath, id, func() error { var e error; ticket, e = s.concludeTicket(ctx, architectPath, id, conclusion); return e })
	return
}

func (s *TicketService) MoveTicket(ctx context.Context, architectPath, id string, params domain.MoveTicketParams) (ticket domain.Ticket, err error) {
	err = s.mutate(ctx, architectPath, id, func() error { var e error; ticket, e = s.moveTicket(ctx, architectPath, id, params); return e })
	return
}

func (s *TicketService) DeleteTicket(ctx context.Context, architectPath, id string) error {
	return s.mutate(ctx, architectPath, id, func() error { return s.deleteTicket(ctx, architectPath, id) })
}
