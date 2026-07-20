package sessionruntime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// newCreateTicketService wires a service against the REAL architectfs writer, so
// these tests assert what actually lands on disk rather than what a fake was
// told.
func newCreateTicketService(t *testing.T) (*Service, *fakeSessionRepository, string) {
	t.Helper()

	architectPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}

	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		cfg: config.Config{
			IntentWaitTimeout: 20,
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: architectPath, Repos: map[string]string{"daemon": t.TempDir()}},
			},
		},
		tickets:      architectfs.NewTicketService(),
		intents:      newIntentStore(),
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}
	return service, repo, architectPath
}

func backlogTicketDirs(t *testing.T, architectPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(architectPath, "tickets", "backlog"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read backlog dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// This is the ticket's whole reason for existing. An agent-runtime tool-call
// timeout makes the agent retry createWorkTicket; without dedup that mints a
// second ticket. Two identical in-flight calls must collapse onto ONE intent
// and ONE ticket, and both callers must get the same ticket id back.
func TestRequestCreateWorkTicketConcurrentRetriesCreateOneTicket(t *testing.T) {
	service, _, architectPath := newCreateTicketService(t)

	params := domain.CreateTicketParams{Title: "Fix the flake", Repo: "daemon", Body: "details"}

	var wg sync.WaitGroup
	results := make([]domain.IntentResolution[domain.Ticket], 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = service.RequestCreateWorkTicket(context.Background(), "session-1", params)
		}(i)
	}

	// Let both calls attach, then approve the single intent they share.
	var intentID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := service.intents.PendingForSession("session-1"); len(ids) > 0 {
			intentID = ids[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if intentID == "" {
		t.Fatal("no pending intent was raised")
	}
	if ids := service.intents.PendingForSession("session-1"); len(ids) != 1 {
		t.Fatalf("pending intents = %v, want exactly 1 (retries must attach, not duplicate)", ids)
	}
	if _, err := service.ApproveIntent(context.Background(), "session-1", intentID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	wg.Wait()

	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].Outcome != domain.IntentOutcomeApproved {
			t.Fatalf("caller %d outcome = %q, want approved", i, results[i].Outcome)
		}
	}
	if results[0].Result.ID != results[1].Result.ID {
		t.Fatalf("callers got different ticket ids (%q vs %q) — the retry minted a duplicate",
			results[0].Result.ID, results[1].Result.ID)
	}
	if got := backlogTicketDirs(t, architectPath); len(got) != 1 {
		t.Fatalf("tickets on disk = %v, want exactly 1", got)
	}

	// A retry AFTER resolution must replay the stored outcome, not block on a
	// dead intent and not mint a second ticket.
	replayed, err := service.RequestCreateWorkTicket(context.Background(), "session-1", params)
	if err != nil {
		t.Fatalf("replayed call: %v", err)
	}
	if replayed.Result.ID != results[0].Result.ID {
		t.Fatalf("replay returned ticket %q, want the original %q", replayed.Result.ID, results[0].Result.ID)
	}
	if got := backlogTicketDirs(t, architectPath); len(got) != 1 {
		t.Fatalf("tickets on disk after replay = %v, want still exactly 1", got)
	}
}

// A denial must be a decision, not a write. Nothing may reach the filesystem.
func TestRequestCreateWorkTicketDeniedWritesNothing(t *testing.T) {
	service, _, architectPath := newCreateTicketService(t)

	done := make(chan domain.IntentResolution[domain.Ticket], 1)
	go func() {
		res, err := service.RequestCreateWorkTicket(context.Background(), "session-1",
			domain.CreateTicketParams{Title: "Should never exist"})
		if err != nil {
			t.Errorf("request: %v", err)
		}
		done <- res
	}()

	var intentID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := service.intents.PendingForSession("session-1"); len(ids) > 0 {
			intentID = ids[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if intentID == "" {
		t.Fatal("no pending intent was raised")
	}

	if err := service.DenyIntent(context.Background(), "session-1", intentID, "not now"); err != nil {
		t.Fatalf("deny: %v", err)
	}

	res := <-done
	if res.Outcome != domain.IntentOutcomeDeniedByUser {
		t.Fatalf("outcome = %q, want denied_by_user", res.Outcome)
	}
	if res.Reason != "not now" {
		t.Fatalf("reason = %q, want the denial reason to reach the agent", res.Reason)
	}
	if got := backlogTicketDirs(t, architectPath); len(got) != 0 {
		t.Fatalf("a denied createWorkTicket wrote %v to disk; it must write nothing", got)
	}
}

// The popup must never be shown for input that cannot succeed: validation runs
// before the intent is raised, and the error goes straight back to the agent.
func TestRequestCreateWorkTicketValidatesBeforeRaisingIntent(t *testing.T) {
	service, repo, _ := newCreateTicketService(t)

	cases := map[string]domain.CreateTicketParams{
		"blank title":  {Title: "   "},
		"unknown repo": {Title: "Valid", Repo: "not-a-repo"},
	}
	for name, params := range cases {
		if _, err := service.RequestCreateWorkTicket(context.Background(), "session-1", params); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}
	if got := repo.events(); len(got) != 0 {
		t.Fatalf("validation failures must not raise an intent event, got %#v", got)
	}
	if ids := service.intents.PendingForSession("session-1"); len(ids) != 0 {
		t.Fatalf("validation failures must not leave pending intents, got %v", ids)
	}
}

// The wait window expiring resolves via the tool's policy. createWorkTicket is
// wait-then-allow, so the ticket IS created and the agent is told it was
// auto-approved.
func TestRequestCreateWorkTicketAutoApprovesOnTimeout(t *testing.T) {
	service, _, architectPath := newCreateTicketService(t)
	service.cfg.IntentWaitTimeout = 1

	res, err := service.RequestCreateWorkTicket(context.Background(), "session-1",
		domain.CreateTicketParams{Title: "Nobody answered"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if res.Outcome != domain.IntentOutcomeAutoApproved {
		t.Fatalf("outcome = %q, want auto_approved", res.Outcome)
	}
	if res.Result.ID == "" {
		t.Fatal("auto-approved createWorkTicket returned no ticket")
	}
	if got := backlogTicketDirs(t, architectPath); len(got) != 1 {
		t.Fatalf("tickets on disk = %v, want exactly 1", got)
	}
}

// The intent event must carry everything a generic cross-session popup needs
// without knowing what the tool does.
func TestRequestCreateWorkTicketPublishesIntentRequiredWithOrigin(t *testing.T) {
	service, repo, _ := newCreateTicketService(t)
	service.cfg.IntentWaitTimeout = 1

	if _, err := service.RequestCreateWorkTicket(context.Background(), "session-1",
		domain.CreateTicketParams{Title: "Popup me"}); err != nil {
		t.Fatalf("request: %v", err)
	}

	events := repo.events()
	var required domain.AppendSessionEventParams
	found := false
	for _, e := range events {
		if e.Status == sessionEventStatusReqd {
			required, found = e, true
			break
		}
	}
	if !found {
		t.Fatalf("no intent/required event was published: %#v", events)
	}
	if required.Type != sessionEventTypeIntent {
		t.Fatalf("event type = %q, want %q", required.Type, sessionEventTypeIntent)
	}
	if required.Raw["intent_id"] == "" || required.Raw["intent_type"] != string(domain.IntentTypeCreateWorkTicket) {
		t.Fatalf("event is missing intent identity: %#v", required.Raw)
	}
	if required.Raw["summary"] != "Popup me" {
		t.Fatalf("summary = %#v, want the ticket title", required.Raw["summary"])
	}
	if required.Raw["policy"] != string(domain.IntentPolicyWaitThenAllow) {
		t.Fatalf("policy = %#v, want wait-then-allow so the UI can label the countdown", required.Raw["policy"])
	}
	origin, ok := required.Raw["origin"].(domain.IntentOrigin)
	if !ok {
		t.Fatalf("origin missing or wrong type: %#v", required.Raw["origin"])
	}
	if origin.ArchitectKey != "hiveryn" || origin.SessionID != "session-1" {
		t.Fatalf("origin = %#v, want it to identify the architect and session", origin)
	}
}
