package architectfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestTicketServiceListAndGet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-backlog-ticket", "---\ntitle: Backlog ticket\nrepo: daemon\ncreated: 2026-05-12T09:00:00Z\nupdated: 2026-05-12T09:01:00Z\nreferences:\n  - abc\n---\n\nBacklog body\n")
	writeTicketFile(t, root, domain.TicketStatusBacklog, "abc", "---\ntitle: Referenced ticket\n---\n\nReference target\n")
	writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-1000-done-ticket", "---\ntitle: Done ticket\nrepo: daemon\n---\n\nDone body\n")
	writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-1000-done-ticket", "---\nstarted_at: 2026-05-12T10:00:00Z\nconcluded_at: 2026-05-12T10:30:00Z\nagent: codex\nprofile: codex-default\nrejected: false\ncommits:\n  - abc1234\n---\n\nSummary\n")
	writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-1100-missing-conclusion", "---\ntitle: Missing conclusion\nrepo: daemon\n---\n\nDone body\n")
	writeTicketFile(t, root, domain.TicketStatusProgress, "2026-05-12-1200-progress-ticket", "---\ntitle: Progress ticket\nrepo: daemon\n---\n\nProgress body\n")
	writeConclusionFile(t, root, domain.TicketStatusProgress, "2026-05-12-1200-progress-ticket", "---\nstarted_at: 2026-05-12T12:00:00Z\nconcluded_at: 2026-05-12T12:10:00Z\nrejected: false\n---\n\nUnexpected conclusion\n")

	service := NewTicketService()
	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(board.Backlog) != 2 || len(board.Progress) != 1 || len(board.Done) != 2 {
		t.Fatalf("unexpected board sizes: %#v", board)
	}
	if !board.Progress[0].HasConclusion || len(board.Progress[0].Warnings) != 1 || board.Progress[0].Warnings[0].Code != warningConclusionOutsideDone {
		t.Fatalf("expected progress ticket warning, got %#v", board.Progress[0])
	}
	if len(board.Done[0].Warnings) != 1 || board.Done[0].Warnings[0].Code != warningDoneWithoutConclusion {
		t.Fatalf("expected done ticket warning, got %#v", board.Done[0])
	}

	ticket, err := service.GetTicket(context.Background(), root, "2026-05-12-1000-done-ticket")
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.Conclusion == nil || ticket.Conclusion.Agent != "codex" || ticket.Body != "Done body\n" {
		t.Fatalf("unexpected ticket detail: %#v", ticket)
	}
	if len(ticket.Conclusion.Commits) != 1 || ticket.Conclusion.Commits[0] != (domain.CommitRef{SHA: "abc1234", Repo: "daemon"}) {
		t.Fatalf("expected legacy flat commit to resolve against ticket repo, got %#v", ticket.Conclusion.Commits)
	}
}

func TestTicketServiceAdditionalReposRoundTripCanonicalOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ticket, err := NewTicketService().CreateTicket(context.Background(), root, domain.CreateTicketParams{
		Title: "Cross repo", Repo: "daemon", AdditionalRepos: []string{" shared ", "desktop"}, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	want := []string{"desktop", "shared"}
	if !slices.Equal(ticket.AdditionalRepos, want) {
		t.Fatalf("additional repos = %v, want %v", ticket.AdditionalRepos, want)
	}
	content := readFile(t, filepath.Join(root, ticketsDirName, string(domain.TicketStatusBacklog), ticket.ID, ticketFileName))
	if !strings.Contains(content, "additional_repos:") || strings.Index(content, "desktop") > strings.Index(content, "shared") {
		t.Fatalf("unexpected frontmatter:\n%s", content)
	}
}

func TestTicketServiceConcludeTicketWritesStructuredCommitRefs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFile(t, root, domain.TicketStatusProgress, "2026-05-12-1300-structured-write", "---\ntitle: Structured write\nrepo: daemon\n---\n\nbody\n")

	service := NewTicketService()
	_, err := service.ConcludeTicket(context.Background(), root, "2026-05-12-1300-structured-write", domain.TicketConclusion{
		StartedAt:   time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC),
		ConcludedAt: time.Date(2026, 5, 12, 13, 30, 0, 0, time.UTC),
		Outcome:     domain.TicketOutcomeCompleted,
		Commits:     []domain.CommitRef{{SHA: "abc123", Repo: "daemon"}, {SHA: "def456", Repo: "desktop"}},
		Body:        "summary",
	})
	if err != nil {
		t.Fatalf("ConcludeTicket: %v", err)
	}

	content := readFile(t, filepath.Join(root, ticketsDirName, string(domain.TicketStatusDone), "2026-05-12-1300-structured-write", conclusionFileName))
	if !strings.Contains(content, "sha: abc123") || !strings.Contains(content, "repo: daemon") {
		t.Fatalf("expected structured daemon commit in conclusion, got:\n%s", content)
	}
	if !strings.Contains(content, "sha: def456") || !strings.Contains(content, "repo: desktop") {
		t.Fatalf("expected structured desktop commit in conclusion, got:\n%s", content)
	}
	if strings.Contains(content, "\n  - abc123\n") || strings.Contains(content, "\n  - def456\n") {
		t.Fatalf("expected structured commit objects instead of flat strings, got:\n%s", content)
	}
}

func TestTicketServiceConcludeTicketOutcomeRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		outcome    domain.TicketOutcome
		conclusion domain.TicketConclusion
	}{
		{
			name:    "completed",
			outcome: domain.TicketOutcomeCompleted,
			conclusion: domain.TicketConclusion{
				StartedAt:   time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC),
				ConcludedAt: time.Date(2026, 5, 12, 13, 30, 0, 0, time.UTC),
				Outcome:     domain.TicketOutcomeCompleted,
				Commits:     []domain.CommitRef{{SHA: "abc123", Repo: "daemon"}},
				Body:        "## Implementation\nDid the thing.",
			},
		},
		{
			name:    "exploratory",
			outcome: domain.TicketOutcomeExploratory,
			conclusion: domain.TicketConclusion{
				StartedAt:   time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC),
				ConcludedAt: time.Date(2026, 5, 12, 13, 30, 0, 0, time.UTC),
				Outcome:     domain.TicketOutcomeExploratory,
				Body:        "## Findings\nRace in the scheduler.",
			},
		},
		{
			name:    "rejected",
			outcome: domain.TicketOutcomeRejected,
			conclusion: domain.TicketConclusion{
				StartedAt:       time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC),
				ConcludedAt:     time.Date(2026, 5, 12, 13, 30, 0, 0, time.UTC),
				Outcome:         domain.TicketOutcomeRejected,
				RejectionReason: "duplicate of another ticket",
				Body:            "## Summary\nDuplicate.",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			id := "2026-05-12-1400-outcome-" + tc.name
			writeTicketFile(t, root, domain.TicketStatusProgress, id, "---\ntitle: Outcome round trip\nrepo: daemon\n---\n\nbody\n")

			service := NewTicketService()
			if _, err := service.ConcludeTicket(context.Background(), root, id, tc.conclusion); err != nil {
				t.Fatalf("ConcludeTicket: %v", err)
			}

			ticket, err := service.GetTicket(context.Background(), root, id)
			if err != nil {
				t.Fatalf("GetTicket: %v", err)
			}
			if ticket.Conclusion == nil || ticket.Conclusion.Outcome != tc.outcome {
				t.Fatalf("expected outcome %q, got %#v", tc.outcome, ticket.Conclusion)
			}

			content := readFile(t, filepath.Join(root, ticketsDirName, string(domain.TicketStatusDone), id, conclusionFileName))
			if !strings.Contains(content, "outcome: "+string(tc.outcome)) {
				t.Fatalf("expected outcome frontmatter key, got:\n%s", content)
			}
			if strings.Contains(content, "rejected:") {
				t.Fatalf("expected no legacy rejected key in newly written conclusion, got:\n%s", content)
			}
		})
	}
}

func TestTicketServiceReadConclusionInfersOutcomeForLegacyFileWithNoOutcomeOrRejectedKey(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-1500-legacy", "---\ntitle: Legacy conclusion\nrepo: daemon\n---\n\nbody\n")
	writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-1500-legacy", "---\nstarted_at: 2026-05-12T15:00:00Z\nconcluded_at: 2026-05-12T15:30:00Z\nagent: codex\ncommits:\n  - abc1234\n---\n\nDone before the outcome field existed.\n")

	service := NewTicketService()
	ticket, err := service.GetTicket(context.Background(), root, "2026-05-12-1500-legacy")
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.Conclusion == nil || ticket.Conclusion.Outcome != domain.TicketOutcomeCompleted {
		t.Fatalf("expected inferred outcome completed, got %#v", ticket.Conclusion)
	}
}

func TestTicketServiceCreateCollisionAndEditPreservesUnknownFrontmatter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Date(2026, 5, 12, 9, 15, 0, 0, time.UTC)
	writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0915-same-title", "---\ntitle: Same title\nrepo: daemon\ncreated: 2026-05-12T09:15:00Z\nupdated: 2026-05-12T09:15:00Z\ncustom_field: keep-me\n---\n\nalpha\nrepeat\nrepeat\n")

	service := NewTicketService()
	created, err := service.CreateTicket(context.Background(), root, domain.CreateTicketParams{
		Title:      "Same title",
		Repo:       "daemon",
		Body:       "new body",
		References: []string{"2026-05-12-0915-same-title"},
		Now:        now,
	})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	if created.ID != "2026-05-12-0915-same-title-2" {
		t.Fatalf("expected collision suffix, got %q", created.ID)
	}

	if _, err := service.EditTicket(context.Background(), root, "2026-05-12-0915-same-title", domain.EditTicketParams{
		OldString: "repeat",
		NewString: "done",
		Now:       now.Add(time.Minute),
	}); err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}

	updated, err := service.EditTicket(context.Background(), root, "2026-05-12-0915-same-title", domain.EditTicketParams{
		OldString:  "repeat",
		NewString:  "done",
		ReplaceAll: true,
		Now:        now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("EditTicket replaceAll: %v", err)
	}
	if strings.Count(updated.Body, "done") != 2 {
		t.Fatalf("expected both matches replaced, got %q", updated.Body)
	}

	updated, err = service.EditTicket(context.Background(), root, "2026-05-12-0915-same-title", domain.EditTicketParams{
		OldString: "alpha\ndone\ndone",
		NewString: "alpha\nomega\ndone",
		Now:       now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("EditTicket normalized: %v", err)
	}
	if !strings.Contains(updated.Body, "omega") {
		t.Fatalf("expected normalized edit to apply, got %q", updated.Body)
	}

	content := readFile(t, filepath.Join(root, ticketsDirName, string(domain.TicketStatusBacklog), "2026-05-12-0915-same-title", ticketFileName))
	if !strings.Contains(content, "custom_field: keep-me") {
		t.Fatalf("expected unknown frontmatter preserved, got:\n%s", content)
	}
}

func TestTicketServiceUpdateMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Date(2026, 5, 12, 9, 15, 0, 0, time.UTC)
	writeTicketFile(t, root, domain.TicketStatusBacklog, "target-ticket", "---\ntitle: Target\n---\n\nbody\n")
	writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0915-update-me", "---\ntitle: Update me\nrepo: daemon\ncustom_field: keep-me\nreferences: []\n---\n\nbody\n")

	service := NewTicketService()
	newTitle := "Renamed ticket"
	newRepo := "desktop"
	newReferences := []string{"target-ticket"}
	updated, err := service.UpdateTicketMetadata(context.Background(), root, "2026-05-12-0915-update-me", domain.UpdateTicketMetadataParams{
		Title:      &newTitle,
		Repo:       &newRepo,
		References: &newReferences,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("UpdateTicketMetadata: %v", err)
	}
	if updated.Title != newTitle || updated.Repo != newRepo || len(updated.References) != 1 || updated.References[0] != "target-ticket" {
		t.Fatalf("unexpected updated ticket: %#v", updated)
	}

	emptyRepo := ""
	emptyReferences := []string{}
	updated, err = service.UpdateTicketMetadata(context.Background(), root, "2026-05-12-0915-update-me", domain.UpdateTicketMetadataParams{
		Repo:       &emptyRepo,
		References: &emptyReferences,
		Now:        now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("UpdateTicketMetadata clear fields: %v", err)
	}
	if updated.Repo != "" || len(updated.References) != 0 {
		t.Fatalf("expected cleared metadata, got %#v", updated)
	}

	content := readFile(t, filepath.Join(root, ticketsDirName, string(domain.TicketStatusBacklog), "2026-05-12-0915-update-me", ticketFileName))
	if !strings.Contains(content, "custom_field: keep-me") {
		t.Fatalf("expected unknown frontmatter preserved, got:\n%s", content)
	}
	if strings.Contains(content, "repo:") {
		t.Fatalf("expected repo field removed, got:\n%s", content)
	}
}

func TestTicketServiceMoveAndDelete(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-move-me", "---\ntitle: Move me\n---\n\nbody\n")

	service := NewTicketService()
	moved, err := service.MoveTicket(context.Background(), root, "2026-05-12-0900-move-me", domain.MoveTicketParams{To: domain.TicketStatusProgress})
	if err != nil {
		t.Fatalf("MoveTicket: %v", err)
	}
	if moved.Status != domain.TicketStatusProgress {
		t.Fatalf("expected progress status, got %#v", moved)
	}
	if _, err := os.Stat(filepath.Join(root, ticketsDirName, string(domain.TicketStatusProgress), "2026-05-12-0900-move-me", ticketFileName)); err != nil {
		t.Fatalf("expected moved ticket on disk: %v", err)
	}

	moved, err = service.MoveTicket(context.Background(), root, "2026-05-12-0900-move-me", domain.MoveTicketParams{To: domain.TicketStatusBacklog})
	if err != nil {
		t.Fatalf("MoveTicket back to backlog: %v", err)
	}
	if moved.Status != domain.TicketStatusBacklog {
		t.Fatalf("expected backlog status, got %#v", moved)
	}

	if err := service.DeleteTicket(context.Background(), root, "2026-05-12-0900-move-me"); err != nil {
		t.Fatalf("DeleteTicket: %v", err)
	}
	if _, err := service.GetTicket(context.Background(), root, "2026-05-12-0900-move-me"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestTicketServiceValidationErrors(t *testing.T) {
	t.Parallel()

	t.Run("duplicate IDs across statuses", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-duplicate", "---\ntitle: One\n---\n\nbody\n")
		writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-duplicate", "---\ntitle: Two\n---\n\nbody\n")

		_, err := NewTicketService().ListTickets(context.Background(), root)
		var conflictErr *domain.ConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("expected conflict error, got %v", err)
		}
	})

	t.Run("missing ticket file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ticketsDirName, string(domain.TicketStatusBacklog), "2026-05-12-0900-missing"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		_, err := NewTicketService().ListTickets(context.Background(), root)
		var validationErr *domain.ValidationError
		if !errors.As(err, &validationErr) || validationErr.Field != "ticket.md" {
			t.Fatalf("expected ticket.md validation error, got %v", err)
		}
	})

	t.Run("invalid rejected conclusion", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-rejected", "---\ntitle: Rejected\n---\n\nbody\n")
		writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-rejected", "---\nstarted_at: 2026-05-12T09:00:00Z\nconcluded_at: 2026-05-12T09:05:00Z\nrejected: true\nrejection_reason: \"\"\n---\n\nnope\n")

		_, err := NewTicketService().GetTicket(context.Background(), root, "2026-05-12-0900-rejected")
		var validationErr *domain.ValidationError
		if !errors.As(err, &validationErr) || validationErr.Field != "rejection_reason" {
			t.Fatalf("expected rejection_reason validation error, got %v", err)
		}
	})

	t.Run("broken references on existing ticket", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-refs", "---\ntitle: Refs\nreferences:\n  - missing-ticket\n---\n\nbody\n")

		board, err := NewTicketService().ListTickets(context.Background(), root)
		if err != nil {
			t.Fatalf("expected ListTickets to succeed, got: %v", err)
		}
		if len(board.Backlog) != 1 {
			t.Fatalf("expected 1 backlog ticket, got %d", len(board.Backlog))
		}
		ticket := board.Backlog[0]
		if len(ticket.Warnings) != 1 || ticket.Warnings[0].Code != warningBrokenReference {
			t.Fatalf("expected 1 BROKEN_REFERENCE warning, got %#v", ticket.Warnings)
		}
		if !strings.Contains(ticket.Warnings[0].Message, "missing-ticket") {
			t.Fatalf("expected warning message to mention missing-ticket, got %q", ticket.Warnings[0].Message)
		}
	})

	t.Run("broken references on create", func(t *testing.T) {
		root := t.TempDir()
		ticket, err := NewTicketService().CreateTicket(context.Background(), root, domain.CreateTicketParams{
			Title:      "Bad refs",
			References: []string{"missing-ticket"},
		})
		if err != nil {
			t.Fatalf("expected CreateTicket to succeed, got: %v", err)
		}
		if ticket.Title != "Bad refs" {
			t.Fatalf("expected title Bad refs, got %q", ticket.Title)
		}
		if len(ticket.References) != 1 || ticket.References[0] != "missing-ticket" {
			t.Fatalf("expected reference to be stored, got %#v", ticket.References)
		}
		if len(ticket.Warnings) != 1 || ticket.Warnings[0].Code != warningBrokenReference {
			t.Fatalf("expected BROKEN_REFERENCE warning on create, got %#v", ticket.Warnings)
		}
	})

	t.Run("broken references on metadata update", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-update", "---\ntitle: Update\n---\n\nbody\n")
		references := []string{"missing-ticket"}

		ticket, err := NewTicketService().UpdateTicketMetadata(context.Background(), root, "2026-05-12-0900-update", domain.UpdateTicketMetadataParams{References: &references})
		if err != nil {
			t.Fatalf("expected UpdateTicketMetadata to succeed, got: %v", err)
		}
		if len(ticket.References) != 1 || ticket.References[0] != "missing-ticket" {
			t.Fatalf("expected reference to be stored, got %#v", ticket.References)
		}
		if len(ticket.Warnings) != 1 || ticket.Warnings[0].Code != warningBrokenReference {
			t.Fatalf("expected BROKEN_REFERENCE warning on metadata update, got %#v", ticket.Warnings)
		}
	})

	t.Run("broken references surface on body edit", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-edit-refs", "---\ntitle: Edit refs\nreferences:\n  - missing-ticket\n---\n\nalpha\n")

		ticket, err := NewTicketService().EditTicket(context.Background(), root, "2026-05-12-0900-edit-refs", domain.EditTicketParams{OldString: "alpha", NewString: "beta"})
		if err != nil {
			t.Fatalf("expected EditTicket to succeed, got: %v", err)
		}
		if len(ticket.Warnings) != 1 || ticket.Warnings[0].Code != warningBrokenReference {
			t.Fatalf("expected BROKEN_REFERENCE warning on body edit, got %#v", ticket.Warnings)
		}
	})

	t.Run("broken references surface on move", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-move-refs", "---\ntitle: Move refs\nreferences:\n  - missing-ticket\n---\n\nbody\n")

		ticket, err := NewTicketService().MoveTicket(context.Background(), root, "2026-05-12-0900-move-refs", domain.MoveTicketParams{To: domain.TicketStatusProgress})
		if err != nil {
			t.Fatalf("expected MoveTicket to succeed, got: %v", err)
		}
		if len(ticket.Warnings) != 1 || ticket.Warnings[0].Code != warningBrokenReference {
			t.Fatalf("expected BROKEN_REFERENCE warning on move, got %#v", ticket.Warnings)
		}
	})

	t.Run("no match edit", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-edit", "---\ntitle: Edit\n---\n\nalpha\nbeta\n")

		_, err := NewTicketService().EditTicket(context.Background(), root, "2026-05-12-0900-edit", domain.EditTicketParams{OldString: "missing", NewString: "delta"})
		var validationErr *domain.ValidationError
		if !errors.As(err, &validationErr) || validationErr.Field != "oldString" {
			t.Fatalf("expected oldString validation error, got %v", err)
		}
	})

	t.Run("edit only allowed in backlog", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusProgress, "2026-05-12-0900-progress-edit", "---\ntitle: Progress edit\n---\n\nalpha\n")
		writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-done-edit", "---\ntitle: Done edit\n---\n\nalpha\n")
		writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-done-edit", "---\nstarted_at: 2026-05-12T09:00:00Z\nconcluded_at: 2026-05-12T09:05:00Z\nrejected: false\n---\n\ndone\n")

		for _, id := range []string{"2026-05-12-0900-progress-edit", "2026-05-12-0900-done-edit"} {
			_, err := NewTicketService().EditTicket(context.Background(), root, id, domain.EditTicketParams{OldString: "alpha", NewString: "beta"})
			var validationErr *domain.ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != "ticket_id" {
				t.Fatalf("expected ticket_id validation error for %s, got %v", id, err)
			}
		}
	})

	t.Run("metadata update only allowed in backlog", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusProgress, "2026-05-12-0900-progress-update", "---\ntitle: Progress update\n---\n\nbody\n")
		writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-done-update", "---\ntitle: Done update\n---\n\nbody\n")
		writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-done-update", "---\nstarted_at: 2026-05-12T09:00:00Z\nconcluded_at: 2026-05-12T09:05:00Z\nrejected: false\n---\n\ndone\n")
		newTitle := "Updated"

		for _, id := range []string{"2026-05-12-0900-progress-update", "2026-05-12-0900-done-update"} {
			_, err := NewTicketService().UpdateTicketMetadata(context.Background(), root, id, domain.UpdateTicketMetadataParams{Title: &newTitle})
			var validationErr *domain.ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != "ticket_id" {
				t.Fatalf("expected ticket_id validation error for %s, got %v", id, err)
			}
		}
	})

	t.Run("delete only allowed in backlog", func(t *testing.T) {
		root := t.TempDir()
		writeTicketFile(t, root, domain.TicketStatusProgress, "2026-05-12-0900-progress-delete", "---\ntitle: Progress delete\n---\n\nbody\n")
		writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-done-delete", "---\ntitle: Done delete\n---\n\nbody\n")
		writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-0900-done-delete", "---\nstarted_at: 2026-05-12T09:00:00Z\nconcluded_at: 2026-05-12T09:05:00Z\nrejected: false\n---\n\ndone\n")

		for _, id := range []string{"2026-05-12-0900-progress-delete", "2026-05-12-0900-done-delete"} {
			err := NewTicketService().DeleteTicket(context.Background(), root, id)
			var validationErr *domain.ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != "ticket_id" {
				t.Fatalf("expected ticket_id validation error for %s, got %v", id, err)
			}
		}
	})
}

func TestMixedTicketAndPathReferences(t *testing.T) {
	root := t.TempDir()
	pathDir := t.TempDir()
	pathFile := filepath.Join(pathDir, "spec.md")
	if err := os.WriteFile(pathFile, []byte("spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(pathDir, "missing")
	writeTicketFile(t, root, domain.TicketStatusBacklog, "target", "---\ntitle: Target\n---\n")
	ticket, err := NewTicketService().CreateTicket(context.Background(), root, domain.CreateTicketParams{Title: "Mixed", References: []string{"target", pathDir + "/.", pathFile, missing}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ticket.ResolvedReferences) != 4 {
		t.Fatalf("resolved = %#v", ticket.ResolvedReferences)
	}
	if ref := ticket.ResolvedReferences[0]; ref.Type != domain.TicketReferenceTicket || !ref.Exists {
		t.Fatalf("ticket = %#v", ref)
	}
	if ref := ticket.ResolvedReferences[1]; ref.Kind != domain.PathReferenceDirectory || !ref.Exists {
		t.Fatalf("directory = %#v", ref)
	}
	if ref := ticket.ResolvedReferences[2]; ref.Kind != domain.PathReferenceFile || !ref.Exists {
		t.Fatalf("file = %#v", ref)
	}
	if ref := ticket.ResolvedReferences[3]; ref.Exists || ref.Kind != "" {
		t.Fatalf("missing = %#v", ref)
	}
	if len(ticket.Warnings) != 1 || ticket.Warnings[0].Code != warningMissingPathReference {
		t.Fatalf("warnings = %#v", ticket.Warnings)
	}
	_, err = NewTicketService().CreateTicket(context.Background(), root, domain.CreateTicketParams{Title: "Duplicate", References: []string{pathDir, pathDir + "/."}})
	if err == nil {
		t.Fatal("expected normalized duplicate rejection")
	}
}

func TestTicketServiceEmptySlicesAreNonNull(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTicketFile(t, root, domain.TicketStatusBacklog, "2026-05-12-0900-empty-slices", "---\ntitle: Empty slices\n---\n\nbody\n")
	writeTicketFile(t, root, domain.TicketStatusDone, "2026-05-12-1000-done-empty", "---\ntitle: Done empty\n---\n\ndone\n")
	writeConclusionFile(t, root, domain.TicketStatusDone, "2026-05-12-1000-done-empty", "---\nstarted_at: 2026-05-12T10:00:00Z\nconcluded_at: 2026-05-12T10:30:00Z\nrejected: false\n---\n\nok\n")

	service := NewTicketService()

	board, err := service.ListTickets(context.Background(), root)
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	for _, summary := range append(append(board.Backlog, board.Progress...), board.Done...) {
		if summary.References == nil {
			t.Fatalf("expected References to be non-nil slice for ticket %s", summary.ID)
		}
		if summary.Warnings == nil {
			t.Fatalf("expected Warnings to be non-nil slice for ticket %s", summary.ID)
		}
	}

	ticket, err := service.GetTicket(context.Background(), root, "2026-05-12-0900-empty-slices")
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.References == nil {
		t.Fatal("expected References to be non-nil slice on detail ticket")
	}
	if ticket.Warnings == nil {
		t.Fatal("expected Warnings to be non-nil slice on detail ticket")
	}
	if ticket.Conclusion != nil {
		t.Fatal("expected Conclusion to be nil on ticket without conclusion")
	}

	done, err := service.GetTicket(context.Background(), root, "2026-05-12-1000-done-empty")
	if err != nil {
		t.Fatalf("GetTicket done: %v", err)
	}
	if done.Conclusion == nil {
		t.Fatal("expected non-nil conclusion on done ticket")
	}
	if done.Conclusion.Commits == nil {
		t.Fatal("expected Commits to be non-nil slice in conclusion")
	}
}

func writeTicketFile(t *testing.T, root string, status domain.TicketStatus, id, content string) {
	t.Helper()
	dir := filepath.Join(root, ticketsDirName, string(status), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir ticket dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ticketFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}
}

func writeConclusionFile(t *testing.T, root string, status domain.TicketStatus, id, content string) {
	t.Helper()
	dir := filepath.Join(root, ticketsDirName, string(status), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir conclusion dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, conclusionFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write conclusion file: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return string(data)
}
