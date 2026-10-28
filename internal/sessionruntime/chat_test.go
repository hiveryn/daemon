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
	// silent withholds the acceptance event (an ambiguous hand-off).
	silent bool
}

func (c *controlledConversation) NativeID() string { return "native" }
func (c *controlledConversation) Turn(ctx context.Context, in agentruntime.ConversationInput, observe func(agentruntime.ConversationEvent) error) error {
	select {
	case c.entered <- in:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !c.silent {
		if err := observe(agentruntime.ConversationEvent{ID: in.ID, Role: "user", Text: in.Text, Status: "completed", TurnID: in.ID, At: time.Now(), Acceptance: "fixture_echo"}); err != nil {
			return err
		}
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

type postResult struct {
	status int
	data   domain.Submission
	code   string
	// receipt is the durable receipt carried by a delivery error.
	receipt *domain.Submission
}

func postMessage(t *testing.T, url, key, text string) postResult {
	data, _ := json.Marshal(domain.SubmissionRequest{RequestID: key, Text: text})
	res, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Error(err)
		return postResult{}
	}
	defer func() { _ = res.Body.Close() }()
	var envelope struct {
		Data  domain.Submission `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Details struct {
				Submission *domain.Submission `json:"submission"`
			} `json:"details"`
		} `json:"error"`
	}
	if err = json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		t.Error(err)
	}
	out := postResult{status: res.StatusCode, data: envelope.Data}
	if envelope.Error != nil {
		out.code, out.receipt = envelope.Error.Code, envelope.Error.Details.Submission
	}
	return out
}

type chatHarness struct {
	repo    *store.SessionStore
	service *Service
	conn    *controlledConversation
	url     string
	run     domain.SessionRun
	session domain.Session
}

func newChatHarness(t *testing.T, ctx context.Context, startOwner bool) *chatHarness {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := store.NewSessionStore(db)
	session, err := repo.CreateSession(ctx, domain.CreateSessionParams{ID: "chat", InteractionMode: domain.InteractionChat, SessionType: domain.SessionTypeTicket, ContextID: "ticket", ArchitectKey: "project"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "fixture", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	session, _ = repo.GetSession(ctx, session.ID)
	conn := &controlledConversation{entered: make(chan agentruntime.ConversationInput, 20), release: make(chan error)}
	owner := &chatOwner{conn: conn, wake: make(chan struct{}, 1), ready: make(chan struct{}), exited: make(chan struct{})}
	close(owner.ready)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := &Service{repo: repo, logger: logger, chats: map[string]*chatOwner{session.ID: owner}}
	if startOwner {
		runCtx, cancel := context.WithCancel(ctx)
		finished := make(chan struct{})
		go func() { defer close(finished); service.runChat(runCtx, session, run, owner) }()
		t.Cleanup(func() { cancel(); <-finished })
	}
	server := httptest.NewServer(api.NewHandler(api.Dependencies{Sessions: service, Logger: logger}))
	t.Cleanup(server.Close)
	return &chatHarness{repo: repo, service: service, conn: conn, url: server.URL + "/api/sessions/chat/messages", run: run, session: session}
}

func TestChatSendIsAcceptedSynchronouslyOrRejectedWhileBusy(t *testing.T) {
	ctx := context.Background()
	h := newChatHarness(t, ctx, true)
	// Concurrent sends: exactly one is taken; every other is refused at once
	// as busy, never kept for later.
	var wg sync.WaitGroup
	results := make(chan postResult, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- postMessage(t, h.url, fmt.Sprint(i), "hello")
		}()
	}
	wg.Wait()
	close(results)
	var accepted domain.Submission
	busy := 0
	for r := range results {
		switch {
		case r.status == 200:
			if accepted.ID != "" {
				t.Fatal("two sends accepted into one busy chat")
			}
			accepted = r.data
		case r.status == 409 && r.code == domain.DeliverySessionBusy && r.receipt != nil && r.receipt.Status == domain.SubmissionRejected:
			busy++
		default:
			t.Fatalf("unexpected result %#v", r)
		}
	}
	if accepted.ID == "" || busy != 11 || accepted.AcceptedAt == nil || accepted.Acceptance != "fixture_echo" || accepted.Status != domain.SubmissionRunning {
		t.Fatalf("accepted %#v busy %d", accepted, busy)
	}
	first := <-h.conn.entered
	if first.ID != accepted.ID {
		t.Fatalf("turn for %s, accepted %s", first.ID, accepted.ID)
	}
	// The same request replays its outcome; different text is a conflict.
	if r := postMessage(t, h.url, accepted.RequestID, "hello"); r.status != 200 || r.data.ID != accepted.ID {
		t.Fatalf("replay failed: %#v", r)
	}
	if r := postMessage(t, h.url, accepted.RequestID, "different"); r.status != 409 || r.code != string(domain.ErrCodeConflict) {
		t.Fatalf("conflicting reuse %#v", r)
	}
	// A rejected request stays rejected: replaying it never sends it.
	if r := postMessage(t, h.url, "0", "hello"); accepted.RequestID != "0" && (r.code != domain.DeliverySessionBusy || r.receipt.Status != domain.SubmissionRejected) {
		t.Fatalf("rejected replay %#v", r)
	}
	h.conn.release <- nil
	waitFor(t, func() bool {
		got, _ := h.repo.GetSubmission(ctx, "chat", accepted.ID)
		return got.Status == domain.SubmissionCompleted
	})
	select {
	case in := <-h.conn.entered:
		t.Fatalf("a rejected send reached the provider later: %#v", in)
	case <-time.After(50 * time.Millisecond):
	}
	history, err := h.repo.ReadConversation(ctx, "chat", domain.ConversationQuery{})
	if err != nil || len(history.Items) < 2 {
		t.Fatalf("history: %#v %v", history, err)
	}
	// Idle again: the next send is accepted; a turn that then fails after
	// acceptance is uncertain and holds the chat.
	next := postMessage(t, h.url, "next", "again")
	if next.status != 200 {
		t.Fatalf("next %#v", next)
	}
	<-h.conn.entered
	h.conn.release <- errors.New("transport closed after write")
	waitFor(t, func() bool {
		got, _ := h.repo.GetSubmission(ctx, "chat", next.data.ID)
		return got.Status == domain.SubmissionUncertain && got.AcceptedAt != nil
	})
	if r := postMessage(t, h.url, "held", "held"); r.code != domain.DeliverySessionBusy || !strings.Contains(r.receipt.Error, "holds the chat") {
		t.Fatalf("held chat admitted input: %#v", r)
	}
}

func TestChatSendWithoutAcceptanceIsUnknownAndUntakenIsRejected(t *testing.T) {
	bound := submitDeliveryBound
	submitDeliveryBound = 200 * time.Millisecond
	t.Cleanup(func() { submitDeliveryBound = bound })
	ctx := context.Background()

	// The owner takes the input but no acceptance evidence arrives in time.
	h := newChatHarness(t, ctx, true)
	h.conn.silent = true
	r := postMessage(t, h.url, "ambiguous", "hello")
	if r.status != 504 || r.code != domain.DeliveryUnknown || r.receipt == nil || r.receipt.Status != domain.SubmissionUnknown {
		t.Fatalf("ambiguous %#v", r)
	}
	in := <-h.conn.entered
	// Replaying it reports the same unknown outcome and never sends again.
	if again := postMessage(t, h.url, "ambiguous", "hello"); again.code != domain.DeliveryUnknown || again.receipt.ID != in.ID {
		t.Fatalf("replay %#v", again)
	}
	select {
	case <-h.conn.entered:
		t.Fatal("unknown input was resent")
	case <-time.After(50 * time.Millisecond):
	}
	// Later native completion settles it: completion implies acceptance.
	h.conn.release <- nil
	waitFor(t, func() bool {
		got, _ := h.repo.GetSubmission(ctx, "chat", in.ID)
		return got.Status == domain.SubmissionCompleted
	})

	// An owner that never takes the input: rejected, and it stays out.
	idle := newChatHarness(t, ctx, false)
	r = postMessage(t, idle.url, "untaken", "hello")
	if r.status != 502 || r.code != domain.DeliveryRejected || r.receipt.Status != domain.SubmissionRejected {
		t.Fatalf("untaken %#v", r)
	}
	if head, err := idle.repo.ReserveSubmission(ctx, "chat"); err != nil || head != nil {
		t.Fatalf("rejected input is still deliverable: %#v %v", head, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
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
	first, _, err := repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "one", Text: "first"}, "kickoff", domain.SubmissionQueued, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = repo.InsertSubmission(ctx, "s", domain.SubmissionRequest{RequestID: "two", Text: "second"}, "notification", domain.SubmissionQueued, "", ""); err != nil {
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
	if err != nil || got.Status != "completed" || got.AcceptedAt == nil {
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

func TestCrossClientDiscoveryRedactionAndEmptyArchive(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := store.NewSessionStore(db)
	session, err := repo.CreateSession(ctx, domain.CreateSessionParams{ID: "chat", InteractionMode: domain.InteractionChat, SessionType: domain.SessionTypeTicket, ArchitectKey: "p", ContextID: "ticket"})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: repo}
	hidden, _, err := repo.InsertSubmission(ctx, session.ID, domain.SubmissionRequest{RequestID: "launch", Text: "PRIVATE INSTRUCTIONS"}, "kickoff", domain.SubmissionQueued, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ReserveSubmission(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	hidden.Status = "blocked"
	hidden.Error = "permission unavailable"
	if err = repo.UpdateSubmission(ctx, hidden); err != nil {
		t.Fatal(err)
	}
	if _, _, err = repo.InsertSubmission(ctx, session.ID, domain.SubmissionRequest{RequestID: "notice", Text: "notice"}, "notification", domain.SubmissionQueued, "", ""); err != nil {
		t.Fatal(err)
	}
	page, err := service.ListSubmissions(ctx, session.ID, domain.SequenceQuery{Limit: 1})
	if err != nil || len(page.Submissions) != 1 || page.Submissions[0].Text != "" || !page.HasMore {
		t.Fatalf("redaction %#v %v", page, err)
	}
	next, err := service.ListSubmissions(ctx, session.ID, domain.SequenceQuery{AfterSequence: page.NextSequence})
	if err != nil || len(next.Submissions) != 1 || !strings.Contains(next.Submissions[0].Error, hidden.ID) || next.Submissions[0].Status != "queued" {
		t.Fatalf("queued %#v %v", next, err)
	}
	// A user send to a session that is not running is refused, durably.
	var refused *domain.DeliveryError
	if _, err = service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "user", Text: "hello"}); !errors.As(err, &refused) || refused.Code != domain.DeliverySessionNotRunning {
		t.Fatalf("held chat send = %v", err)
	}
	if _, err = service.ListSubmissions(ctx, session.ID, domain.SequenceQuery{Limit: 201}); err == nil {
		t.Fatal("unbounded page accepted")
	}
	if err = service.archiveChat(ctx, session, "discarded"); err != nil {
		t.Fatal(err)
	}
	// Even if deletion was interrupted, archived descriptors cannot be relaunched.
	if _, err = service.createRun(ctx, session.ID, domain.CreateSessionRunRequest{}); err == nil {
		t.Fatal("archive relaunched")
	}
	if err = repo.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReadConversation(ctx, session.ID, domain.ConversationQuery{}); err != nil {
		t.Fatal("empty retained archive unreadable", err)
	}
	if _, err = service.ListSubmissions(ctx, session.ID, domain.SequenceQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReadConversation(ctx, "unknown", domain.ConversationQuery{}); err == nil {
		t.Fatal("unknown archive accepted")
	}
	if _, err = service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "new", Text: "new"}); !errors.As(err, &refused) || refused.Code != domain.DeliverySessionArchived {
		t.Fatalf("archive admitted input: %v", err)
	}
	// The earlier refusal replays as recorded; it was never sent.
	replay, err := service.SubmitMessage(ctx, session.ID, domain.SubmissionRequest{RequestID: "user", Text: "hello"})
	if !errors.As(err, &refused) || refused.Code != domain.DeliverySessionNotRunning || replay.Status != domain.SubmissionRejected {
		t.Fatalf("replay %#v %v", replay, err)
	}
}
