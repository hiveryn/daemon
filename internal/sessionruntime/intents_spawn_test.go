package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
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

func TestApprovedSpawnTicketIntentRollsBackSessionOnLaunchFailure(t *testing.T) {
	service, repo := newSpawnIntentService(t, domain.SessionTypeArchitect)
	operations := []string{}
	repo.operations = &operations
	for _, repoPath := range service.cfg.Architects["hiveryn"].Repos {
		createTestGitCommit(t, repoPath)
	}
	adapter := &fakeAdapter{}
	service.adapters = map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter}
	service.receiver = ingest.NewReceiver(adapter)
	service.terminal = &fakeTerminalManager{startErr: errors.New("pty unavailable")}
	service.bridgeCancels = map[string]func(){}

	done := make(chan domain.IntentResolution[domain.SpawnTicketSessionResult], 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := service.RequestSpawnTicketSession(context.Background(), "architect-session", "ticket-1", "codex")
		done <- res
		errCh <- err
	}()

	intentID := waitForPendingIntent(t, service, "architect-session")
	_, approveErr := service.ApproveIntent(context.Background(), "architect-session", intentID)
	res := <-done
	err := <-errCh
	if approveErr == nil || !strings.Contains(approveErr.Error(), "pty unavailable") {
		t.Fatalf("expected original launch failure, got approveErr=%v resolution=%#v requestErr=%v", approveErr, res, err)
	}
	if repo.deletedRunID != "" {
		t.Fatalf("rollback should delete the session transactionally, not require a separate run repair: %q", repo.deletedRunID)
	}
	if !slicesContain(operations, "delete") {
		t.Fatalf("created session was not rolled back: %#v", operations)
	}
}

func waitForPendingIntent(t *testing.T, service *Service, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := service.intents.PendingForSession(sessionID); len(ids) == 1 {
			return ids[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("intent was not created")
	return ""
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
