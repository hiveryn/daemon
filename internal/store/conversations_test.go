package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestConversationQueueDurabilityAndIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewSessionStore(db)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := repo.EnqueueSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "same", Text: "one"}, "user")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- sub.ID
		}()
	}
	wg.Wait()
	close(ids)
	var id string
	for got := range ids {
		if id != "" && id != got {
			t.Fatal("duplicate submission")
		}
		id = got
	}
	_, err = repo.EnqueueSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "same", Text: "different"}, "user")
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	second, err := repo.EnqueueSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "second", Text: "two"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.ReserveSubmission(ctx, "s")
	if err != nil || first == nil || first.ID != id {
		t.Fatalf("reserve: %#v %v", first, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repo = NewSessionStore(db)
	if err = repo.SettleSubmissions(ctx, "s", "uncertain", "restart"); err != nil {
		t.Fatal(err)
	}
	if next, err := repo.ReserveSubmission(ctx, "s"); err != nil || next != nil {
		t.Fatalf("replayed uncertain turn: %#v %v", next, err)
	}
	first.Status = "completed"
	if err = repo.UpdateSubmission(ctx, *first); err != nil {
		t.Fatal(err)
	}
	next, err := repo.ReserveSubmission(ctx, "s")
	if err != nil || next == nil || next.ID != second.ID {
		t.Fatalf("queue order: %#v %v", next, err)
	}
	if err = repo.SettleSubmissions(ctx, "s", "failed", "discard"); err != nil {
		t.Fatal(err)
	}
	next.Status = "completed"
	if err = repo.UpdateSubmission(ctx, *next); err != nil {
		t.Fatal(err)
	}
	settled, err := repo.GetSubmission(ctx, "s", next.ID)
	if err != nil || settled.Status != "failed" {
		t.Fatalf("late completion overwrote discard: %#v %v", settled, err)
	}
}
func TestConversationRevisionSnapshotSurvivesUpdatesAndCompaction(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewSessionStore(db)
	for i := range 3 {
		if err = repo.PutConversationItem(ctx, "s", domain.ConversationItem{ID: fmt.Sprint(i), Text: "original", Role: "assistant", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{Limit: 1})
	if err != nil || !page.HasMore {
		t.Fatalf("page: %#v %v", page, err)
	}
	snapshot := page.SnapshotRevision
	if err = repo.PutConversationItem(ctx, "s", domain.ConversationItem{ID: "0", Text: "updated", Role: "assistant"}); err != nil {
		t.Fatal(err)
	}
	rest, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{AfterRevision: page.NextRevision, SnapshotRevision: snapshot, Limit: 200})
	if err != nil || len(rest.Items) != 2 {
		t.Fatalf("snapshot moved: %#v %v", rest, err)
	}
	delta, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{AfterRevision: snapshot})
	if err != nil || len(delta.Items) != 1 || delta.Items[0].Text != "updated" || delta.Items[0].Sequence != page.Items[0].Sequence {
		t.Fatalf("old-item update missing: %#v %v", delta, err)
	}
	// Provider compaction yields no earlier items. An empty poll never deletes
	// observed history; snapshots can still reconstruct all three stable IDs.
	all, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{})
	if err != nil || len(all.Items) != 4 {
		t.Fatalf("history lost: %#v %v", all, err)
	}
}

func TestChatArchiveRetainsIdentityAndInterruptsWithoutSuccess(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewSessionStore(db)
	sub, err := repo.EnqueueSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "one", Text: "hello"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []domain.ConversationItem{
		{ID: "tool", Role: "tool", Tool: "conclude", Status: "inProgress", SubmissionID: sub.ID},
		{ID: "failed", Role: "tool", Tool: "Read", Status: "failed", Text: "native failure", SubmissionID: sub.ID},
		{ID: "complete", Role: "tool", Tool: "Read", Status: "started", SubmissionID: sub.ID},
		{ID: "complete", Role: "tool", Status: "completed", Text: "output", SubmissionID: sub.ID},
	} {
		if err = repo.PutConversationItem(ctx, "s", item); err != nil {
			t.Fatal(err)
		}
	}
	before, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{})
	if err != nil {
		t.Fatal(err)
	}
	chat := domain.ArchivedChat{ID: "s", ArchitectKey: "project", SessionType: domain.SessionTypeTicket, ContextID: "ticket", Agent: "claude", Model: "sonnet", ProfileName: "work", Lifecycle: "concluded", EndedAt: time.Now().UTC()}
	if err = repo.ArchiveChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if err = repo.ArchiveChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if err = repo.PutConversationItem(ctx, "s", domain.ConversationItem{ID: "tool", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	page, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{AfterRevision: before.SnapshotRevision})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Status != "interrupted" || page.Items[0].Tool != "conclude" {
		t.Fatalf("final revisions: %#v", page)
	}
	archives, err := repo.ListArchivedChats(ctx, domain.SequenceQuery{ArchitectKey: "project", Limit: 1})
	if err != nil || len(archives.Chats) != 1 || archives.Chats[0].Agent != "claude" || archives.Chats[0].Lifecycle != "concluded" {
		t.Fatalf("archives: %#v %v", archives, err)
	}
	other, err := repo.ListArchivedChats(ctx, domain.SequenceQuery{ArchitectKey: "other"})
	if err != nil || len(other.Chats) != 0 {
		t.Fatalf("scope: %#v %v", other, err)
	}
	receipt, err := repo.GetSubmission(ctx, "s", sub.ID)
	if err != nil || receipt.Status != "failed" {
		t.Fatalf("receipt %#v %v", receipt, err)
	}
	// Prior complete revisions retain identity even when the result omitted it.
	if before.Items[len(before.Items)-1].Tool != "Read" {
		t.Fatal("lost tool identity")
	}
}

func TestSubmissionDiscoveryPagesAndCurrentStates(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewSessionStore(db)
	for _, sid := range []string{"s", "other", "s", "s"} {
		_, err = repo.EnqueueSubmission(ctx, sid, domain.SubmissionRequest{RequestID: fmt.Sprint(time.Now().UnixNano()), Text: "test"}, "user")
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := repo.ListSubmissions(ctx, "s", domain.SequenceQuery{Limit: 2})
	if err != nil || len(first.Submissions) != 2 || !first.HasMore {
		t.Fatalf("first %#v %v", first, err)
	}
	next, err := repo.ListSubmissions(ctx, "s", domain.SequenceQuery{Limit: 2, AfterSequence: first.NextSequence})
	if err != nil || len(next.Submissions) != 1 || next.HasMore {
		t.Fatalf("next %#v %v", next, err)
	}
	head, err := repo.ReserveSubmission(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	head.Status = "uncertain"
	head.Error = "unknown acceptance"
	if err = repo.UpdateSubmission(ctx, *head); err != nil {
		t.Fatal(err)
	}
	again, err := repo.ListSubmissions(ctx, "s", domain.SequenceQuery{Limit: 1})
	if err != nil || again.Submissions[0].Status != "uncertain" {
		t.Fatalf("updated %#v %v", again, err)
	}
}
