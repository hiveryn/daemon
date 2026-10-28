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
			sub, _, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "same", Text: "one"}, "kickoff", domain.SubmissionQueued, "", "")
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
	_, _, err = repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "same", Text: "different"}, "kickoff", domain.SubmissionQueued, "", "")
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	second, _, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "second", Text: "two"}, "notification", domain.SubmissionQueued, "", "")
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
	// The reserved head was handed over before the restart: unknown, held.
	if err = repo.ReconcileDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetSubmission(ctx, "s", id); got.Status != domain.SubmissionUnknown || got.ErrorCode != domain.DeliveryUnknown {
		t.Fatalf("reserved before restart = %#v", got)
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
	// Provenance is part of an item's content: an unchanged put adds no
	// revision, a changed origin/via does.
	if err = repo.PutConversationItem(ctx, "s", domain.ConversationItem{ID: "1", Text: "original", Role: "assistant"}); err != nil {
		t.Fatal(err)
	}
	if err = repo.PutConversationItem(ctx, "s", domain.ConversationItem{ID: "1", Text: "original", Role: "user", Origin: "kickoff", Via: "kickoff"}); err != nil {
		t.Fatal(err)
	}
	after, err := repo.ReadConversation(ctx, "s", domain.ConversationQuery{AfterRevision: all.SnapshotRevision})
	if err != nil || len(after.Items) != 1 || after.Items[0].Via != "kickoff" || after.Items[0].Origin != "kickoff" {
		t.Fatalf("provenance revision: %#v %v", after, err)
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
	sub, _, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "one", Text: "hello"}, "notification", domain.SubmissionQueued, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// A user send whose hand-off was reserved but never confirmed ends
	// unconfirmed, not failed; one never handed over ends rejected.
	handed, _, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "two", Text: "handed"}, "user", domain.SubmissionDelivering, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ReserveTerminalSubmission(ctx, "s", handed.ID, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	waiting, _, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "three", Text: "waiting"}, "user", domain.SubmissionDelivering, "", "")
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
	if receipt, _ = repo.GetSubmission(ctx, "s", handed.ID); receipt.Status != domain.SubmissionUncertain {
		t.Fatalf("handed receipt %#v", receipt)
	}
	if receipt, _ = repo.GetSubmission(ctx, "s", waiting.ID); receipt.Status != domain.SubmissionRejected || receipt.ErrorCode != domain.DeliverySessionEnding {
		t.Fatalf("waiting receipt %#v", receipt)
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
		_, _, err = repo.InsertSubmission(ctx, sid, domain.SubmissionRequest{RequestID: fmt.Sprint(time.Now().UnixNano()), Text: "test"}, "notification", domain.SubmissionQueued, "", "")
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

func TestSynchronousDeliveryStatesNeverRequeue(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewSessionStore(db)
	insert := func(request, status, code string) domain.Submission {
		t.Helper()
		sub, created, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: request, Text: request}, "user", status, code, "")
		if err != nil || !created {
			t.Fatalf("insert %s: %v %v", request, created, err)
		}
		return sub
	}
	// A rejected send is final: never reserved, never revived by evidence.
	rejected := insert("busy", domain.SubmissionRejected, domain.DeliverySessionBusy)
	if again, created, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "busy", Text: "busy"}, "user", domain.SubmissionDelivering, "", ""); err != nil || created || again.Status != domain.SubmissionRejected {
		t.Fatalf("replayed rejection = %#v %v %v", again, created, err)
	}
	rejected.Status = domain.SubmissionRunning
	if err := repo.UpdateSubmission(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetSubmission(ctx, "s", rejected.ID); got.Status != domain.SubmissionRejected {
		t.Fatalf("rejection revived: %#v", got)
	}
	if head, err := repo.ReserveSubmission(ctx, "s"); err != nil || head != nil {
		t.Fatalf("rejected entry reserved: %#v %v", head, err)
	}

	// An unreserved attempt can still be withdrawn as rejected; once a
	// hand-off is reserved only unknown remains.
	withdrawn := insert("withdrawn", domain.SubmissionDelivering, "")
	if ok, err := repo.SettleDelivery(ctx, "s", withdrawn.ID, domain.SubmissionRejected, domain.DeliveryRejected, "deadline"); err != nil || !ok {
		t.Fatalf("withdraw = %v %v", ok, err)
	}
	handed := insert("handed", domain.SubmissionDelivering, "")
	if ok, err := repo.ReserveTerminalSubmission(ctx, "s", handed.ID, "ref", "term", time.Now()); err != nil || !ok {
		t.Fatalf("reserve = %v %v", ok, err)
	}
	if ok, err := repo.SettleDelivery(ctx, "s", handed.ID, domain.SubmissionRejected, domain.DeliveryRejected, "deadline"); err != nil || ok {
		t.Fatalf("reserved hand-off withdrawn as rejected: %v %v", ok, err)
	}
	if ok, err := repo.SettleDelivery(ctx, "s", handed.ID, domain.SubmissionUnknown, domain.DeliveryUnknown, "ambiguous"); err != nil || !ok {
		t.Fatalf("unknown = %v %v", ok, err)
	}

	// Acceptance once recorded is kept through later turn states.
	accepted := insert("accepted", domain.SubmissionDelivering, "")
	at := time.Now().UTC()
	accepted.Status, accepted.AcceptedAt, accepted.Acceptance = domain.SubmissionAccepted, &at, "codex_thread_queue"
	if err := repo.UpdateSubmission(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	accepted.Status, accepted.AcceptedAt, accepted.Acceptance = domain.SubmissionCompleted, nil, ""
	if err := repo.UpdateSubmission(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetSubmission(ctx, "s", accepted.ID)
	if err != nil || got.Status != domain.SubmissionCompleted || got.AcceptedAt == nil || got.Acceptance != "codex_thread_queue" {
		t.Fatalf("acceptance lost: %#v %v", got, err)
	}

	// Restart: an unreserved attempt was never handed over.
	orphan := insert("orphan", domain.SubmissionDelivering, "")
	if err := repo.ReconcileDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetSubmission(ctx, "s", orphan.ID); got.Status != domain.SubmissionRejected || got.ErrorCode != domain.DeliveryInterrupted {
		t.Fatalf("orphan = %#v", got)
	}
}

func TestCutoverSettlesFormerQueuedUserInputWithoutDispatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// Rows as the former queue left them, then the cutover statement replayed.
	rows := []struct{ id, origin, status, turn, handed string }{
		{"q-user", "user", "queued", "", ""},
		{"q-kick", "kickoff", "queued", "", ""},
		{"s-run", "user", "started", "turn-1", "2026-10-27T10:00:00Z"},
		{"s-amb", "user", "started", "", "2026-10-27T10:00:00Z"},
		{"done", "user", "completed", "turn-0", ""},
	}
	for i, r := range rows {
		if _, err := db.ExecContext(ctx, "INSERT INTO conversation_submissions(id,session_id,request_id,origin,text,status,native_turn_id,handed_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
			r.id, "s", fmt.Sprint(i), r.origin, "text "+r.id, r.status, r.turn, r.handed, "2026-10-27T09:00:00Z", "2026-10-27T09:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version=?", len(migrationFiles)); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"error_code", "accepted_at", "acceptance"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE conversation_submissions DROP COLUMN "+column); err != nil {
			t.Fatal(err)
		}
	}
	if err := runMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewSessionStore(db)
	want := map[string]string{"q-user": "rejected", "q-kick": "queued", "s-run": "running", "s-amb": "unknown", "done": "completed"}
	for id, status := range want {
		got, err := repo.GetSubmission(ctx, "s", id)
		if err != nil || got.Status != status || got.Text != "text "+id {
			t.Fatalf("%s = %#v %v, want %s with text kept", id, got, err, status)
		}
	}
	if got, _ := repo.GetSubmission(ctx, "s", "q-user"); got.ErrorCode != domain.DeliveryQueuedBeforeCutover {
		t.Fatalf("cutover code = %#v", got)
	}
	if got, _ := repo.GetSubmission(ctx, "s", "s-run"); got.AcceptedAt == nil || got.Acceptance != "native_record" {
		t.Fatalf("running acceptance = %#v", got)
	}
	if got, _ := repo.GetSubmission(ctx, "s", "done"); got.AcceptedAt == nil {
		t.Fatalf("completed without acceptance = %#v", got)
	}
	if got, _ := repo.GetSubmission(ctx, "s", "s-amb"); got.AcceptedAt != nil || got.ErrorCode != domain.DeliveryUnknown {
		t.Fatalf("ambiguous = %#v", got)
	}
}
