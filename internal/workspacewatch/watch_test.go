package workspacewatch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

func TestFilesystemQuietPeriodAndCompleteReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("hiveryn.yaml", "name: Example\nrepos:\n  core: "+root+"\n")
	write("OVERVIEW.md", "---\nupdated_at: 2026-01-01T00:00:00Z\n---\n\nOverview.\n")
	write("workflows/deliver.md", "Deliver.\n")
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	protection := architectfs.NewProtection(store.NewWorkspaceRecordStore(db))
	tickets := architectfs.NewTicketService(nil)
	tickets.SetProtection(protection)
	events := make(chan domain.ArchitectEvent, 20)
	messages := make(chan string, 20)
	w := &Watcher{Source: config.StaticSource(config.Config{Architects: map[string]config.ArchitectConfig{"example": {Path: root}}}), Protection: protection, Workspaces: workspacefs.NewService(tickets), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Publish: func(_ string, e domain.ArchitectEvent) { events <- e }, Notify: func(_ context.Context, _ string, body string) error { messages <- body; return nil }}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("startup did not reconcile")
	}
	// Atomic editor replacement and a newly created nested ticket directory.
	write("draft.tmp", "broken overview")
	if err := os.Rename(filepath.Join(root, "draft.tmp"), filepath.Join(root, "OVERVIEW.md")); err != nil {
		t.Fatal(err)
	}
	write("tickets/backlog/2026-01-01-0000-invalid/ticket.md", "---\ntitle: Broken\n---\nBody")
	select {
	case m := <-messages:
		t.Fatalf("validation before quiet period: %s", m)
	case <-time.After(time.Second):
	}
	var first string
	select {
	case first = <-messages:
	case <-time.After(5 * time.Second):
		t.Fatal("missing validation after edit")
	}
	if !strings.Contains(first, "OVERVIEW.md") || !strings.Contains(first, "INVALID_TICKET") {
		t.Fatalf("not a complete report: %s", first)
	}
	board, err := tickets.ListTickets(ctx, root)
	if err != nil || len(board.Backlog) != 1 || len(board.Backlog[0].Warnings) == 0 {
		t.Fatalf("invalid ticket vanished: %+v %v", board, err)
	}
	write("workflows/deliver.md", "Changed delivery.\n")
	select {
	case second := <-messages:
		if !strings.Contains(second, "OVERVIEW.md") || !strings.Contains(second, "INVALID_TICKET") {
			t.Fatalf("previous errors suppressed: %s", second)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing repeat complete report")
	}
	// Replacing an entire watched directory must install watches on its new inode.
	if err := os.Rename(filepath.Join(root, "workflows"), filepath.Join(root, "old-workflows")); err != nil {
		t.Fatal(err)
	}
	write("workflows/deliver.md", "Replacement directory.\n")
	select {
	case <-messages:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement directory was not reconciled")
	}
	write("workflows/deliver.md", "Edit after replacement.\n")
	select {
	case m := <-messages:
		if !strings.Contains(m, "INVALID_TICKET") {
			t.Fatal(m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement directory was not watched")
	}

}

func TestProtectedRecordsSurviveRestartAndDoNotAuthorizeEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	records := store.NewWorkspaceRecordStore(db)
	p := architectfs.NewProtection(records)
	service := architectfs.NewTicketService(nil)
	service.SetProtection(p)
	ticket, err := service.CreateTicket(ctx, root, domain.CreateTicketParams{Title: "Owned", Repo: "core", Body: "Original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.MoveTicket(ctx, root, ticket.ID, domain.MoveTicketParams{To: domain.TicketStatusProgress}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tickets/progress", ticket.ID, "ticket.md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(original), "repo: core", "repo: other", 1)
	if err := os.WriteFile(path, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	// A new protection instance uses the durable accepted fingerprint.
	p = architectfs.NewProtection(records)
	service.SetProtection(p)
	_, diags, err := p.Inspect(ctx, root)
	if err != nil || len(diags) != 1 {
		t.Fatalf("%+v %v", diags, err)
	}
	if _, err := service.MoveTicket(ctx, root, ticket.ID, domain.MoveTicketParams{To: domain.TicketStatusBacklog}); err == nil {
		t.Fatal("external scope edit was accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != edited {
		t.Fatal("conflict was auto-reverted")
	}
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConcludeTicket(ctx, root, ticket.ID, domain.TicketConclusion{StartedAt: time.Now(), ConcludedAt: time.Now(), Outcome: domain.TicketOutcomeExploratory, Body: "Verified"}); err != nil {
		t.Fatal(err)
	}
	_, diags, err = p.Inspect(ctx, root)
	if err != nil || len(diags) != 0 {
		t.Fatalf("owned conclusion caused conflict: %+v %v", diags, err)
	}
	conclusion := filepath.Join(root, "tickets/done", ticket.ID, "conclusion.md")
	if err := os.Remove(conclusion); err != nil {
		t.Fatal(err)
	}
	_, diags, err = p.Inspect(ctx, root)
	if err != nil || len(diags) != 1 {
		t.Fatalf("deleted conclusion not detected: %+v %v", diags, err)
	}
}
