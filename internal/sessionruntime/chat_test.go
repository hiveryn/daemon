package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

type controlledConversation struct {
	entered chan agentruntime.ConversationInput
	release chan error
}

func (c *controlledConversation) NativeID() string { return "native" }
func (c *controlledConversation) Turn(ctx context.Context, in agentruntime.ConversationInput, observe func(agentruntime.ConversationEvent) error) error {
	select {
	case c.entered <- in:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.release:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := observe(agentruntime.ConversationEvent{ID: in.ID + ":tool", Role: "tool", Status: "failed", Text: "fixture tool failed", TurnID: in.ID, At: time.Now()}); err != nil {
		return err
	}
	return observe(agentruntime.ConversationEvent{ID: in.ID + ":done", Role: "system", Status: "completed", Complete: true, TurnID: in.ID, At: time.Now()})
}
func TestChatHTTPConcurrentAdmissionBusyAndAmbiguousHandoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := store.NewSessionStore(db)
	session, err := repo.CreateSession(ctx, domain.CreateSessionParams{ID: "chat", InteractionMode: domain.InteractionChat, SessionType: domain.SessionTypeTicket, ContextID: "ticket", ArchitectKey: "project"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "fixture", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	conn := &controlledConversation{entered: make(chan agentruntime.ConversationInput, 20), release: make(chan error)}
	owner := &chatOwner{conn: conn, wake: make(chan struct{}, 1), ready: make(chan struct{})}
	close(owner.ready)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := &Service{repo: repo, logger: logger, chats: map[string]*chatOwner{session.ID: owner}}
	finished := make(chan struct{})
	go func() { defer close(finished); service.runChat(ctx, session, run, owner) }()
	defer func() { cancel(); <-finished }()
	server := httptest.NewServer(api.NewHandler(api.Dependencies{Sessions: service, Logger: logger}))
	defer server.Close()
	post := func(key, text string) (int, domain.Submission) {
		data, _ := json.Marshal(domain.SubmissionRequest{RequestID: key, Text: text})
		res, err := http.Post(server.URL+"/api/sessions/chat/messages", "application/json", bytes.NewReader(data))
		if err != nil {
			t.Error(err)
			return 0, domain.Submission{}
		}
		defer func() { _ = res.Body.Close() }()
		var envelope struct {
			Data domain.Submission `json:"data"`
		}
		if err = json.NewDecoder(res.Body).Decode(&envelope); err != nil {
			t.Error(err)
		}
		return res.StatusCode, envelope.Data
	}
	var wg sync.WaitGroup
	subs := make(chan domain.Submission, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, sub := post(fmt.Sprint(i), "hello")
			if status != 202 {
				t.Errorf("HTTP %d", status)
			}
			subs <- sub
		}()
	}
	wg.Wait()
	close(subs)
	first := <-conn.entered
	var firstSub domain.Submission
	for sub := range subs {
		if sub.ID == first.ID {
			firstSub = sub
		}
	}
	if status, replay := post(firstSub.RequestID, "hello"); status != 202 || replay.ID != first.ID {
		t.Fatalf("replay failed: %d %#v", status, replay)
	}
	if status, _ := post(firstSub.RequestID, "different"); status != 409 {
		t.Fatalf("conflicting reuse HTTP %d", status)
	}
	select {
	case <-conn.entered:
		t.Fatal("concurrent native turns")
	case <-time.After(30 * time.Millisecond):
	}
	// All HTTP clients have disconnected; releasing the native turn still
	// completes it and starts the next queued submission.
	conn.release <- nil
	second := <-conn.entered
	receipt, err := repo.GetSubmission(ctx, "chat", first.ID)
	if err != nil || receipt.Status != "completed" {
		t.Fatalf("completion: %#v %v", receipt, err)
	}
	history, err := repo.ReadConversation(ctx, "chat", domain.ConversationQuery{})
	if err != nil || history.Items[0].Status != "failed" {
		t.Fatalf("tool failure lost: %#v %v", history, err)
	}
	conn.release <- errors.New("transport closed after write")
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	receipt, err = repo.GetSubmission(ctx, "chat", second.ID)
	if err != nil || receipt.Status != "uncertain" {
		t.Fatalf("ambiguous handoff: %#v %v", receipt, err)
	}
	if next, err := repo.ReserveSubmission(ctx, "chat"); err != nil || next != nil {
		t.Fatalf("uncertain turn replayed: %#v %v", next, err)
	}
	if err = repo.SettleSubmissions(ctx, "chat", "failed", "concluded"); err != nil {
		t.Fatal(err)
	}
}

type receiptConversation struct {
	controlledConversation
	events []agentruntime.ConversationEvent
}

func (c *receiptConversation) Reconcile(context.Context, agentruntime.ConversationInput, string) ([]agentruntime.ConversationEvent, error) {
	return c.events, nil
}
func TestChatReconcilesExplicitNativeCompletionBeforeQueueRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := store.NewSessionStore(db)
	service := &Service{repo: repo}
	first, err := repo.EnqueueSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "one", Text: "first"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.EnqueueSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "two", Text: "second"}, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ReserveSubmission(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	conn := &receiptConversation{}
	if err = service.reconcileChat(ctx, "s", conn); err != nil {
		t.Fatal(err)
	}
	if next, err := repo.ReserveSubmission(ctx, "s"); err != nil || next != nil {
		t.Fatalf("missing native evidence authorized replay: %#v %v", next, err)
	}
	conn.events = []agentruntime.ConversationEvent{{ID: "native", Role: "assistant", Text: "answer", Status: "completed"}, {ID: "turn", Role: "system", Status: "completed", Complete: true}}
	if err = service.reconcileChat(ctx, "s", conn); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetSubmission(ctx, "s", first.ID)
	if err != nil || got.Status != "completed" {
		t.Fatalf("receipt %#v %v", got, err)
	}
	if next, err := repo.ReserveSubmission(ctx, "s"); err != nil || next == nil || next.RequestID != "two" {
		t.Fatalf("queue did not recover: %#v %v", next, err)
	}
}
func TestChatLaunchDiagnosticsRedactConfiguredCredentials(t *testing.T) {
	d := newChatDiagnostics([]string{"API_KEY=secret-example", "NORMAL=normal"})
	_, _ = d.Write([]byte("auth failed: secret-example Bearer hidden-token sk-test123456789012; original failure retained"))
	text := d.String()
	for _, secret := range []string{"secret-example", "hidden-token", "sk-test123456789012"} {
		if strings.Contains(text, secret) {
			t.Fatalf("credential exposed: %s", text)
		}
	}
	if !strings.Contains(text, "original failure retained") {
		t.Fatal("lost diagnostic")
	}
}
