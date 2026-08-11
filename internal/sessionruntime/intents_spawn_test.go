package sessionruntime

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

func newSpawnIntentService(t *testing.T, sessionType domain.SessionType) (*Service, *fakeSessionRepository) {
	t.Helper()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID: "architect-session", ArchitectKey: "hiveryn", SessionType: sessionType,
		CurrentRun: &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{
			TicketSummary: domain.TicketSummary{
				ID: "ticket-1", Title: "Implement spawn", Status: domain.TicketStatusBacklog,
				Repo: "daemon", AdditionalRepos: []string{"shared"},
			},
		}},
		cfg: config.Config{
			IntentWaitTimeout: 20,
			Variants:          map[string]config.VariantConfig{"codex": {Agent: "codex", Model: "gpt-5"}},
			Architects: map[string]config.ArchitectConfig{"hiveryn": {
				Path: t.TempDir(), Repos: map[string]string{"daemon": t.TempDir(), "shared": t.TempDir()},
			}},
		},
		intents: newIntentStore(), eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}
	return service, repo
}

func TestRequestSpawnTicketSessionRequiresArchitectAndExplicitKnownProfile(t *testing.T) {
	service, _ := newSpawnIntentService(t, domain.SessionTypeTicket)
	if _, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", "codex"); err == nil {
		t.Fatal("ticket session must not be authorized to spawn another ticket session")
	}

	service, _ = newSpawnIntentService(t, domain.SessionTypeArchitect)
	for _, profile := range []string{"", "missing"} {
		if _, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", profile); err == nil {
			t.Fatalf("profile %q should fail validation before creating an intent", profile)
		}
		if ids := service.intents.PendingForSession("architect-session"); len(ids) != 0 {
			t.Fatalf("invalid profile %q created pending intent %v", profile, ids)
		}
	}
}

func TestSpawnTicketIntentShowsScopeAndPolicyAndDenialCreatesNoSession(t *testing.T) {
	service, repo := newSpawnIntentService(t, domain.SessionTypeArchitect)
	done := make(chan domain.IntentResolution[domain.SpawnTicketSessionResult], 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", "codex")
		done <- res
		errCh <- err
	}()

	var intentID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := service.intents.PendingForSession("architect-session"); len(ids) == 1 {
			intentID = ids[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if intentID == "" {
		t.Fatal("spawn intent was not created")
	}
	intent, ok := service.intents.Get(intentID)
	if !ok || intent.Policy != domain.IntentPolicyWaitThenAllow || intent.Payload["profile"] != "codex" {
		t.Fatalf("unexpected intent %#v", intent)
	}
	scope, ok := intent.Payload["repository_scope"].([]string)
	if !ok || len(scope) != 2 || scope[0] != "daemon" || scope[1] != "shared" {
		t.Fatalf("intent repository scope = %#v", intent.Payload["repository_scope"])
	}
	if err := service.DenyIntent(context.Background(), "architect-session", intentID, "not now"); err != nil {
		t.Fatalf("deny intent: %v", err)
	}
	res := <-done
	if err := <-errCh; err != nil {
		t.Fatalf("request returned error: %v", err)
	}
	if res.Outcome != domain.IntentOutcomeDeniedByUser || repo.createdSession.SessionType != domain.SessionTypeArchitect {
		t.Fatalf("denial outcome/session = %q/%#v", res.Outcome, repo.createdSession)
	}
}
