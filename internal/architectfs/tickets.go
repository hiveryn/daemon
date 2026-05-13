package architectfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

const (
	ticketsDirName     = "tickets"
	ticketFileName     = "ticket.md"
	conclusionFileName = "conclusion.md"

	warningDoneWithoutConclusion = "DONE_WITHOUT_CONCLUSION"
	warningConclusionOutsideDone = "CONCLUSION_OUTSIDE_DONE"
)

type TicketService struct{}

func NewTicketService() *TicketService {
	return &TicketService{}
}

func (s *TicketService) ListTickets(_ context.Context, architectPath string) (domain.TicketBoard, error) {
	entries, err := scanTickets(architectPath)
	if err != nil {
		return domain.TicketBoard{}, err
	}

	board := domain.TicketBoard{
		Backlog:  make([]domain.TicketSummary, 0, len(entries[domain.TicketStatusBacklog])),
		Progress: make([]domain.TicketSummary, 0, len(entries[domain.TicketStatusProgress])),
		Done:     make([]domain.TicketSummary, 0, len(entries[domain.TicketStatusDone])),
	}

	for _, item := range entries[domain.TicketStatusBacklog] {
		board.Backlog = append(board.Backlog, item.summary())
	}
	for _, item := range entries[domain.TicketStatusProgress] {
		board.Progress = append(board.Progress, item.summary())
	}
	for _, item := range entries[domain.TicketStatusDone] {
		board.Done = append(board.Done, item.summary())
	}

	return board, nil

}

func (s *TicketService) GetTicket(_ context.Context, architectPath, id string) (domain.Ticket, error) {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	return entry.ticket(), nil
}

func (s *TicketService) CreateTicket(_ context.Context, architectPath string, params domain.CreateTicketParams) (domain.Ticket, error) {
	if strings.TrimSpace(params.Title) == "" {
		return domain.Ticket{}, &domain.ValidationError{Field: "title", Message: "is required"}
	}

	now := params.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	entries, err := scanTickets(architectPath)
	if err != nil {
		return domain.Ticket{}, err
	}
	references, err := normalizeReferences(params.References)
	if err != nil {
		return domain.Ticket{}, err
	}
	if err := validateTicketReferences("new ticket", references, ticketIDs(entries)); err != nil {
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
		Title:      strings.TrimSpace(params.Title),
		Repo:       strings.TrimSpace(params.Repo),
		Created:    &now,
		Updated:    &now,
		References: references,
	}, params.Body)
	if err := writeMarkdownDocument(filepath.Join(dir, ticketFileName), doc); err != nil {
		return domain.Ticket{}, err
	}

	entry, err := loadTicketEntry(architectPath, status, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	return entry.ticket(), nil
}

func (s *TicketService) EditTicket(_ context.Context, architectPath, id string, params domain.EditTicketParams) (domain.Ticket, error) {
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
	setNodeTime(entry.document.Metadata, "updated", now)

	if err := writeMarkdownDocument(filepath.Join(entry.dir, ticketFileName), entry.document); err != nil {
		return domain.Ticket{}, err
	}

	updated, err := loadTicketEntry(architectPath, entry.status, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	return updated.ticket(), nil
}

func (s *TicketService) UpdateTicketMetadata(_ context.Context, architectPath, id string, params domain.UpdateTicketMetadataParams) (domain.Ticket, error) {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}

	entries, err := scanTickets(architectPath)
	if err != nil {
		return domain.Ticket{}, err
	}
	ids := ticketIDs(entries)

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

	if params.References != nil {
		references, err := normalizeReferences(*params.References)
		if err != nil {
			return domain.Ticket{}, err
		}
		if err := validateTicketReferences(id, references, ids); err != nil {
			return domain.Ticket{}, err
		}
		setNodeStrings(entry.document.Metadata, "references", references)
	}

	now := params.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	setNodeTime(entry.document.Metadata, "updated", now)

	if err := writeMarkdownDocument(filepath.Join(entry.dir, ticketFileName), entry.document); err != nil {
		return domain.Ticket{}, err
	}

	updated, err := loadTicketEntry(architectPath, entry.status, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	return updated.ticket(), nil
}

func (s *TicketService) DeleteTicket(_ context.Context, architectPath, id string) error {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(entry.dir); err != nil {
		return fmt.Errorf("remove ticket directory: %w", err)
	}
	return nil
}

func (s *TicketService) ConcludeTicket(_ context.Context, architectPath, id string, conclusion domain.TicketConclusion) (domain.Ticket, error) {
	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	if entry.status != domain.TicketStatusProgress {
		return domain.Ticket{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in progress to conclude"}
	}

	doc := newConclusionDocument(conclusion)
	if err := writeMarkdownDocument(filepath.Join(entry.dir, conclusionFileName), doc); err != nil {
		return domain.Ticket{}, err
	}

	now := time.Now().UTC()
	setNodeTime(entry.document.Metadata, "updated", now)
	if err := writeMarkdownDocument(filepath.Join(entry.dir, ticketFileName), entry.document); err != nil {
		return domain.Ticket{}, err
	}

	moved, err := s.MoveTicket(context.Background(), architectPath, id, domain.MoveTicketParams{To: domain.TicketStatusDone})
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
	if conclusion.Rejected {
		setNodeBool(meta, "rejected", true)
	}
	if conclusion.RejectionReason != "" {
		setNodeString(meta, "rejection_reason", conclusion.RejectionReason)
	}
	if len(conclusion.Commits) > 0 {
		setNodeStrings(meta, "commits", conclusion.Commits)
	}
	return MarkdownDocument{Metadata: meta, Body: conclusion.Body}
}

func (s *TicketService) MoveTicket(_ context.Context, architectPath, id string, params domain.MoveTicketParams) (domain.Ticket, error) {
	if !params.To.Valid() {
		return domain.Ticket{}, &domain.ValidationError{Field: "to", Message: "must be backlog, progress, or done"}
	}

	entry, err := getTicketEntry(architectPath, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	if entry.status == params.To {
		return entry.ticket(), nil
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

	moved, err := loadTicketEntry(architectPath, params.To, id)
	if err != nil {
		return domain.Ticket{}, err
	}
	return moved.ticket(), nil
}

type ticketEntry struct {
	id         string
	status     domain.TicketStatus
	dir        string
	document   MarkdownDocument
	metadata   ticketMetadata
	conclusion *domain.TicketConclusion
	warnings   []domain.TicketWarning
}

func (e ticketEntry) summary() domain.TicketSummary {
	return domain.TicketSummary{
		ID:            e.id,
		Status:        e.status,
		Title:         e.metadata.Title,
		Repo:          e.metadata.Repo,
		Created:       e.metadata.Created,
		Updated:       e.metadata.Updated,
		References:    nonNilStrings(e.metadata.References),
		HasConclusion: e.conclusion != nil,
		Warnings:      cloneWarnings(e.warnings),
	}

}

func (e ticketEntry) ticket() domain.Ticket {
	t := domain.Ticket{
		TicketSummary: e.summary(),
		Body:          e.document.Body,
	}
	if e.conclusion != nil {
		conclusion := *e.conclusion
		conclusion.Commits = nonNilStrings(conclusion.Commits)
		t.Conclusion = &conclusion
	}
	return t
}

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
	if err := validateAllTicketReferences(entries); err != nil {
		return nil, err
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
		if !item.IsDir() {
			continue
		}
		id := item.Name()
		if prior, ok := seen[id]; ok {
			return nil, &domain.ConflictError{Resource: "ticket", Field: "id", Message: id + " exists in both " + string(prior) + " and " + string(status)}
		}
		seen[id] = status

		entry, err := loadTicketEntry(architectPath, status, id)
		if err != nil {
			return nil, err
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
	document, err := readMarkdownDocument(filepath.Join(dir, ticketFileName))
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
	if strings.TrimSpace(metadata.Title) == "" {
		return ticketEntry{}, &domain.ValidationError{Field: "title", Message: "is required in ticket " + id}
	}

	entry := ticketEntry{
		id:       id,
		status:   status,
		dir:      dir,
		document: document,
		metadata: metadata,
	}

	conclusionPath := filepath.Join(dir, conclusionFileName)
	if _, err := os.Stat(conclusionPath); err == nil {
		conclusion, err := readConclusion(conclusionPath)
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
	if len(id) < len("2006-01-02-1504") {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation("2006-01-02-1504", id[:16], time.Local)
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

func cloneWarnings(warnings []domain.TicketWarning) []domain.TicketWarning {
	cloned := make([]domain.TicketWarning, len(warnings))
	copy(cloned, warnings)
	return cloned
}

type ticketMetadata struct {
	Title      string
	Repo       string
	Created    *time.Time
	Updated    *time.Time
	References []string
}

func decodeTicketMetadata(node *yaml.Node) (ticketMetadata, error) {
	if node == nil {
		return ticketMetadata{}, nil
	}
	var raw struct {
		Title      string    `yaml:"title"`
		Repo       string    `yaml:"repo"`
		Created    time.Time `yaml:"created"`
		Updated    time.Time `yaml:"updated"`
		References []string  `yaml:"references"`
	}
	if err := node.Decode(&raw); err != nil {
		return ticketMetadata{}, fmt.Errorf("decode ticket frontmatter: %w", err)
	}
	metadata := ticketMetadata{
		Title:      raw.Title,
		Repo:       raw.Repo,
		References: append([]string(nil), raw.References...),
	}
	var err error
	metadata.References, err = normalizeReferences(metadata.References)
	if err != nil {
		return ticketMetadata{}, err
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
	if metadata.Created != nil {
		setNodeTime(meta, "created", metadata.Created.UTC())
	}
	if metadata.Updated != nil {
		setNodeTime(meta, "updated", metadata.Updated.UTC())
	}
	if metadata.References != nil {
		setNodeStrings(meta, "references", metadata.References)
	}
	return MarkdownDocument{Metadata: meta, Body: body}
}

func readConclusion(path string) (*domain.TicketConclusion, error) {
	doc, err := readMarkdownDocument(path)
	if err != nil {
		return nil, err
	}
	if doc.Metadata == nil {
		return nil, &domain.ValidationError{Field: "conclusion.md", Message: "frontmatter is required"}
	}
	var raw struct {
		StartedAt       time.Time `yaml:"started_at"`
		ConcludedAt     time.Time `yaml:"concluded_at"`
		Agent           string    `yaml:"agent"`
		Profile         string    `yaml:"profile"`
		Rejected        bool      `yaml:"rejected"`
		RejectionReason string    `yaml:"rejection_reason"`
		Commits         []string  `yaml:"commits"`
	}
	if err := doc.Metadata.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode ticket conclusion frontmatter: %w", err)
	}
	if raw.StartedAt.IsZero() {
		return nil, &domain.ValidationError{Field: "started_at", Message: "is required in conclusion.md"}
	}
	if raw.ConcludedAt.IsZero() {
		return nil, &domain.ValidationError{Field: "concluded_at", Message: "is required in conclusion.md"}
	}
	if raw.Rejected && strings.TrimSpace(raw.RejectionReason) == "" {
		return nil, &domain.ValidationError{Field: "rejection_reason", Message: "is required when rejected is true"}
	}
	return &domain.TicketConclusion{
		StartedAt:       raw.StartedAt.UTC(),
		ConcludedAt:     raw.ConcludedAt.UTC(),
		Agent:           raw.Agent,
		Profile:         raw.Profile,
		Rejected:        raw.Rejected,
		RejectionReason: raw.RejectionReason,
		Commits:         nonNilStrings(raw.Commits),
		Body:            doc.Body,
	}, nil
}

func validateAllTicketReferences(entries map[domain.TicketStatus][]ticketEntry) error {
	ids := ticketIDs(entries)
	for _, status := range []domain.TicketStatus{domain.TicketStatusBacklog, domain.TicketStatusProgress, domain.TicketStatusDone} {
		for _, entry := range entries[status] {
			if err := validateTicketReferences(entry.id, entry.metadata.References, ids); err != nil {
				return err
			}
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

func validateTicketReferences(ticketID string, references []string, validIDs map[string]struct{}) error {
	for _, reference := range references {
		if _, ok := validIDs[reference]; ok {
			continue
		}
		return &domain.ValidationError{Field: "references", Message: "contains unknown ticket reference " + reference + " in ticket " + ticketID}
	}
	return nil
}

func normalizeReferences(references []string) ([]string, error) {
	if references == nil {
		return []string{}, nil
	}
	normalized := make([]string, 0, len(references))
	for _, reference := range references {
		reference = strings.TrimSpace(reference)
		if reference == "" {
			return nil, &domain.ValidationError{Field: "references", Message: "must contain only non-empty ticket IDs"}
		}
		normalized = append(normalized, reference)
	}
	return normalized, nil
}
