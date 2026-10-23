package architectfs

import (
	"context"
	"github.com/hiveryn/daemon/internal/domain"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoricalScopePreservationDoesNotRelaxBacklog(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hiveryn.yaml"), []byte("name: History\nrepos:\n  core: "+root+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ status, slug, repo, warning string }{
		{"done", "unrecorded", "", "HISTORICAL_SCOPE_UNRECORDED"},
		{"done", "retired", "repo: retired\n", "HISTORICAL_SCOPE_RETIRED"},
		{"backlog", "missing", "", "INVALID_TICKET"},
		{"backlog", "retired", "repo: retired\n", "INVALID_TICKET_SCOPE"},
	} {
		id := "2026-01-01-0000-" + tc.slug
		dir := filepath.Join(root, "tickets", tc.status, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		data := "---\ntitle: Historical evidence\n" + tc.repo + "created_at: 2026-01-01T00:00:00Z\nupdated_at: 2026-01-01T00:00:00Z\n---\nUnchanged narrative.\n"
		if err := os.WriteFile(filepath.Join(dir, "ticket.md"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		entries, err := scanStatusTickets(root, domain.TicketStatus(tc.status), map[string]domain.TicketStatus{})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatal("record disappeared")
		}
		// Avoid a duplicate ID in the other status while checking through public reads.
		ticket, err := NewTicketService(nil).GetTicket(context.Background(), root, id)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, w := range ticket.Warnings {
			if w.Code == tc.warning {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: warnings %+v", tc.slug, ticket.Warnings)
		}
		if tc.status == "done" && !strings.Contains(ticket.Body, "Unchanged narrative.") {
			t.Fatal("historical narrative lost")
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewTicketService(nil).CreateTicket(context.Background(), root, domain.CreateTicketParams{Title: "New", Body: "No scope"}); err == nil {
		t.Fatal("new ticket without repo accepted")
	}
}
