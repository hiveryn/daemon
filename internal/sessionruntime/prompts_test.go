package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

func TestBuildArchitectInstructionsWithoutCustomFile(t *testing.T) {
	t.Parallel()
	workspace := testWorkspace(t, nil)

	got, err := buildArchitectInstructions(config.ArchitectConfig{Path: workspace})
	if err != nil {
		t.Fatalf("buildArchitectInstructions: %v", err)
	}
	builtin, _ := builtinPrompt(architectSystemPromptName)
	if got.Text != builtin {
		t.Fatalf("expected the built-in prompt alone, got:\n%s", got.Text)
	}
	if got.Custom.Present {
		t.Fatalf("no ARCHITECT_SYSTEM.md was written, yet Present=%v", got.Custom.Present)
	}
	for _, removed := range []string{"spawnTicketSession", "listAgentProfiles", "readArchitectConfig", "readDefaultPrompt"} {
		if strings.Contains(got.Text, removed) {
			t.Fatalf("built-in architect prompt still names removed tool %q", removed)
		}
	}
	if !strings.Contains(got.Text, "Start by running checkWorkspace") {
		t.Fatal("built-in architect prompt does not tell the architect to run checkWorkspace")
	}
	// The daemon does not enforce the conclusion gate yet, so the prompt must
	// not claim that it does.
	if strings.Contains(got.Text, "daemon independently reruns") || strings.Contains(got.Text, "rejects uncommitted") {
		t.Fatal("built-in architect prompt claims a conclusion gate the daemon does not enforce")
	}
}

func TestBuildArchitectInstructionsAppendsCustomFile(t *testing.T) {
	t.Parallel()
	workspace := testWorkspace(t, nil)
	writeWorkspaceFile(t, workspace, workspacefs.ArchitectSystemFileName, "Work as a peer.\nDiscuss material decisions.\n")

	got, err := buildArchitectInstructions(config.ArchitectConfig{Path: workspace})
	if err != nil {
		t.Fatalf("buildArchitectInstructions: %v", err)
	}
	builtin, _ := builtinPrompt(architectSystemPromptName)
	if !strings.HasPrefix(got.Text, builtin) {
		t.Fatal("custom file replaced the built-in prompt instead of being appended")
	}
	if !strings.Contains(got.Text, "Work as a peer.\nDiscuss material decisions.") {
		t.Fatalf("custom content missing from instructions:\n%s", got.Text)
	}
	if !got.Custom.Loaded {
		t.Fatalf("custom file not reported as loaded: %+v", got.Custom)
	}
}

// A broken ARCHITECT_SYSTEM.md must be surfaced to the architect explicitly and
// must not stop the session from being assembled.
func TestBuildArchitectInstructionsSurfacesUnloadableCustomFile(t *testing.T) {
	t.Parallel()
	workspace := testWorkspace(t, nil)
	writeWorkspaceFile(t, workspace, workspacefs.ArchitectSystemFileName, "")

	got, err := buildArchitectInstructions(config.ArchitectConfig{Path: workspace})
	if err != nil {
		t.Fatalf("an unloadable custom file must not fail assembly: %v", err)
	}
	if !got.Custom.Present || got.Custom.Loaded {
		t.Fatalf("expected present-but-not-loaded, got %+v", got.Custom)
	}
	if !strings.Contains(got.Text, "was NOT loaded") || !strings.Contains(got.Text, domain.DiagEmptyBody) {
		t.Fatalf("instructions do not surface the unloadable file:\n%s", got.Text)
	}
}

func TestRenderWorkerKickoff(t *testing.T) {
	t.Parallel()
	ctx := workspacefs.WorkerContext{
		ProjectOverviewPath: "/ws/PROJECT_OVERVIEW.md",
		ProjectStatePath:    "/ws/PROJECT_STATE.md",
		RoadmapCurrentPath:  "/ws/ROADMAP_CURRENT.md",
	}
	repos := []workerRepo{{Key: "daemon", Path: "/repos/daemon"}, {Key: "shared", Path: "/repos/shared"}}

	got, err := renderWorkerKickoff("ticket-1", repos, ctx)
	if err != nil {
		t.Fatalf("renderWorkerKickoff: %v", err)
	}
	want := strings.TrimSpace(`Read your ticket with readTicket(id: "ticket-1") and complete the work.

Writable repositories:
- daemon: /repos/daemon
- shared: /repos/shared

Read these project documents:
/ws/PROJECT_OVERVIEW.md
/ws/PROJECT_STATE.md
/ws/ROADMAP_CURRENT.md

Read and follow the workflows selected for this session:
none`)
	if got != want {
		t.Fatalf("kickoff without workflows:\n%s\nwant:\n%s", got, want)
	}

	ctx.RoadmapCurrentPath = ""
	got, err = renderWorkerKickoff("ticket-1", repos, ctx)
	if err != nil {
		t.Fatalf("renderWorkerKickoff: %v", err)
	}
	if strings.Contains(got, "ROADMAP") || !strings.Contains(got, "/ws/PROJECT_STATE.md\n\nRead and follow") {
		t.Fatalf("kickoff without a roadmap must list only the two documents:\n%s", got)
	}
	ctx.RoadmapCurrentPath = "/ws/ROADMAP_CURRENT.md"

	ctx.Workflows = []string{"/ws/workflows/B.md", "/ws/workflows/A.md"}
	got, err = renderWorkerKickoff("ticket-1", repos, ctx)
	if err != nil {
		t.Fatalf("renderWorkerKickoff: %v", err)
	}
	if !strings.HasSuffix(got, "selected for this session:\n/ws/workflows/B.md\n/ws/workflows/A.md") {
		t.Fatalf("kickoff with workflows lost order or paths:\n%s", got)
	}
	if strings.Contains(got, "none") {
		t.Fatalf("kickoff with workflows still says none:\n%s", got)
	}
}

func TestResumeInstructionsTellTheAgentToRereadSelectedWorkflows(t *testing.T) {
	t.Parallel()
	if got := resumeInstructions("system", nil); got != "system" {
		t.Fatalf("no workflows must leave instructions untouched, got %q", got)
	}
	got := resumeInstructions("system", []string{"/ws/workflows/A.md"})
	if !strings.HasPrefix(got, "system\n\n## Session resumed") {
		t.Fatalf("resume note not appended:\n%s", got)
	}
	if !strings.Contains(got, "reread the selected workflows") || !strings.Contains(got, "/ws/workflows/A.md") {
		t.Fatalf("resume note does not instruct rereading the canonical paths:\n%s", got)
	}
	if strings.Contains(strings.ToLower(got), "unchanged") {
		t.Fatalf("resume note must not claim the files are unchanged:\n%s", got)
	}
}

// newTicketCreateService wires a Service able to create a ticket session for a
// backlog ticket scoped to repoPath, in the given workspace.
func newTicketCreateService(t *testing.T, workspace, repoPath string, additional map[string]string) (*Service, *fakeSessionRepository) {
	t.Helper()
	repos := map[string]string{"daemon": repoPath}
	var additionalKeys []string
	for key, path := range additional {
		repos[key] = path
		additionalKeys = append(additionalKeys, key)
	}
	repo := newFakeSessionRepository()
	service := &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Variants:   map[string]config.VariantConfig{"codex": {Agent: "codex"}},
			Architects: map[string]config.ArchitectConfig{"hiveryn": {Name: "Hiveryn", Path: workspace, Repos: repos}},
		},
		repo: repo,
		tickets: &fakeTicketService{ticket: domain.Ticket{TicketSummary: domain.TicketSummary{
			ID: "ticket-1", Title: "Ticket", Repo: "daemon", AdditionalRepos: additionalKeys, Status: domain.TicketStatusBacklog,
		}, Body: "body"}},
	}
	return service, repo
}

func gitRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return dir
}

func TestCreateSessionTicketPersistsSelectionAndRendersFixedKickoff(t *testing.T) {
	t.Parallel()
	repoPath := gitRepoDir(t)
	sharedPath := gitRepoDir(t)
	workspace := testWorkspace(t, map[string]string{"daemon": repoPath, "shared": sharedPath})
	a := writeWorkflow(t, workspace, "A.md", "---\nattach: manual\n---\n\nA.\n")
	b := writeWorkflow(t, workspace, "B.md", "---\nattach: suggested\nrepos:\n- daemon\n---\n\nB.\n")
	writeWorkflow(t, workspace, "C.md", "---\nattach: suggested\nrepos:\n- shared\n---\n\nC.\n") // matches scope, not selected

	service, _ := newTicketCreateService(t, workspace, repoPath, map[string]string{"shared": sharedPath})
	session, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		SessionType: domain.SessionTypeTicket, ArchitectKey: "hiveryn", TicketID: "ticket-1", Workflows: []string{b, a},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if len(session.Workflows) != 2 || session.Workflows[0] != b || session.Workflows[1] != a {
		t.Fatalf("selection not persisted as given: %v", session.Workflows)
	}
	worker, _ := builtinPrompt(workerSystemPromptName)
	if session.Instructions != worker {
		t.Fatalf("worker instructions are not the built-in WORKER_SYSTEM:\n%s", session.Instructions)
	}
	for _, fragment := range []string{
		`readTicket(id: "ticket-1")`,
		"- daemon: " + repoPath,
		"- shared: " + sharedPath,
		"/PROJECT_OVERVIEW.md", "/PROJECT_STATE.md", "/ROADMAP_CURRENT.md",
		"selected for this session:\n" + b + "\n" + a,
	} {
		if !strings.Contains(session.Prompt, fragment) {
			t.Fatalf("kickoff missing %q:\n%s", fragment, session.Prompt)
		}
	}
	if strings.Contains(session.Prompt, "C.md") {
		t.Fatalf("an unselected repo-matched workflow was attached:\n%s", session.Prompt)
	}
	if strings.Contains(session.Prompt, "A.\n") || strings.Contains(session.Prompt, "body") {
		t.Fatalf("kickoff embeds workflow or ticket bodies instead of paths:\n%s", session.Prompt)
	}
	if session.CreatedBy != domain.SessionCreatedByDesktop {
		t.Fatalf("created_by = %q", session.CreatedBy)
	}
}

func TestCreateSessionTicketRejectsInvalidSelectionInsteadOfDroppingIt(t *testing.T) {
	t.Parallel()
	repoPath := gitRepoDir(t)
	workspace := testWorkspace(t, map[string]string{"daemon": repoPath})
	a := writeWorkflow(t, workspace, "A.md", "---\nattach: manual\n---\n\nA.\n")
	missing := filepath.Join(filepath.Dir(a), "renamed.md")

	service, repo := newTicketCreateService(t, workspace, repoPath, nil)
	_, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		SessionType: domain.SessionTypeTicket, ArchitectKey: "hiveryn", TicketID: "ticket-1", Workflows: []string{a, missing},
	})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Field != "workflows" || !strings.Contains(verr.Message, "renamed.md") {
		t.Fatalf("expected an actionable workflows validation error, got %v", err)
	}
	if repo.createdSession.ID != "" {
		t.Fatalf("session was created despite the invalid selection: %#v", repo.createdSession)
	}
}

func TestCreateSessionRejectsRemovedFreeformType(t *testing.T) {
	t.Parallel()
	repoPath := gitRepoDir(t)
	workspace := testWorkspace(t, map[string]string{"daemon": repoPath})

	service, repo := newTicketCreateService(t, workspace, repoPath, nil)
	_, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		SessionType: domain.SessionType("freeform"), ArchitectKey: "hiveryn",
	})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Field != "session_type" {
		t.Fatalf("expected a session_type validation error, got %v", err)
	}
	if repo.createdSession.ID != "" {
		t.Fatalf("session was created for a removed session type: %#v", repo.createdSession)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "freeform")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no freeform directory to be written, got %v", statErr)
	}
}

func TestCreateSessionTicketFailsWhenProjectDocumentsAreMissing(t *testing.T) {
	t.Parallel()
	repoPath := gitRepoDir(t)
	workspace := testWorkspace(t, map[string]string{"daemon": repoPath})
	if err := os.Remove(filepath.Join(workspace, workspacefs.ProjectStateFileName)); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	service, _ := newTicketCreateService(t, workspace, repoPath, nil)
	_, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		SessionType: domain.SessionTypeTicket, ArchitectKey: "hiveryn", TicketID: "ticket-1",
	})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Field != "workspace" || !strings.Contains(verr.Message, "PROJECT_STATE.md") {
		t.Fatalf("expected a workspace validation error naming PROJECT_STATE.md, got %v", err)
	}
}

func TestCreateSessionArchitectUsesFixedKickoffAndRejectsWorkflows(t *testing.T) {
	t.Parallel()
	workspace := testWorkspace(t, nil)
	service, _ := newTicketCreateService(t, workspace, gitRepoDir(t), nil)

	_, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		SessionType: domain.SessionTypeArchitect, ArchitectKey: "hiveryn", Workflows: []string{"/x.md"},
	})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Field != "workflows" {
		t.Fatalf("expected workflows to be rejected for architect sessions, got %v", err)
	}

	session, err := service.CreateSession(context.Background(), domain.CreateSessionRequest{
		SessionType: domain.SessionTypeArchitect, ArchitectKey: "hiveryn",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, fragment := range []string{"# Project: hiveryn", "Session started: ", "Start by running checkWorkspace.", "listTickets", "readRecentArchitectConclusion", "Configured repos:\n- daemon: "} {
		if !strings.Contains(session.Prompt, fragment) {
			t.Fatalf("architect kickoff missing %q:\n%s", fragment, session.Prompt)
		}
	}
	if !strings.Contains(session.Instructions, "You are the project's architect") {
		t.Fatalf("architect instructions are not the built-in prompt:\n%s", session.Instructions)
	}
}

// Launch, restore and resume all revalidate the stored selection against the
// live workspace: a selected workflow deleted after the session was created
// fails the launch with the path named, and is never quietly dropped.
func TestCreateRunFailsWhenSelectedWorkflowDisappeared(t *testing.T) {
	t.Parallel()
	repoPath := gitRepoDir(t)
	workspace := testWorkspace(t, map[string]string{"daemon": repoPath})
	a := writeWorkflow(t, workspace, "A.md", "---\nattach: manual\n---\n\nA.\n")

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-1",
		Prompt: "kickoff", Workdir: repoPath, Workflows: []string{a}, Instructions: "worker",
	}
	adapter := &fakeAdapter{}
	tickets := &fakeTicketService{}
	service := &Service{
		intents: newIntentStore(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:  testRuntimeConfigWithPaths(workspace, repoPath),
		repo: repo, tickets: tickets, receiver: ingest.NewReceiver(adapter),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter},
		terminal: &fakeTerminalManager{}, eventStreams: map[string]map[uint64]chan domain.SessionEvent{}, bridgeCancels: map[string]func(){},
	}

	if err := os.Remove(a); err != nil {
		t.Fatalf("remove workflow: %v", err)
	}
	_, err := service.CreateRun(context.Background(), "session-1", domain.CreateSessionRunRequest{ProfileName: "codex"})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || !strings.Contains(verr.Message, a) || !strings.Contains(verr.Message, "does not exist") {
		t.Fatalf("expected the missing selected workflow to fail the launch actionably, got %v", err)
	}
	if tickets.movedTo != "" {
		t.Fatalf("ticket moved despite failed launch: %q", tickets.movedTo)
	}
	if adapter.launchRequest.ID != "" {
		t.Fatal("agent was launched despite the failed validation")
	}
}

// On resume the stored kickoff is not replayed, so the reread instruction rides
// on the instructions channel; and the launch fails if the selection is broken.
func TestResumeAppendsRereadInstructionAndRevalidatesSelection(t *testing.T) {
	t.Parallel()
	repoPath := gitRepoDir(t)
	workspace := testWorkspace(t, map[string]string{"daemon": repoPath})
	a := writeWorkflow(t, workspace, "A.md", "---\nattach: manual\n---\n\nA.\n")

	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID: "session-1", ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket, ContextID: "ticket-1",
		Prompt: "kickoff", Workdir: repoPath, Workflows: []string{a}, Instructions: "worker",
		CurrentRun: &domain.SessionRun{
			ID: "run-1", Status: domain.SessionRunStatusRunning, ProfileName: "codex",
			ProfileSnapshot: &domain.AgentProfileSnapshot{Agent: "codex"}, Workdir: repoPath, NativeID: "native-1",
		},
	}
	repo.listedSessions = []domain.Session{repo.createdSession}
	adapter := &fakeAdapter{}
	service := &Service{
		intents: newIntentStore(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:  testRuntimeConfigWithPaths(workspace, repoPath),
		repo: repo, tickets: &fakeTicketService{}, receiver: ingest.NewReceiver(adapter),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{agentruntime.AgentCodex: adapter},
		terminal: &fakeTerminalManager{}, eventStreams: map[string]map[uint64]chan domain.SessionEvent{}, bridgeCancels: map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}

	if err := service.RestoreRunningSessions(context.Background()); err != nil {
		t.Fatalf("RestoreRunningSessions: %v", err)
	}
	if repo.failedRunID != "" {
		t.Fatalf("restore marked run failed: %s", repo.failedRunReason)
	}
	if !adapter.launchRequest.Resume || adapter.launchRequest.ResumeID != "native-1" {
		t.Fatalf("restore did not resume the native session: %#v", adapter.launchRequest)
	}
	if adapter.launchRequest.Prompt != "" {
		t.Fatalf("resume replayed the kickoff prompt: %q", adapter.launchRequest.Prompt)
	}
	if !strings.HasPrefix(adapter.launchRequest.Instructions, "worker\n\n## Session resumed") || !strings.Contains(adapter.launchRequest.Instructions, a) {
		t.Fatalf("resume instructions do not tell the agent to reread %s:\n%s", a, adapter.launchRequest.Instructions)
	}

	// Break the selection and resume again through the main-terminal-exit path.
	if err := os.Remove(a); err != nil {
		t.Fatalf("remove workflow: %v", err)
	}
	_, err := service.resumeSessionMainTerminal(context.Background(), repo.createdSession, *repo.createdSession.CurrentRun, terminalSize{Cols: 80, Rows: 24})
	if err == nil || !strings.Contains(err.Error(), a) {
		t.Fatalf("resume with a missing selected workflow must fail naming it, got %v", err)
	}
}

// Every provider must receive the daemon's instructions additively, with the
// kickoff kept separate. This drives the real adapters so a regression in the
// daemon's adapter options (Claude's append flag) is caught here.
func TestLaunchKeepsNativeInstructionsAndSeparateKickoff(t *testing.T) {
	t.Parallel()
	service, err := New(context.Background(), config.Config{}, nil, newFakeSessionRepository(), &fakeTicketService{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for kind, adapter := range service.adapters {
		spec, err := adapter.PrepareLaunch(context.Background(), agentruntime.StartRequest{
			ID: "s1", Agent: kind, Workdir: t.TempDir(), Prompt: "KICKOFF-TEXT", Instructions: "ROLE-TEXT", Yolo: true,
		})
		if err != nil {
			t.Fatalf("%s PrepareLaunch: %v", kind, err)
		}
		args := strings.Join(spec.Args, "\x00")
		switch kind {
		case agentruntime.AgentClaude:
			if !strings.Contains(args, "--append-system-prompt\x00ROLE-TEXT") {
				t.Fatalf("claude must append instructions, got %q", spec.Args)
			}
			if strings.Contains(args, "--system-prompt\x00") {
				t.Fatalf("claude replaced its native system prompt: %q", spec.Args)
			}
		case agentruntime.AgentCodex:
			if !strings.Contains(args, "developer_instructions=") || !strings.Contains(args, "ROLE-TEXT") {
				t.Fatalf("codex must carry instructions as developer_instructions, got %q", spec.Args)
			}
		case agentruntime.AgentOpenCode:
			// OpenCode writes the instructions to a file referenced from its
			// config; the file must hold exactly the role text.
			var found bool
			for _, path := range spec.CleanupPaths {
				data, readErr := os.ReadFile(path)
				if readErr == nil && string(data) == "ROLE-TEXT" {
					found = true
				}
			}
			if !found {
				t.Fatalf("opencode instruction file with the role text not found in %v", spec.CleanupPaths)
			}
		}
		if !strings.Contains(args, "KICKOFF-TEXT") {
			t.Fatalf("%s lost the kickoff prompt: %q", kind, spec.Args)
		}
		for _, path := range spec.CleanupPaths {
			_ = os.Remove(path)
		}
	}
}
