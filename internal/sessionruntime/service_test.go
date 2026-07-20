package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	aropencode "github.com/hiveryn/agentruntime/adapter/opencode"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

func TestCreateRunMarksRunFailedWhenTerminalStartFails(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		Instructions: "system",
	}
	terminal := &fakeTerminalManager{startErr: errors.New("terminal unavailable")}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	_, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{
		ProfileName: "codex",
		Cols:        132,
		Rows:        48,
	})
	if err == nil {
		t.Fatal("expected terminal start error")
	}

	if repo.createdRun.ID == "" {
		t.Fatal("expected run to be reserved before terminal start")
	}
	if terminal.firstStartSpec().SessionID != "session-1" {
		t.Fatalf("expected terminal to start on session-1, got %#v", terminal.firstStartSpec())
	}
	if terminal.firstStartSpec().Size.Cols != 132 || terminal.firstStartSpec().Size.Rows != 48 {
		t.Fatalf("expected requested dimensions, got %#v", terminal.firstStartSpec().Size)
	}
	if repo.failedRunID != repo.createdRun.ID || repo.failedRunReason != domain.SessionRunFailureLaunchFailed {
		t.Fatalf("expected failed run state, got run=%q reason=%q", repo.failedRunID, repo.failedRunReason)
	}
	if adapter.ensureRequest.Marker != setupMarker {
		t.Fatalf("expected setup marker %q, got %q", setupMarker, adapter.ensureRequest.Marker)
	}
}

func TestRestoreRunningSessionsMarksRestoreFailure(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedSessions = []domain.Session{{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
	}

	err := service.RestoreRunningSessions(context.Background())
	if err != nil {
		t.Fatalf("expected restore to succeed despite individual failure, got %v", err)
	}
	if repo.failedRunID != "run-1" || repo.failedRunReason != domain.SessionRunFailureRestoreFailed {
		t.Fatalf("expected restore failure to mark run failed, got run=%q reason=%q", repo.failedRunID, repo.failedRunReason)
	}
}

func TestResolveStoredRunLaunchContextAllowsEmptyNativeID(t *testing.T) {
	t.Parallel()

	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    newFakeSessionRepository(),
	}
	session := domain.Session{ID: "session-1", ArchitectKey: "hiveryn"}
	run := domain.SessionRun{
		ID:          "run-1",
		ProfileName: "codex",
		Workdir:     t.TempDir(),
		NativeID:    "", // race left this empty; must fall back to id-less resume
		ProfileSnapshot: &domain.AgentProfileSnapshot{
			Agent: "codex",
		},
	}

	profile, agentKind, err := service.resolveStoredRunLaunchContext(session, run)
	if err != nil {
		t.Fatalf("expected empty NativeID to resolve, got %v", err)
	}
	if agentKind != agentruntime.AgentCodex {
		t.Fatalf("expected codex agent kind, got %q", agentKind)
	}
	if profile.Agent != "codex" {
		t.Fatalf("expected codex profile agent, got %q", profile.Agent)
	}
}

func TestRestoreRunningSessionsMovesTicketToBacklogOnRestoreFailure(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedSessions = []domain.Session{{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "fix the thing",
		Workdir:      t.TempDir(),
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}}
	tickets := &fakeTicketService{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		tickets: tickets,
	}

	err := service.RestoreRunningSessions(context.Background())
	if err != nil {
		t.Fatalf("expected restore to succeed despite individual failure, got %v", err)
	}
	if repo.failedRunID != "run-1" || repo.failedRunReason != domain.SessionRunFailureRestoreFailed {
		t.Fatalf("expected restore failure to mark run failed, got run=%q reason=%q", repo.failedRunID, repo.failedRunReason)
	}
}

func TestConcludeArchitectSessionAppendsEndedEventRawBody(t *testing.T) {
	t.Parallel()

	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-architect",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CreatedAt:    time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC),
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfig(t),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-architect", domain.ConcludeSessionParams{Body: "architect conclusion"})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	if event.RunID != "run-1" {
		t.Fatalf("expected run id in ended event, got %#v", event)
	}
	if event.Raw["body"] != "architect conclusion" || event.Raw["lifecycle"] != "concluded" {
		t.Fatalf("unexpected ended event raw %#v", event.Raw)
	}
	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}
}

func TestConcludeArchitectSessionConflictsWhenWorkerRunsStillRunning(t *testing.T) {
	t.Parallel()

	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-architect",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CurrentRun: &domain.SessionRun{
			ID:     "run-1",
			Status: domain.SessionRunStatusRunning,
		},
	}
	repo.listedSessions = []domain.Session{
		repo.createdSession,
		{ID: "session-ticket-2", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-2", Prompt: "kickoff", Workdir: t.TempDir(), CurrentRun: &domain.SessionRun{ID: "run-2", Status: domain.SessionRunStatusRunning}},
		{ID: "session-ticket-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-1", Prompt: "kickoff", Workdir: t.TempDir(), CurrentRun: &domain.SessionRun{ID: "run-3", Status: domain.SessionRunStatusRunning}},
		{ID: "session-other", ArchitectKey: "other", SessionType: domain.SessionTypeTicket, ContextID: "ticket-3", Prompt: "kickoff", Workdir: t.TempDir(), CurrentRun: &domain.SessionRun{ID: "run-4", Status: domain.SessionRunStatusRunning}},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfig(t),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-architect", domain.ConcludeSessionParams{Body: "architect conclusion"})
	if err == nil {
		t.Fatal("expected conflict error")
	}

	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if got, want := conflict.Message, "Cannot conclude architect session: 2 ticket session(s) still active: session-ticket-1, session-ticket-2"; got != want {
		t.Fatalf("expected conflict message %q, got %q", want, got)
	}
	if len(operations) != 0 {
		t.Fatalf("expected no conclude operations, got %#v", operations)
	}
}

func TestConcludeTicketSessionAppendsEndedEventRawConclusionData(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	daemonRepoPath := t.TempDir()
	desktopRepoPath := t.TempDir()
	daemonCommit := createTestGitCommit(t, daemonRepoPath)
	desktopCommit := createTestGitCommit(t, desktopRepoPath)
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	started := created.Add(5 * time.Minute)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-work",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      daemonRepoPath,
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			Workdir:     daemonRepoPath,
			StartedAt:   &started,
			ProfileName: "codex",
		},
	}
	cfg := testRuntimeConfigWithPaths(architectPath, daemonRepoPath)
	cfg.Architects["hiveryn"] = config.ArchitectConfig{Path: architectPath, Repos: map[string]string{"daemon": daemonRepoPath, "desktop": desktopRepoPath}}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		repo:    repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{
			TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusProgress, Created: &created, Updated: &created},
			Body:          "body",
		}},
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-work", domain.ConcludeSessionParams{
		Body:            "worker conclusion",
		Commits:         []domain.CommitRef{{SHA: daemonCommit, Repo: "daemon"}, {SHA: desktopCommit, Repo: "desktop"}},
		Outcome:         domain.TicketOutcomeRejected,
		RejectionReason: "needs another pass",
	})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	commits, ok := event.Raw["commits"].([]domain.CommitRef)
	if !ok || len(commits) != 2 {
		t.Fatalf("expected commits in raw payload, got %#v", event.Raw["commits"])
	}
	if event.Raw["outcome"] != "rejected" || event.Raw["rejection_reason"] != "needs another pass" {
		t.Fatalf("unexpected event raw %#v", event.Raw)
	}
	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}
}

func TestConcludeTicketSessionExploratoryAllowsNoCommits(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	daemonRepoPath := t.TempDir()
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	started := created.Add(5 * time.Minute)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-work",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      daemonRepoPath,
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			Workdir:     daemonRepoPath,
			StartedAt:   &started,
			ProfileName: "codex",
		},
	}
	cfg := testRuntimeConfigWithPaths(architectPath, daemonRepoPath)
	cfg.Architects["hiveryn"] = config.ArchitectConfig{Path: architectPath, Repos: map[string]string{"daemon": daemonRepoPath}}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		repo:    repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{
			TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusProgress, Created: &created, Updated: &created},
			Body:          "body",
		}},
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-work", domain.ConcludeSessionParams{
		Body:    "Investigated the flake; no commits produced.",
		Outcome: domain.TicketOutcomeExploratory,
	})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	event := repo.lastAppendedEvent(t)
	if event.Raw["outcome"] != "exploratory" {
		t.Fatalf("expected outcome exploratory, got %#v", event.Raw["outcome"])
	}
}

func TestConcludeTicketSessionInvalidOutcomeRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-work",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		CurrentRun: &domain.SessionRun{
			ID:     "run-1",
			Status: domain.SessionRunStatusRunning,
		},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:         repo,
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-work", domain.ConcludeSessionParams{Body: "done"})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "outcome" {
		t.Fatalf("expected outcome validation error, got %v", err)
	}
}

func TestUnspawnTicketSessionMovesTicketBackAndDeletesRun(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-work",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      repoPath,
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			Workdir:     repoPath,
			ProfileName: "codex",
		},
	}
	tickets := &fakeTicketService{ticket: domain.Ticket{
		TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusProgress, Created: &created, Updated: &created},
		Body:          "body",
	}}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfigWithPaths(architectPath, repoPath),
		repo:         repo,
		tickets:      tickets,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	result, err := service.UnspawnTicketSession(context.Background(), "session-work")
	if err != nil {
		t.Fatalf("UnspawnTicketSession failed: %v", err)
	}

	if result.SessionID != "session-work" || result.TicketID != "ticket-1" || result.ArchitectKey != "hiveryn" {
		t.Fatalf("unexpected result %#v", result)
	}
	if tickets.movedTo != domain.TicketStatusBacklog {
		t.Fatalf("expected ticket moved to backlog, got %q", tickets.movedTo)
	}
	if repo.deletedRunID != "run-1" {
		t.Fatalf("expected run deleted, got %q", repo.deletedRunID)
	}
	event := repo.lastAppendedEvent(t)
	if event.Status != "ended" || event.Message != "session discarded" || event.Raw["lifecycle"] != "discarded" {
		t.Fatalf("unexpected discard event %#v", event)
	}
	if got, want := strings.Join(operations, ","), "event,kill,delete_run,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}
}

func TestUnspawnTicketSessionRejectsNonTicketSession(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-architect",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	service := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), repo: repo, intents: newIntentStore()}

	_, err := service.UnspawnTicketSession(context.Background(), "session-architect")
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "session_type" {
		t.Fatalf("expected session_type validation error, got %v", err)
	}
}

func TestRequestConclusionRejectsMissingCommitsBeforeApproval(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 13, 15, 30, 0, 0, time.UTC)
	started := created.Add(5 * time.Minute)
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-work",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:        "run-1",
			Status:    domain.SessionRunStatusRunning,
			StartedAt: &started,
		},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:         repo,
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.RequestConclusion(context.Background(), "session-work", domain.ConcludeSessionParams{
		Summary:        "done",
		Outcome:        domain.TicketOutcomeCompleted,
		Implementation: "did it",
	})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "commits" {
		t.Fatalf("expected commits validation error, got %v", err)
	}
	if len(repo.appendedEvents) != 0 {
		t.Fatalf("expected no approval_required event, got %#v", repo.appendedEvents)
	}
}

func TestDenyIntentResolvesWithoutRunningExec(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:         repo,
		intents:      newIntentStore(),
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	executed := false
	in := domain.Intent{
		ID:      "intent-1",
		Type:    domain.IntentTypeConcludeSession,
		Summary: "done",
		Origin:  domain.IntentOrigin{SessionID: "session-work", ArchitectKey: "hiveryn"},
	}
	_, ch, _, _ := service.intents.Begin("key-1", in, func(context.Context) (any, error) {
		executed = true
		return nil, nil
	})

	if err := service.DenyIntent(context.Background(), "session-work", "intent-1", "needs work"); err != nil {
		t.Fatalf("deny: %v", err)
	}

	if executed {
		t.Fatal("a denied intent must never run its side effect")
	}

	event := repo.lastAppendedEvent(t)
	if event.Type != sessionEventTypeIntent || event.Status != sessionEventStatusResolvd {
		t.Fatalf("expected an intent/resolved event, got %q/%q", event.Type, event.Status)
	}
	if event.Raw["outcome"] != string(domain.IntentOutcomeDeniedByUser) {
		t.Fatalf("expected outcome denied_by_user, got %#v", event.Raw["outcome"])
	}
	if event.Raw["reason"] != "needs work" {
		t.Fatalf("expected the denial reason to reach the event, got %#v", event.Raw["reason"])
	}

	// The blocked agent call must be released with the denial.
	select {
	case res := <-ch:
		if res.Outcome != domain.IntentOutcomeDeniedByUser {
			t.Fatalf("waiter got outcome %q, want denied_by_user", res.Outcome)
		}
	default:
		t.Fatal("waiter was not released by the denial")
	}
}

// A cancelled agent ctx must NOT kill the intent. An agent-runtime tool-call
// timeout cancels ctx and retries, and the retry has to be able to replay the
// original outcome rather than mint a duplicate. This is the deliberate
// behavior change from the old approval flow, which resolved as "cancelled".
func TestRequestConclusionCancelledCtxLeavesIntentAlive(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:         repo,
		intents:      newIntentStore(),
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	in := domain.Intent{
		ID:     "intent-1",
		Type:   domain.IntentTypeCreateWorkTicket,
		Origin: domain.IntentOrigin{SessionID: "session-work"},
	}
	id, ch, _, _ := service.intents.Begin("key-1", in, func(context.Context) (any, error) {
		return domain.Ticket{}, nil
	})

	// The requester walks away.
	service.intents.Detach(id, ch)

	if got := service.intents.PendingForSession("session-work"); len(got) != 1 {
		t.Fatalf("intent did not survive the caller's cancellation: pending = %v", got)
	}

	// It still resolves, and the resolution is replayable by the retry.
	service.intents.Finish(id, intentResult{Outcome: domain.IntentOutcomeAutoApproved, Result: domain.Ticket{}})
	_, _, replayed, disposition := service.intents.Begin("key-1", in, nil)
	if disposition != intentReplayed || replayed.Outcome != domain.IntentOutcomeAutoApproved {
		t.Fatalf("retry did not replay the resolved outcome: disposition=%v outcome=%q", disposition, replayed.Outcome)
	}
}

func TestReconcileIntentsResolvesDanglingRequired(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedSessions = []domain.Session{
		{ID: "dangling", SessionType: domain.SessionTypeTicket},
		{ID: "resolved", SessionType: domain.SessionTypeTicket},
		{ID: "ended", SessionType: domain.SessionTypeTicket},
		{ID: "multi", SessionType: domain.SessionTypeTicket},
	}
	required := func(id string) domain.SessionEvent {
		return domain.SessionEvent{
			Type:   sessionEventTypeIntent,
			Status: sessionEventStatusReqd,
			Raw:    map[string]any{"intent_id": id, "intent_type": string(domain.IntentTypeCreateWorkTicket)},
		}
	}
	resolved := func(id string) domain.SessionEvent {
		return domain.SessionEvent{
			Type:   sessionEventTypeIntent,
			Status: sessionEventStatusResolvd,
			Raw:    map[string]any{"intent_id": id},
		}
	}
	repo.sessionEvents = map[string][]domain.SessionEvent{
		"dangling": {required("i-1")},
		"resolved": {required("i-1"), resolved("i-1")},
		"ended":    {required("i-1"), {Type: "status", Status: "ended"}},
		// N-per-session: only the still-open one gets reconciled.
		"multi": {required("i-1"), required("i-2"), resolved("i-1")},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:         repo,
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	if err := service.ReconcileIntents(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(repo.appendedEvents) != 2 {
		t.Fatalf("expected exactly two resolution events (dangling + multi/i-2), got %#v", repo.appendedEvents)
	}
	for _, event := range repo.appendedEvents {
		if event.Status != sessionEventStatusResolvd {
			t.Fatalf("unexpected reconciliation event: %#v", event)
		}
		if event.Raw["outcome"] != string(domain.IntentOutcomeError) {
			t.Fatalf("expected outcome error, got %#v", event.Raw["outcome"])
		}
	}
	if repo.appendedEvents[0].SessionID != "dangling" {
		t.Fatalf("expected the dangling session first, got %q", repo.appendedEvents[0].SessionID)
	}
	if repo.appendedEvents[1].SessionID != "multi" || repo.appendedEvents[1].Raw["intent_id"] != "i-2" {
		t.Fatalf("expected multi/i-2 to be the only other orphan, got %#v", repo.appendedEvents[1])
	}
}

func TestMoveTicketToDoneRejectsMissingCommits(t *testing.T) {
	t.Parallel()

	service := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), intents: newIntentStore()}

	_, err := service.MoveTicketToDone(context.Background(), "hiveryn", "ticket-1", domain.MoveTicketToDoneParams{Body: "done", Outcome: domain.TicketOutcomeCompleted})
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "commits" {
		t.Fatalf("expected commits validation error, got %v", err)
	}
}

func TestConcludeFreeformSessionWritesConclusionAndAllowsNoCommits(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	workdir := t.TempDir()
	created := time.Date(2026, 5, 13, 16, 0, 0, 0, time.UTC)
	started := created.Add(2 * time.Minute)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-freeform",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-13-1600-investigate-login-failure",
		Prompt:       "Investigate login failure and report root cause",
		Workdir:      workdir,
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
			StartedAt:   &started,
		},
	}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: architectPath, Repos: map[string]string{}},
			},
		},
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-freeform", domain.ConcludeSessionParams{Body: "Root cause identified."})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	path := filepath.Join(architectPath, "freeform", repo.createdSession.ContextID, "conclusion.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conclusion: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Root cause identified.") {
		t.Fatalf("expected conclusion body in %q", content)
	}
	if strings.Contains(content, "commits:") {
		t.Fatalf("did not expect commits frontmatter in %q", content)
	}
	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}
}

func TestConcludeFreeformSessionValidatesProvidedCommits(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	createTestGitCommit(t, repoPath)
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-freeform",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-13-1600-investigate-login-failure",
		Prompt:       "Investigate login failure and report root cause",
		Workdir:      t.TempDir(),
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: architectPath, Repos: map[string]string{"daemon": repoPath}},
			},
		},
		repo:         repo,
		terminal:     &fakeTerminalManager{},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-freeform", domain.ConcludeSessionParams{
		Body:    "Root cause identified.",
		Commits: []domain.CommitRef{{SHA: "deadbeef", Repo: "daemon"}},
	})
	if err == nil {
		t.Fatal("expected commit validation error")
	}
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "commits" {
		t.Fatalf("expected commits validation error, got %T %v", err, err)
	}
}

func TestConcludeArchitectSessionEmptyBodySkipsConclusionFile(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-architect",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CreatedAt:    time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC),
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          testRuntimeConfig(t),
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}
	service.cfg.Architects["hiveryn"] = config.ArchitectConfig{Path: architectPath, Repos: map[string]string{}}

	_, err := service.ConcludeSession(context.Background(), "session-architect", domain.ConcludeSessionParams{Body: ""})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}

	event := repo.lastAppendedEvent(t)
	if event.Raw["body"] != "" {
		t.Fatalf("expected empty body in ended event, got %#v", event.Raw["body"])
	}

	dir := filepath.Join(architectPath, "architect-sessions", "2026-05-13-1500")
	conclusionPath := filepath.Join(dir, "conclusion.md")
	if _, err := os.Stat(conclusionPath); !os.IsNotExist(err) {
		t.Fatal("expected no conclusion.md written for discarded architect session")
	}
}

func TestConcludeFreeformSessionEmptyBodySkipsConclusionFile(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	workdir := t.TempDir()
	created := time.Date(2026, 5, 13, 16, 0, 0, 0, time.UTC)
	operations := []string{}
	repo := newFakeSessionRepository()
	repo.operations = &operations
	repo.createdSession = domain.Session{
		ID:           "session-freeform",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-13-1600-investigate-login-failure",
		Prompt:       "Investigate login failure and report root cause",
		Workdir:      workdir,
		CreatedAt:    created,
		CurrentRun: &domain.SessionRun{
			ID:          "run-1",
			Status:      domain.SessionRunStatusRunning,
			ProfileName: "codex",
		},
	}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: architectPath, Repos: map[string]string{}},
			},
		},
		repo:         repo,
		terminal:     &fakeTerminalManager{operations: &operations},
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	_, err := service.ConcludeSession(context.Background(), "session-freeform", domain.ConcludeSessionParams{Body: ""})
	if err != nil {
		t.Fatalf("ConcludeSession failed: %v", err)
	}

	if got, want := strings.Join(operations, ","), "complete,event,kill,delete"; got != want {
		t.Fatalf("expected operations %q, got %q", want, got)
	}

	event := repo.lastAppendedEvent(t)
	if event.Raw["body"] != "" {
		t.Fatalf("expected empty body in ended event, got %#v", event.Raw["body"])
	}

	dir := filepath.Join(architectPath, "freeform", repo.createdSession.ContextID)
	conclusionPath := filepath.Join(dir, "conclusion.md")
	if _, err := os.Stat(conclusionPath); !os.IsNotExist(err) {
		t.Fatal("expected no conclusion.md written for discarded freeform session")
	}
}

func TestCreateSessionFreeformWritesPromptFile(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	workdir := t.TempDir()
	repo := newFakeSessionRepository()
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: architectPath, Repos: map[string]string{}},
			},
		},
		repo:    repo,
		tickets: &fakeTicketService{},
	}

	prompt := "Investigate login failure and report root cause"
	session, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		Prompt:       prompt,
		Workdir:      workdir,
		Slug:         "Investigate Login Failure",
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if session.SessionType != domain.SessionTypeFreeform {
		t.Fatalf("unexpected session %#v", session)
	}
	if !strings.HasSuffix(session.ContextID, "investigate-login-failure") {
		t.Fatalf("expected normalized freeform context id, got %#v", session)
	}
	if session.Workdir != workdir || session.Prompt != prompt {
		t.Fatalf("unexpected persisted session %#v", session)
	}
	data, err := os.ReadFile(filepath.Join(architectPath, "freeform", session.ContextID, "prompt.md"))
	if err != nil {
		t.Fatalf("read prompt.md: %v", err)
	}
	if string(data) != prompt {
		t.Fatalf("unexpected prompt contents %q", string(data))
	}
}

func TestCreateRunFreeformUsesStoredSessionWorkdir(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeFreeform,
		ContextID:    "2026-05-13-1600-investigate-login-failure",
		Prompt:       "Investigate login failure and report root cause",
		Workdir:      workdir,
	}
	adapter := &fakeAdapter{}
	terminal := &fakeTerminalManager{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}
	if repo.createdRun.Workdir != workdir {
		t.Fatalf("expected run workdir %q, got %#v", workdir, repo.createdRun)
	}
	if adapter.launchRequest.Workdir != workdir {
		t.Fatalf("expected launch request workdir %q, got %#v", workdir, adapter.launchRequest)
	}
}

func TestCreateSessionReloadsArchitectsFile(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	hiverynPath := t.TempDir()
	lithoPath := t.TempDir()
	cfgPath := writeRuntimeConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
	})
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}

	repo := newFakeSessionRepository()
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          cfg,
		configSource: source,
		repo:         repo,
		tickets:      &fakeTicketService{},
	}

	_, err = service.CreateSession(context.Background(), domain.CreateSessionRequest{
		ArchitectKey: "litho",
		SessionType:  domain.SessionTypeArchitect,
	})
	if err == nil {
		t.Fatal("expected missing architect before reload")
	}
	if _, ok := err.(*domain.NotFoundError); !ok {
		t.Fatalf("expected not found error, got %T %v", err, err)
	}

	writeRuntimeArchitects(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
		"litho": {
			Path:  lithoPath,
			Repos: map[string]string{"app": "/tmp/lithoapp"},
		},
	})

	session, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		ArchitectKey: "litho",
		SessionType:  domain.SessionTypeArchitect,
	})
	if err != nil {
		t.Fatalf("CreateSession after reload failed: %v", err)
	}
	if session.ArchitectKey != "litho" || session.SessionType != domain.SessionTypeArchitect {
		t.Fatalf("unexpected session after reload: %#v", session)
	}
}

func TestCreateRunReloadsTabsFileForSessionLayouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sessionType domain.SessionType
		contextID   string
		updatedCmd  string
	}{
		{name: "architect", sessionType: domain.SessionTypeArchitect, contextID: "2026-05-13-1500", updatedCmd: "btop"},
		{name: "ticket", sessionType: domain.SessionTypeTicket, contextID: "ticket-1", updatedCmd: "yazi"},
		{name: "freeform", sessionType: domain.SessionTypeFreeform, contextID: "2026-05-13-1600-investigate", updatedCmd: "lazygit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			architectPath := t.TempDir()
			workdir := t.TempDir()
			cfgPath := writeRuntimeConfigFiles(t, configDir, map[string]config.ArchitectConfig{
				"hiveryn": {
					Path:  architectPath,
					Repos: map[string]string{"daemon": workdir},
				},
			})
			writeRuntimeTabs(t, configDir, map[string][]config.TabEntry{
				string(tt.sessionType): {{Type: "terminal", Command: "old-command"}},
			})

			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			source, err := config.NewReloadingSource(cfgPath, cfg)
			if err != nil {
				t.Fatalf("create config source: %v", err)
			}

			repo := newFakeSessionRepository()
			repo.createdSession = domain.Session{
				ID:           "session-1",
				ArchitectKey: "hiveryn",
				SessionType:  tt.sessionType,
				ContextID:    tt.contextID,
				Prompt:       "kickoff",
				Workdir:      workdir,
			}
			adapter := &fakeAdapter{}
			terminal := &fakeTerminalManager{}
			service := &Service{
				intents:      newIntentStore(),
				logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
				cfg:          cfg,
				configSource: source,
				repo:         repo,
				tickets:      &fakeTicketService{},
				receiver: ingest.NewReceiver(
					adapter,
				),
				adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
					agentruntime.AgentCodex: adapter,
				},
				terminal:       terminal,
				eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
				bridgeCancels:  map[string]func(){},
				terminalStates: map[string]sessionTerminalState{},
			}

			writeRuntimeTabs(t, configDir, map[string][]config.TabEntry{
				string(tt.sessionType): {{Type: "terminal", Command: tt.updatedCmd}},
			})

			if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
				t.Fatalf("CreateRun failed: %v", err)
			}

			tabs, err := service.ListSessionTabs(context.Background(), "session-1")
			if err != nil {
				t.Fatalf("ListSessionTabs failed: %v", err)
			}
			if len(tabs) != 1 {
				t.Fatalf("expected 1 auto-created tab, got %#v", tabs)
			}
			if tabs[0].Type != "terminal" || tabs[0].Command != tt.updatedCmd || tabs[0].Status != "running" {
				t.Fatalf("unexpected reloaded tabs layout %#v", tabs)
			}
		})
	}
}

func TestCreateTerminalUsesCurrentRunWorkdirAndConfiguredShell(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      repoPath,
		CurrentRun: &domain.SessionRun{
			ID:      "run-1",
			Status:  domain.SessionRunStatusRunning,
			Workdir: repoPath,
		},
	}
	terminal := &fakeTerminalManager{}
	service := &Service{
		intents:        newIntentStore(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:            config.Config{Shell: "/bin/zsh"},
		repo:           repo,
		terminal:       terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}

	info, err := service.CreateTerminal(context.Background(), "session-1", domain.CreateTerminalParams{Placement: domain.TerminalPlacementTab})
	if err != nil {
		t.Fatalf("CreateTerminal failed: %v", err)
	}
	if info.Command != "/bin/zsh" {
		t.Fatalf("expected configured shell, got %#v", info)
	}
	if got := terminal.firstStartSpec(); got.Command != "/bin/zsh" || got.Workdir != repoPath {
		t.Fatalf("unexpected terminal start spec %#v", got)
	}
	if tabs := service.terminalStates["session-1"].tabs; len(tabs) != 1 || tabs[0].tab.Placement != domain.TerminalPlacementTab {
		t.Fatalf("expected created tab placement to be stored, got %#v", tabs)
	}
}

func TestCreateTerminalRejectsMissingPlacement(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      repoPath,
		CurrentRun: &domain.SessionRun{
			ID:      "run-1",
			Status:  domain.SessionRunStatusRunning,
			Workdir: repoPath,
		},
	}
	service := &Service{
		intents:        newIntentStore(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}

	_, err := service.CreateTerminal(context.Background(), "session-1", domain.CreateTerminalParams{})
	if err == nil {
		t.Fatal("expected missing placement to be rejected")
	}
	vErr, ok := err.(*domain.ValidationError)
	if !ok || vErr.Field != "placement" {
		t.Fatalf("expected placement validation error, got %T %#v", err, err)
	}
}

func TestCreateTerminalRejectsSecondSplit(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      repoPath,
		CurrentRun: &domain.SessionRun{
			ID:      "run-1",
			Status:  domain.SessionRunStatusRunning,
			Workdir: repoPath,
		},
	}
	service := &Service{
		intents:       newIntentStore(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:          repo,
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{
			"session-1": {
				tabs: []sessionTabState{
					{tab: domain.SessionTab{Type: "kanban"}},
					{tab: domain.SessionTab{Type: "terminal", ID: "term-split", Placement: domain.TerminalPlacementSplit, BaseTabID: "kanban"}},
				},
			},
		},
	}

	_, err := service.CreateTerminal(context.Background(), "session-1", domain.CreateTerminalParams{Placement: domain.TerminalPlacementSplit, BaseTabID: "kanban"})
	if err == nil {
		t.Fatal("expected duplicate split terminal to be rejected")
	}
	cErr, ok := err.(*domain.ConflictError)
	if !ok || cErr.Field != "base_tab_id" {
		t.Fatalf("expected placement conflict error, got %T %#v", err, err)
	}
}

func TestCreateTerminalAllowsSplitOnDifferentBaseTab(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      repoPath,
		CurrentRun: &domain.SessionRun{
			ID:      "run-1",
			Status:  domain.SessionRunStatusRunning,
			Workdir: repoPath,
		},
	}
	terminal := &fakeTerminalManager{}
	service := &Service{
		intents:       newIntentStore(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:          repo,
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{
			"session-1": {
				tabs: []sessionTabState{
					{tab: domain.SessionTab{Type: "kanban"}},
					{tab: domain.SessionTab{Type: "event-log"}},
					{tab: domain.SessionTab{Type: "terminal", ID: "term-split", Placement: domain.TerminalPlacementSplit, BaseTabID: "kanban"}},
				},
			},
		},
	}

	info, err := service.CreateTerminal(context.Background(), "session-1", domain.CreateTerminalParams{Placement: domain.TerminalPlacementSplit, BaseTabID: "event-log"})
	if err != nil {
		t.Fatalf("CreateTerminal failed: %v", err)
	}
	if info.TerminalID == "" {
		t.Fatalf("expected created terminal info, got %#v", info)
	}
	tabs := service.terminalStates["session-1"].tabs
	created := tabs[len(tabs)-1].tab
	if created.Placement != domain.TerminalPlacementSplit || created.BaseTabID != "event-log" {
		t.Fatalf("expected event-log split placement, got %#v", created)
	}
	if terminal.firstStartSpec().TerminalID != info.TerminalID {
		t.Fatalf("expected started terminal %q, got %#v", info.TerminalID, terminal.firstStartSpec())
	}
}

func TestCreateTerminalRejectsSplitWithoutBaseTabID(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeTicket,
		ContextID:    "ticket-1",
		Prompt:       "kickoff",
		Workdir:      repoPath,
		CurrentRun: &domain.SessionRun{
			ID:      "run-1",
			Status:  domain.SessionRunStatusRunning,
			Workdir: repoPath,
		},
	}
	service := &Service{
		intents:       newIntentStore(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:          repo,
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{
			"session-1": {
				tabs: []sessionTabState{{tab: domain.SessionTab{Type: "kanban"}}},
			},
		},
	}

	_, err := service.CreateTerminal(context.Background(), "session-1", domain.CreateTerminalParams{Placement: domain.TerminalPlacementSplit})
	if err == nil {
		t.Fatal("expected split without base tab id to be rejected")
	}
	vErr, ok := err.(*domain.ValidationError)
	if !ok || vErr.Field != "base_tab_id" {
		t.Fatalf("expected base_tab_id validation error, got %T %#v", err, err)
	}
}

func TestKillTerminalRejectsMainTerminalID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "session-1"}
	service := &Service{
		intents:        newIntentStore(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{"session-1": {mainTerminalID: "term-main-1"}},
	}

	err := service.KillTerminal(context.Background(), "session-1", "term-main-1")
	if err == nil {
		t.Fatal("expected main terminal kill to be rejected")
	}
	if vErr, ok := err.(*domain.ValidationError); !ok || vErr.Field != "terminal_id" {
		t.Fatalf("expected validation error, got %T %#v", err, err)
	}
}

func newRunningBrowserTabTestService() (*Service, *fakeSessionRepository) {
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID: "session-1",
		CurrentRun: &domain.SessionRun{
			ID:     "run-1",
			Status: domain.SessionRunStatusRunning,
		},
	}
	service := &Service{
		intents:        newIntentStore(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}
	return service, repo
}

func TestPreviewBrowserTabCreatesNewTab(t *testing.T) {
	t.Parallel()

	service, repo := newRunningBrowserTabTestService()

	info, err := service.PreviewBrowserTab(context.Background(), "session-1", domain.PreviewBrowserTabParams{Target: "https://example.com"})
	if err != nil {
		t.Fatalf("PreviewBrowserTab failed: %v", err)
	}
	if info.TabID == "" || info.Target != "https://example.com" || info.SessionID != "session-1" {
		t.Fatalf("unexpected info: %#v", info)
	}

	tabs := service.terminalStates["session-1"].tabs
	if len(tabs) != 1 || tabs[0].tab.Type != "browser" || tabs[0].tab.ID != info.TabID || tabs[0].tab.Target != "https://example.com" {
		t.Fatalf("unexpected tabs: %#v", tabs)
	}

	event := repo.lastAppendedEvent(t)
	if event.Status != "tab_changed" {
		t.Fatalf("expected tab_changed event, got %#v", event)
	}
}

func TestPreviewBrowserTabNavigatesExistingTab(t *testing.T) {
	t.Parallel()

	service, _ := newRunningBrowserTabTestService()
	service.terminalStates["session-1"] = sessionTerminalState{
		tabs: []sessionTabState{
			{tab: domain.SessionTab{Type: "browser", ID: "tab-1", Target: "https://old.example.com"}},
		},
	}

	info, err := service.PreviewBrowserTab(context.Background(), "session-1", domain.PreviewBrowserTabParams{Target: "https://new.example.com", TabID: "tab-1"})
	if err != nil {
		t.Fatalf("PreviewBrowserTab failed: %v", err)
	}
	if info.TabID != "tab-1" || info.Target != "https://new.example.com" {
		t.Fatalf("unexpected info: %#v", info)
	}

	tabs := service.terminalStates["session-1"].tabs
	if len(tabs) != 1 || tabs[0].tab.Target != "https://new.example.com" {
		t.Fatalf("unexpected tabs after navigate: %#v", tabs)
	}
}

func TestPreviewBrowserTabRejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	service, _ := newRunningBrowserTabTestService()

	_, err := service.PreviewBrowserTab(context.Background(), "session-1", domain.PreviewBrowserTabParams{Target: "ftp://example.com"})
	if _, ok := err.(*domain.ValidationError); !ok {
		t.Fatalf("expected validation error, got %T %#v", err, err)
	}
}

func TestPreviewBrowserTabUnknownTabIDNotFound(t *testing.T) {
	t.Parallel()

	service, _ := newRunningBrowserTabTestService()

	_, err := service.PreviewBrowserTab(context.Background(), "session-1", domain.PreviewBrowserTabParams{Target: "https://example.com", TabID: "missing"})
	if _, ok := err.(*domain.NotFoundError); !ok {
		t.Fatalf("expected not found error, got %T %#v", err, err)
	}
}

func TestCloseBrowserTabRemovesTab(t *testing.T) {
	t.Parallel()

	service, repo := newRunningBrowserTabTestService()
	service.terminalStates["session-1"] = sessionTerminalState{
		tabs: []sessionTabState{
			{tab: domain.SessionTab{Type: "browser", ID: "tab-1", Target: "https://example.com"}},
		},
	}

	if err := service.CloseBrowserTab(context.Background(), "session-1", "tab-1"); err != nil {
		t.Fatalf("CloseBrowserTab failed: %v", err)
	}
	if tabs := service.terminalStates["session-1"].tabs; len(tabs) != 0 {
		t.Fatalf("expected tab removed, got %#v", tabs)
	}
	event := repo.lastAppendedEvent(t)
	if event.Status != "tab_changed" {
		t.Fatalf("expected tab_changed event, got %#v", event)
	}
}

func TestCloseBrowserTabUnknownIDNotFound(t *testing.T) {
	t.Parallel()

	service, _ := newRunningBrowserTabTestService()

	err := service.CloseBrowserTab(context.Background(), "session-1", "missing")
	if _, ok := err.(*domain.NotFoundError); !ok {
		t.Fatalf("expected not found error, got %T %#v", err, err)
	}
}

func TestListSessionsHydratesMainTerminalID(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.listedSessions = []domain.Session{{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}}
	service := &Service{
		intents:        newIntentStore(),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:           repo,
		terminalStates: map[string]sessionTerminalState{"session-1": {mainTerminalID: "term-main-1"}},
	}

	sessions, err := service.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0].CurrentRun == nil || sessions[0].CurrentRun.MainTerminalID != "term-main-1" {
		t.Fatalf("expected hydrated main terminal id, got %#v", sessions)
	}
}

func TestHandleTerminalExitResumesMainTerminal(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	started := time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC)
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      "/tmp/workdir",
		Instructions: "system prompt",
		CurrentRun: &domain.SessionRun{
			ID:              "run-1",
			ProfileName:     "codex",
			Status:          domain.SessionRunStatusRunning,
			NativeID:        "native-1",
			Workdir:         "/tmp/workdir",
			StartedAt:       &started,
			ProfileSnapshot: &domain.AgentProfileSnapshot{Agent: "codex", Env: map[string]string{"CODEX_HOME": "/custom/codex"}},
		},
	}
	adapter := &fakeAdapter{}
	terminal := &fakeTerminalManager{}
	oldBridgeCancelled := false
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){"session-1": func() { oldBridgeCancelled = true }},
		terminalStates: map[string]sessionTerminalState{
			"session-1": {
				mainTerminalID: "term-main-old",
				tabs: []sessionTabState{{
					tab: domain.SessionTab{Type: "terminal", ID: "term-extra", Command: "yazi", Status: "running"},
				}},
			},
		},
	}

	service.handleTerminalExit(terminalExit{SessionID: "session-1", TerminalID: "term-main-old", Err: errors.New("process exited")})

	if !oldBridgeCancelled {
		t.Fatal("expected previous receiver bridge to be cancelled")
	}
	startSpec := terminal.firstStartSpec()
	if !adapter.launchRequest.Resume || adapter.launchRequest.ResumeID != "native-1" {
		t.Fatalf("expected resume launch request, got %#v", adapter.launchRequest)
	}
	state := service.terminalStates["session-1"]
	if state.mainTerminalID != startSpec.TerminalID {
		t.Fatalf("expected terminal state to point at resumed terminal %q, got %#v", startSpec.TerminalID, state)
	}
	if len(state.tabs) != 1 || state.tabs[0].tab.ID != "term-extra" {
		t.Fatalf("expected existing tabs to be preserved, got %#v", state.tabs)
	}
	event := repo.lastAppendedEvent(t)
	if event.Type != "main_terminal_resumed" || event.RunID != "run-1" {
		t.Fatalf("unexpected resumed event %#v", event)
	}
}

func TestHandleReceiverEventUpdatesCurrentRun(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:    repo,
	}

	service.handleReceiverEvent(agentruntime.Event{
		ID:       "session-1",
		Status:   agentruntime.StatusWorking,
		NativeID: "native-1",
		Message:  "hello",
		At:       time.Now().UTC(),
	})

	event := repo.lastAppendedEvent(t)
	if event.SessionID != "session-1" || event.RunID != "run-1" {
		t.Fatalf("unexpected appended event %#v", event)
	}
	if repo.updatedRunNativeID != "run-1" || repo.updatedRunNative != "native-1" {
		t.Fatalf("expected run native id update, got run=%q native=%q", repo.updatedRunNativeID, repo.updatedRunNative)
	}
}

func TestHandleReceiverEventEmitsAgentStatusOnTransition(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Workdir:      t.TempDir(),
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	service := &Service{
		intents:      newIntentStore(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:         repo,
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}

	sub, err := service.SubscribeSessionEvents(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("subscribe session events: %v", err)
	}
	defer sub.Close()

	// working -> active is a transition from the zero-value status, so it emits.
	service.handleReceiverEvent(agentruntime.Event{ID: "session-1", Status: agentruntime.StatusWorking, At: time.Now().UTC()})
	// A second working event maps to the same "active" status: no emit.
	service.handleReceiverEvent(agentruntime.Event{ID: "session-1", Status: agentruntime.StatusWorking, At: time.Now().UTC()})
	// awaiting_input -> waiting is a transition: emit.
	service.handleReceiverEvent(agentruntime.Event{ID: "session-1", Status: agentruntime.StatusAwaitingInput, At: time.Now().UTC()})

	want := strings.Join([]string{domain.AgentStatusActive, domain.AgentStatusWaiting}, ",")

	var persisted []string
	for _, params := range repo.appendedEvents {
		if params.Type == "agent_status" {
			persisted = append(persisted, params.Status)
		}
	}
	if got := strings.Join(persisted, ","); got != want {
		t.Fatalf("persisted agent_status events = %q, want %q", got, want)
	}

	var streamed []string
drain:
	for {
		select {
		case ev := <-sub.C():
			if ev.Type == "agent_status" {
				streamed = append(streamed, ev.Status)
			}
		default:
			break drain
		}
	}
	if got := strings.Join(streamed, ","); got != want {
		t.Fatalf("streamed agent_status events = %q, want %q", got, want)
	}
}

func TestCreateRunAddsMCPServer(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		Instructions: "system",
	}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: adapter,
		},
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		baseURL:        "http://127.0.0.1:4200",
		executablePath: func() (string, error) { return "/tmp/hiverynd", nil },
	}

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	assertMCPServer(t, adapter.launchRequest.MCPServers, domain.SessionTypeArchitect, "session-1")
}

func TestCreateRunMergesVariantMCPServers(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		Instructions: "system",
	}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Variants: map[string]config.VariantConfig{
				"codex-sentrux": {
					Agent: "codex",
					MCP: map[string]config.MCPServerConfig{
						"sentrux": {Command: "sentrux", Args: []string{"--mcp"}},
						"alpha":   {Command: "alpha"},
					},
				},
			},
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: t.TempDir(), Repos: map[string]string{}},
			},
		},
		repo:           repo,
		receiver:       ingest.NewReceiver(adapter),
		adapters:       map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter},
		terminal:       &fakeTerminalManager{},
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		baseURL:        "http://127.0.0.1:4200",
		executablePath: func() (string, error) { return "/tmp/hiverynd", nil },
	}

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex-sentrux"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	servers := adapter.launchRequest.MCPServers
	if len(servers) != 3 {
		t.Fatalf("expected base + 2 variant mcp servers, got %#v", servers)
	}
	if servers[0].Name != "hiveryn-daemon" {
		t.Fatalf("expected base hiveryn-daemon server first, got %#v", servers)
	}
	// Variant servers are appended in sorted-by-name order for determinism.
	if servers[1].Name != "alpha" || servers[2].Name != "sentrux" {
		t.Fatalf("expected variant servers sorted (alpha, sentrux), got %#v", servers)
	}
	if servers[2].Command != "sentrux" || len(servers[2].Args) != 1 || servers[2].Args[0] != "--mcp" {
		t.Fatalf("unexpected sentrux server config %#v", servers[2])
	}
}

func TestSnapshotVariantRoundTripsMCP(t *testing.T) {
	t.Parallel()

	profile := config.VariantConfig{
		Agent: "claude",
		Args:  []string{"--model", "claude-sonnet-4-6"},
		Env:   map[string]string{"K": "V"},
		MCP: map[string]config.MCPServerConfig{
			"sentrux": {
				Command:           "sentrux",
				Args:              []string{"--mcp"},
				Env:               map[string]string{"E": "1"},
				BearerTokenEnvVar: "TOKEN",
			},
		},
	}

	snap := snapshotVariant(profile)
	got, ok := snap.MCP["sentrux"]
	if !ok {
		t.Fatalf("expected snapshot to carry sentrux mcp, got %#v", snap.MCP)
	}
	if got.Command != "sentrux" || got.BearerTokenEnvVar != "TOKEN" {
		t.Fatalf("unexpected snapshot server %#v", got)
	}

	rebuilt := mcpServersFromSnapshot(snap.MCP)
	rebuiltServer, ok := rebuilt["sentrux"]
	if !ok {
		t.Fatalf("expected rebuilt sentrux mcp, got %#v", rebuilt)
	}
	if rebuiltServer.Command != "sentrux" {
		t.Fatalf("expected rebuilt command sentrux, got %q", rebuiltServer.Command)
	}
	if len(rebuiltServer.Args) != 1 || rebuiltServer.Args[0] != "--mcp" {
		t.Fatalf("expected rebuilt args [--mcp], got %#v", rebuiltServer.Args)
	}
	if rebuiltServer.Env["E"] != "1" || rebuiltServer.BearerTokenEnvVar != "TOKEN" {
		t.Fatalf("unexpected rebuilt server %#v", rebuiltServer)
	}
}

func TestCreateRunOpenCodeArchitectDefinesNamedAgent(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		Instructions: "system prompt",
	}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "opencode"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	assertOpenCodeArchitectAgentConfig(t, adapter.launchRequest, "hiveryn", "system prompt")
}

func TestCreateRunOpenCodeLaunchSpecOmitNilAgentPermission(t *testing.T) {
	t.Parallel()

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "2026-05-13-1500",
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		Instructions: "system prompt",
	}
	adapter := &fakePrepareLaunchAdapter{delegate: aropencode.New(aropencode.DefaultOptions())}
	terminal := &fakeTerminalManager{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfig(t),
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      terminal,
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
		baseURL:       "http://127.0.0.1:4200",
	}

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "opencode"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	spec := terminal.firstStartSpec()
	if !hasArgFlag(spec.Args, "--agent") || !hasArgFlag(spec.Args, "--prompt") {
		t.Fatalf("expected opencode args to include --prompt and --agent, got %#v", spec.Args)
	}
	configContent := spec.Env["OPENCODE_CONFIG_CONTENT"]
	if strings.Contains(configContent, `"permission":null`) {
		t.Fatalf("nil agent permission must be omitted, got %s", configContent)
	}
}

func TestCreateRunOpenCodeFailsWhenProfileAlreadySetsAgentFlag(t *testing.T) {
	t.Parallel()

	cfg := testRuntimeConfig(t)
	cfg.Variants["opencode"] = config.VariantConfig{Agent: "opencode", Args: []string{"--agent", "custom"}}
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeArchitect, ContextID: "2026-05-13-1500", Prompt: "kickoff", Workdir: t.TempDir(), Instructions: "system"}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		repo:    repo,
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	_, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "opencode"})
	if err == nil {
		t.Fatal("expected architect OpenCode launch to fail when --agent is preset")
	}
	if !strings.Contains(err.Error(), "must not include --agent") {
		t.Fatalf("expected --agent validation error, got %v", err)
	}
	if repo.failedRunReason != domain.SessionRunFailureLaunchFailed {
		t.Fatalf("expected failed run reason launch_failed, got %q", repo.failedRunReason)
	}
}

func TestCreateRunOpenCodeTicketDoesNotDefineNamedAgent(t *testing.T) {
	t.Parallel()

	architectPath := t.TempDir()
	repoPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	created := time.Date(2026, 5, 13, 14, 30, 0, 0, time.UTC)
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-1", Prompt: "kickoff", Workdir: repoPath}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     testRuntimeConfigWithPaths(architectPath, repoPath),
		repo:    repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{TicketSummary: domain.TicketSummary{ID: "ticket-1", Title: "Ticket", Repo: "daemon", Status: domain.TicketStatusBacklog, Created: &created, Updated: &created}, Body: "body"}},
		receiver: ingest.NewReceiver(
			adapter,
		),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentOpenCode: adapter,
		},
		terminal:      &fakeTerminalManager{},
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}

	if _, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "opencode"}); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}
	if hasArgFlag(adapter.launchRequest.Args, "--agent") {
		t.Fatalf("ticket OpenCode args unexpectedly include --agent: %#v", adapter.launchRequest.Args)
	}
	if len(adapter.launchRequest.OpenCodeAgentConfig) != 0 {
		t.Fatalf("ticket OpenCode unexpectedly defined named agent config: %#v", adapter.launchRequest.OpenCodeAgentConfig)
	}
}

func testRuntimeConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Variants: map[string]config.VariantConfig{
			"codex":    {Agent: "codex", Env: map[string]string{"CODEX_HOME": "/custom/codex"}},
			"opencode": {Agent: "opencode"},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {Path: t.TempDir(), Repos: map[string]string{}},
		},
	}
}

func testRuntimeConfigWithPaths(architectPath, repoPath string) config.Config {
	return config.Config{
		Variants: map[string]config.VariantConfig{
			"codex":    {Agent: "codex", Env: map[string]string{"CODEX_HOME": "/custom/codex"}},
			"opencode": {Agent: "opencode"},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {Path: architectPath, Repos: map[string]string{"daemon": repoPath}},
		},
	}
}

func writeRuntimeConfigFiles(t *testing.T, configDir string, architects map[string]config.ArchitectConfig) string {
	t.Helper()

	path := filepath.Join(configDir, "config.yaml")
	writeRuntimeYAML(t, path, map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "debug",
	})
	writeRuntimeYAML(t, filepath.Join(configDir, "variants.yaml"), map[string]config.VariantConfig{
		"codex": {Agent: "codex"},
	})
	writeRuntimeArchitects(t, configDir, architects)
	return path
}

// writeRuntimeArchitects writes the bare architects.yaml registry (key -> path)
// plus a hiveryn.yaml in each architect's workspace, derived from the test's
// ArchitectConfig values.
func writeRuntimeArchitects(t *testing.T, configDir string, architects map[string]config.ArchitectConfig) {
	t.Helper()

	registry := map[string]string{}
	for key, architect := range architects {
		registry[key] = architect.Path
		name := architect.Name
		if name == "" {
			name = key
		}
		repos := map[string]string{}
		for repoKey, repoPath := range architect.Repos {
			repos[repoKey] = repoPath
		}
		writeRuntimeYAML(t, filepath.Join(architect.Path, "hiveryn.yaml"), map[string]any{
			"name":  name,
			"repos": repos,
		})
	}
	writeRuntimeYAML(t, filepath.Join(configDir, "architects.yaml"), registry)
}

func writeRuntimeTabs(t *testing.T, configDir string, tabs map[string][]config.TabEntry) {
	t.Helper()
	writeRuntimeYAML(t, filepath.Join(configDir, "tabs.yaml"), tabs)
}

func writeRuntimeYAML(t *testing.T, path string, v any) {
	t.Helper()

	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %q: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

type fakeAdapter struct {
	ensureRequest agentruntime.SetupRequest
	ensureErr     error
	launchRequest agentruntime.StartRequest
}

func (fakeAdapter) Agent() agentruntime.AgentKind { return agentruntime.AgentCodex }

func (fakeAdapter) ConfigRoot(env map[string]string) string { return env["CODEX_HOME"] }

func (f *fakeAdapter) PrepareLaunch(_ context.Context, req agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	f.launchRequest = req
	return agentruntime.LaunchSpec{Command: "fake-command", Workdir: "/tmp", Args: append([]string(nil), req.Args...)}, nil
}

func (f *fakeAdapter) EnsureSetup(_ context.Context, req agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	f.ensureRequest = req
	if f.ensureErr != nil {
		return agentruntime.SetupResult{}, f.ensureErr
	}
	return agentruntime.SetupResult{}, nil
}

func (f *fakeAdapter) RemoveSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}

func (fakeAdapter) NormalizeEvent(context.Context, []byte) (*agentruntime.Event, error) {
	return nil, nil
}

func (fakeAdapter) LocateTranscript(context.Context, agentruntime.LocateRequest) (string, error) {
	return "", nil
}

func (fakeAdapter) ParseUsage(context.Context, string) (agentruntime.Usage, error) {
	return agentruntime.Usage{}, nil
}

type fakePrepareLaunchAdapter struct {
	delegate agentruntime.Adapter
}

func (f *fakePrepareLaunchAdapter) Agent() agentruntime.AgentKind { return f.delegate.Agent() }

func (f *fakePrepareLaunchAdapter) ConfigRoot(env map[string]string) string {
	return f.delegate.ConfigRoot(env)
}

func (f *fakePrepareLaunchAdapter) PrepareLaunch(ctx context.Context, req agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	return f.delegate.PrepareLaunch(ctx, req)
}

func (f *fakePrepareLaunchAdapter) EnsureSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}

func (f *fakePrepareLaunchAdapter) RemoveSetup(context.Context, agentruntime.SetupRequest) (agentruntime.SetupResult, error) {
	return agentruntime.SetupResult{}, nil
}

func (f *fakePrepareLaunchAdapter) NormalizeEvent(context.Context, []byte) (*agentruntime.Event, error) {
	return nil, nil
}

func (f *fakePrepareLaunchAdapter) LocateTranscript(ctx context.Context, req agentruntime.LocateRequest) (string, error) {
	return f.delegate.LocateTranscript(ctx, req)
}

func (f *fakePrepareLaunchAdapter) ParseUsage(ctx context.Context, transcriptPath string) (agentruntime.Usage, error) {
	return f.delegate.ParseUsage(ctx, transcriptPath)
}

type fakeTerminalManager struct {
	startErr   error
	startSpecs []terminalStartSpec
	terminals  []domain.TerminalInfo
	killed     []string
	operations *[]string
}

func (f *fakeTerminalManager) Start(_ context.Context, spec terminalStartSpec) error {
	f.startSpecs = append(f.startSpecs, spec)
	if f.startErr == nil {
		f.terminals = append(f.terminals, domain.TerminalInfo{TerminalID: spec.TerminalID, SessionID: spec.SessionID, Command: spec.Command, Status: "running"})
	}
	return f.startErr
}

func (f *fakeTerminalManager) firstStartSpec() terminalStartSpec {
	if len(f.startSpecs) > 0 {
		return f.startSpecs[0]
	}
	return terminalStartSpec{}
}

func (f *fakeTerminalManager) Attach(context.Context, string, string) (domain.TerminalAttachment, error) {
	return nil, nil
}

func (f *fakeTerminalManager) Kill(_ context.Context, sessionID, id string) error {
	f.killed = append(f.killed, sessionID+":"+id)
	for i, terminal := range f.terminals {
		if terminal.SessionID == sessionID && terminal.TerminalID == id {
			f.terminals = append(f.terminals[:i], f.terminals[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeTerminalManager) KillBySession(context.Context, string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "kill")
	}
	return nil
}

func (f *fakeTerminalManager) ListBySession(sessionID string) []domain.TerminalInfo {
	terminals := make([]domain.TerminalInfo, 0, len(f.terminals))
	for _, terminal := range f.terminals {
		if terminal.SessionID == sessionID {
			terminals = append(terminals, terminal)
		}
	}
	return terminals
}

func (f *fakeTerminalManager) Shutdown(context.Context) error { return nil }

type fakeSessionRepository struct {
	// The intent path is genuinely concurrent (an owner goroutine plus N
	// waiters), so the fake has to be safe to touch from several goroutines.
	mu                 sync.Mutex
	createdSession     domain.Session
	createdRun         domain.SessionRun
	listedSessions     []domain.Session
	failedRunID        string
	failedRunReason    domain.SessionRunFailureReason
	completedRunID     string
	deletedRunID       string
	updatedRunNativeID string
	updatedRunNative   string
	appendedEvents     []domain.AppendSessionEventParams
	sessionEvents      map[string][]domain.SessionEvent
	operations         *[]string
}

func newFakeSessionRepository() *fakeSessionRepository { return &fakeSessionRepository{} }

func (f *fakeSessionRepository) CreateSession(_ context.Context, params domain.CreateSessionParams) (domain.Session, error) {
	f.createdSession = domain.Session{ID: params.ID, ArchitectKey: params.ArchitectKey, SessionType: params.SessionType, ContextID: params.ContextID, Prompt: params.Prompt, Workdir: params.Workdir, Instructions: params.Instructions, CreatedBy: params.CreatedBy}
	if f.createdSession.ID == "" {
		f.createdSession.ID = "session-created"
	}
	return f.createdSession, nil
}

func (f *fakeSessionRepository) GetSession(context.Context, string) (domain.Session, error) {
	if f.createdSession.ID != "" {
		return cloneSession(f.createdSession), nil
	}
	if len(f.listedSessions) > 0 {
		return cloneSession(f.listedSessions[0]), nil
	}
	return domain.Session{}, &domain.NotFoundError{Resource: "session", ID: "missing"}
}

func (f *fakeSessionRepository) ListSessions(context.Context) ([]domain.Session, error) {
	if len(f.listedSessions) == 0 {
		if f.createdSession.ID == "" {
			return nil, nil
		}
		return []domain.Session{cloneSession(f.createdSession)}, nil
	}
	sessions := make([]domain.Session, len(f.listedSessions))
	for i := range f.listedSessions {
		sessions[i] = cloneSession(f.listedSessions[i])
	}
	return sessions, nil
}

func (f *fakeSessionRepository) DeleteSession(context.Context, string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete")
	}
	return nil
}

func (f *fakeSessionRepository) CreateRun(_ context.Context, params domain.CreateSessionRunParams) (domain.SessionRun, error) {
	startedAt := params.StartedAt
	snapshot := params.ProfileSnapshot
	f.createdRun = domain.SessionRun{
		ID:              params.ID,
		SessionID:       params.SessionID,
		Status:          domain.SessionRunStatusRunning,
		ProfileName:     params.ProfileName,
		ProfileSnapshot: &snapshot,
		Workdir:         params.Workdir,
		NativeID:        params.NativeID,
		StartedAt:       &startedAt,
	}
	if f.createdRun.ID == "" {
		f.createdRun.ID = "run-created"
	}
	f.createdSession.CurrentRun = &f.createdRun
	return f.createdRun, nil
}

func (f *fakeSessionRepository) GetRun(context.Context, string) (domain.SessionRun, error) {
	return f.createdRun, nil
}

func (f *fakeSessionRepository) GetCurrentRun(context.Context, string) (*domain.SessionRun, error) {
	if f.createdSession.CurrentRun == nil {
		return nil, nil
	}
	run := *f.createdSession.CurrentRun
	return &run, nil
}

func (f *fakeSessionRepository) DeleteRun(_ context.Context, id string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "delete_run")
	}
	f.deletedRunID = id
	if f.createdSession.CurrentRun != nil && f.createdSession.CurrentRun.ID == id {
		f.createdSession.CurrentRun = nil
	}
	return nil
}

func (f *fakeSessionRepository) MarkRunCompleted(_ context.Context, id string) error {
	if f.operations != nil {
		*f.operations = append(*f.operations, "complete")
	}
	f.completedRunID = id
	if f.createdSession.CurrentRun != nil && f.createdSession.CurrentRun.ID == id {
		f.createdSession.CurrentRun.Status = domain.SessionRunStatusCompleted
	}
	return nil
}

func (f *fakeSessionRepository) MarkRunFailed(_ context.Context, id string, reason domain.SessionRunFailureReason) error {
	f.failedRunID = id
	f.failedRunReason = reason
	if f.createdSession.CurrentRun != nil && f.createdSession.CurrentRun.ID == id {
		f.createdSession.CurrentRun.Status = domain.SessionRunStatusFailed
		f.createdSession.CurrentRun.FailureReason = reason
	}
	return nil
}

func (f *fakeSessionRepository) UpdateRunNativeID(_ context.Context, id, nativeID string) error {
	f.updatedRunNativeID = id
	f.updatedRunNative = nativeID
	if f.createdSession.CurrentRun != nil && f.createdSession.CurrentRun.ID == id {
		f.createdSession.CurrentRun.NativeID = nativeID
	}
	return nil
}

func (f *fakeSessionRepository) UpdateRunAgentStatus(_ context.Context, id, agentStatus string) error {
	if f.createdSession.CurrentRun != nil && f.createdSession.CurrentRun.ID == id {
		f.createdSession.CurrentRun.AgentStatus = agentStatus
	}
	return nil
}

func (f *fakeSessionRepository) ListSessionEvents(_ context.Context, sessionID string) ([]domain.SessionEvent, error) {
	return f.sessionEvents[sessionID], nil
}

func (f *fakeSessionRepository) AppendSessionEvent(_ context.Context, params domain.AppendSessionEventParams) (domain.SessionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.operations != nil {
		*f.operations = append(*f.operations, "event")
	}
	f.appendedEvents = append(f.appendedEvents, params)
	return domain.SessionEvent{SessionID: params.SessionID, RunID: params.RunID, Type: params.Type, Status: params.Status, Message: params.Message, Raw: params.Raw, At: params.At}, nil
}

// events returns a snapshot under the lock; the intent policy goroutine may
// still be publishing while a test inspects the log.
func (f *fakeSessionRepository) events() []domain.AppendSessionEventParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AppendSessionEventParams(nil), f.appendedEvents...)
}

func (f *fakeSessionRepository) lastAppendedEvent(t *testing.T) domain.AppendSessionEventParams {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.appendedEvents) == 0 {
		t.Fatal("expected appended session event")
	}
	return f.appendedEvents[len(f.appendedEvents)-1]
}

type fakeTicketService struct {
	ticket      domain.Ticket
	err         error
	moveErr     error
	movedTo     domain.TicketStatus
	concludeErr error
}

func (f *fakeTicketService) ListTickets(context.Context, string) (domain.TicketBoard, error) {
	return domain.TicketBoard{}, nil
}

func (f *fakeTicketService) GetTicket(context.Context, string, string) (domain.Ticket, error) {
	return f.ticket, f.err
}

func (f *fakeTicketService) CreateTicket(context.Context, string, domain.CreateTicketParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) EditTicket(context.Context, string, string, domain.EditTicketParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) UpdateTicketMetadata(context.Context, string, string, domain.UpdateTicketMetadataParams) (domain.Ticket, error) {
	return domain.Ticket{}, nil
}

func (f *fakeTicketService) DeleteTicket(context.Context, string, string) error { return nil }

func (f *fakeTicketService) MoveTicket(_ context.Context, _ string, _ string, params domain.MoveTicketParams) (domain.Ticket, error) {
	f.movedTo = params.To
	return domain.Ticket{}, f.moveErr
}

func (f *fakeTicketService) ConcludeTicket(context.Context, string, string, domain.TicketConclusion) (domain.Ticket, error) {
	return domain.Ticket{}, f.concludeErr
}

func assertOpenCodeArchitectAgentConfig(t *testing.T, req agentruntime.StartRequest, architectKey, systemPrompt string) {
	t.Helper()
	if req.Instructions != "" {
		t.Fatalf("expected architect OpenCode instructions to move into named agent config, got %#v", req)
	}
	if len(req.Args) < 2 || req.Args[0] != "--agent" || req.Args[1] != architectKey {
		t.Fatalf("expected args to start with --agent %s, got %#v", architectKey, req.Args)
	}
	entry, ok := req.OpenCodeAgentConfig[architectKey]
	if !ok {
		t.Fatalf("expected named OpenCode agent config for %q, got %#v", architectKey, req.OpenCodeAgentConfig)
	}
	if entry.Description != "Hiveryn architect" || entry.Mode != "primary" || entry.Prompt != systemPrompt {
		t.Fatalf("unexpected architect agent config %#v", entry)
	}
	if entry.Permission != nil {
		t.Fatalf("expected no permission config, got %#v", entry)
	}
}

func assertMCPServer(t *testing.T, servers []agentruntime.MCPServerConfig, sessionType domain.SessionType, sessionID string) {
	t.Helper()
	if len(servers) != 1 {
		t.Fatalf("mcp servers = %#v", servers)
	}
	server := servers[0]
	if server.Name != "hiveryn-daemon" || server.Command != "/tmp/hiverynd" {
		t.Fatalf("unexpected mcp server %#v", server)
	}
	if server.Env["HIVERYN_SESSION_TYPE"] != string(sessionType) || server.Env["HIVERYN_SESSION_ID"] != sessionID {
		t.Fatalf("unexpected mcp env %#v", server.Env)
	}
}

func cloneSession(session domain.Session) domain.Session {
	cloned := session
	if session.CurrentRun != nil {
		run := *session.CurrentRun
		cloned.CurrentRun = &run
	}
	return cloned
}

func createTestGitCommit(t *testing.T, repoPath string) string {
	t.Helper()
	runGit(t, repoPath, "init")
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoPath, "add", "README.md")
	runGit(t, repoPath, "commit", "-m", "initial")
	return runGit(t, repoPath, "rev-parse", "HEAD")
}

func runGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
