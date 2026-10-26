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
