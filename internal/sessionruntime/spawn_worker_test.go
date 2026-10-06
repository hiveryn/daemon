package sessionruntime

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// spawnFixture is an architect with a real workspace, a real ticket board and
// a real session store, so spawn tests assert what the desktop would see.
type spawnFixture struct {
	*actionFixture
	architect domain.Session
	workspace string
	repoPath  string
	tickets   *architectfs.TicketService

	eventsMu sync.Mutex
	events   []domain.ArchitectEvent
}

func newSpawnFixture(t *testing.T) *spawnFixture {
	t.Helper()
	f := &spawnFixture{actionFixture: newActionFixture(t), tickets: architectfs.NewTicketService()}
	f.service.tickets = f.tickets
	f.service.cfg.IntentWaitTimeout = 600
	// A second variant name on the fixture's only (codex) adapter.
	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "codex"}
	f.service.SetArchitectPublisher(func(_ string, event domain.ArchitectEvent) {
		f.eventsMu.Lock()
		defer f.eventsMu.Unlock()
		f.events = append(f.events, event)
	})
	f.architect = f.actionFixture.architect(t, "hiveryn")
	f.repoPath = t.TempDir()
	if err := os.Mkdir(f.repoPath+"/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	f.workspace = testWorkspace(t, map[string]string{"daemon": f.repoPath})
	f.service.cfg.Architects["hiveryn"] = config.ArchitectConfig{Name: "hiveryn", Path: f.workspace, Repos: map[string]string{"daemon": f.repoPath}}
	return f
}

func (f *spawnFixture) backlogTicket(t *testing.T, title string) domain.Ticket {
	t.Helper()
	ticket, err := f.tickets.CreateTicket(context.Background(), f.workspace, domain.CreateTicketParams{Title: title, Repo: "daemon", Body: "Do it.", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	return ticket
}

func (f *spawnFixture) ticketStatus(t *testing.T, id string) domain.TicketStatus {
	t.Helper()
	ticket, err := f.tickets.GetTicket(context.Background(), f.workspace, id)
	if err != nil {
		t.Fatalf("get ticket: %v", err)
	}
	return ticket.Status
}

func (f *spawnFixture) ticketSessions(t *testing.T) []domain.Session {
	t.Helper()
	sessions, err := f.sessions.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Session
	for _, s := range sessions {
		if s.SessionType == domain.SessionTypeTicket {
			out = append(out, s)
		}
	}
	return out
}

type spawnOutcome struct {
	res domain.SpawnTicketWorkerResponse
	err error
}

// spawn raises a request and returns the pending intent once it is shown,
// with the channel its resolution arrives on.
func (f *spawnFixture) spawn(t *testing.T, req domain.SpawnTicketWorkerRequest) (domain.Intent, chan spawnOutcome) {
	t.Helper()
	before := map[string]bool{}
	for _, id := range f.service.intents.PendingForSession(f.architect.ID) {
		before[id] = true
	}
	done := make(chan spawnOutcome, 1)
	go func() {
		res, err := f.service.RequestSpawnTicketWorker(context.Background(), f.architect.ID, req)
		done <- spawnOutcome{res, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case out := <-done:
			t.Fatalf("spawnTicketWorker resolved before it was shown: %+v, %v", out.res, out.err)
		default:
		}
		for _, id := range f.service.intents.PendingForSession(f.architect.ID) {
			if before[id] {
				continue
			}
			if in, ok := f.service.intents.GetForSession(f.architect.ID, id); ok {
				return in, done
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("spawnTicketWorker was never shown")
	return domain.Intent{}, nil
}

func wait(t *testing.T, done chan spawnOutcome) spawnOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("spawnTicketWorker did not resolve")
		return spawnOutcome{}
	}
}

func TestSpawnTicketWorkerApprovalLaunchesWorkerWithNamedWorkflows(t *testing.T) {
	f := newSpawnFixture(t)
	a := writeWorkflow(t, f.workspace, "AUTONOMOUS_COMMIT.md", "---\nattach: manual\n---\n\nCommit body.\n")
	b := writeWorkflow(t, f.workspace, "ISOLATED_VERIFICATION.md", "---\nattach: suggested\nrepos:\n- daemon\n---\n\nVerify body.\n")
	writeWorkflow(t, f.workspace, "UNSELECTED.md", "---\nattach: suggested\nrepos:\n- daemon\n---\n\nNot me.\n")
	ticket := f.backlogTicket(t, "Ship spawn")

	in, done := f.spawn(t, domain.SpawnTicketWorkerRequest{
		TicketID:  ticket.ID,
		Variant:   " claude ",
		Workflows: []string{"ISOLATED_VERIFICATION.md", "AUTONOMOUS_COMMIT", "ISOLATED_VERIFICATION"},
	})

	// The approval shows the ticket, the variant and exactly the selection,
	// normalized: no suffix, no duplicate, no repo-matched suggestion.
	if in.Type != domain.IntentTypeSpawnTicketWorker || in.Policy != domain.IntentPolicyWaitThenAllow || in.Summary != "Ship spawn" {
		t.Fatalf("intent = %+v", in)
	}
	if in.Payload["ticket_id"] != ticket.ID || in.Payload["variant"] != "claude" || in.Payload["repo"] != "daemon" {
		t.Fatalf("payload = %+v", in.Payload)
	}
	if got, _ := in.Payload["workflows"].([]string); !slices.Equal(got, []string{"ISOLATED_VERIFICATION", "AUTONOMOUS_COMMIT"}) {
		t.Fatalf("payload workflows = %#v", in.Payload["workflows"])
	}
	if len(f.ticketSessions(t)) != 0 || f.ticketStatus(t, ticket.ID) != domain.TicketStatusBacklog {
		t.Fatal("something launched before approval")
	}

	if _, err := f.service.ApproveIntent(context.Background(), f.architect.ID, in.ID, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	out := wait(t, done)
	if out.err != nil || out.res.Outcome != domain.IntentOutcomeApproved || out.res.IntentID != in.ID || out.res.Worker == nil {
		t.Fatalf("spawn = %+v, %v", out.res, out.err)
	}
	worker := *out.res.Worker
	if worker.TicketID != ticket.ID || worker.Variant != "claude" || worker.RunID == "" || worker.MainTerminalID == "" ||
		!slices.Equal(worker.Workflows, []string{"ISOLATED_VERIFICATION", "AUTONOMOUS_COMMIT"}) ||
		!slices.Equal(worker.WorkflowPaths, []string{b, a}) {
		t.Fatalf("worker = %+v", worker)
	}

	sessions := f.ticketSessions(t)
	if len(sessions) != 1 || sessions[0].ID != worker.SessionID {
		t.Fatalf("ticket sessions = %+v", sessions)
	}
	s := sessions[0]
	if s.CreatedBy != domain.SessionCreatedByArchitect || s.ArchitectKey != "hiveryn" || s.ContextID != ticket.ID ||
		!slices.Equal(s.Workflows, []string{b, a}) || s.CurrentRun == nil || s.CurrentRun.Status != domain.SessionRunStatusRunning || s.CurrentRun.ProfileName != "claude" {
		t.Fatalf("worker session = %+v run %+v", s, s.CurrentRun)
	}
	if f.ticketStatus(t, ticket.ID) != domain.TicketStatusProgress {
		t.Fatal("ticket not moved to progress")
	}
	// The new run's first message carries the selected bodies, in order.
	prompt := f.adapter.launchRequest.Prompt
	iv, ic := strings.Index(prompt, "Verify body."), strings.Index(prompt, "Commit body.")
	if iv < 0 || ic < 0 || iv > ic || strings.Contains(prompt, "Not me.") {
		t.Fatalf("launch prompt does not carry exactly the selected workflows in order:\n%s", prompt)
	}
	// Discoverable by the desktop exactly like its own launch.
	f.eventsMu.Lock()
	defer f.eventsMu.Unlock()
	if !slices.ContainsFunc(f.events, func(e domain.ArchitectEvent) bool {
		return e.Reason == domain.ArchitectEventSessionStarted && e.SessionID == worker.SessionID
	}) {
		t.Fatalf("no session_started for the worker: %+v", f.events)
	}
}

func TestSpawnTicketWorkerEmptySelectionAndAutoApproval(t *testing.T) {
	f := newSpawnFixture(t)
	f.service.cfg.IntentWaitTimeout = 1
	writeWorkflow(t, f.workspace, "SUGGESTED.md", "---\nattach: suggested\nrepos:\n- daemon\n---\n\nSuggested.\n")
	ticket := f.backlogTicket(t, "No workflows")

	res, err := f.service.RequestSpawnTicketWorker(context.Background(), f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex"})
	if err != nil || res.Outcome != domain.IntentOutcomeAutoApproved || res.Worker == nil {
		t.Fatalf("spawn = %+v, %v", res, err)
	}
	if len(res.Worker.Workflows) != 0 || len(res.Worker.WorkflowPaths) != 0 {
		t.Fatalf("empty selection gained workflows: %+v", res.Worker)
	}
	if !strings.Contains(f.adapter.launchRequest.Prompt, "Workflows selected for this session: none") {
		t.Fatalf("prompt = %s", f.adapter.launchRequest.Prompt)
	}
}

func TestSpawnTicketWorkerDenialLaunchesNothing(t *testing.T) {
	f := newSpawnFixture(t)
	ticket := f.backlogTicket(t, "Deny me")

	in, done := f.spawn(t, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex"})
	if err := f.service.DenyIntent(context.Background(), f.architect.ID, in.ID, "not now"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	out := wait(t, done)
	if out.err != nil || out.res.Outcome != domain.IntentOutcomeDeniedByUser || out.res.Reason != "not now" || out.res.Worker != nil {
		t.Fatalf("spawn = %+v, %v", out.res, out.err)
	}
	if len(f.ticketSessions(t)) != 0 || f.ticketStatus(t, ticket.ID) != domain.TicketStatusBacklog || len(f.terminal.startSpecs) != 0 {
		t.Fatal("denied request launched something")
	}
}

// Everything the launch would refuse is refused before the user is asked.
func TestSpawnTicketWorkerValidatesBeforeApproval(t *testing.T) {
	f := newSpawnFixture(t)
	writeWorkflow(t, f.workspace, "AUTONOMOUS_COMMIT.md", "---\nattach: manual\n---\n\nA.\n")
	writeWorkflow(t, f.workspace, "BROKEN.md", "no frontmatter\n")
	ticket := f.backlogTicket(t, "Validate")
	done := f.backlogTicket(t, "Already done")
	if _, err := f.tickets.MoveTicket(context.Background(), f.workspace, done.ID, domain.MoveTicketParams{To: domain.TicketStatusProgress}); err != nil {
		t.Fatal(err)
	}
	other := f.actionFixture.architect(t, "other")
	_ = other

	ctx := context.Background()
	cases := []struct {
		name string
		req  domain.SpawnTicketWorkerRequest
		want []string
	}{
		{"missing ticket", domain.SpawnTicketWorkerRequest{Variant: "codex"}, []string{"ticket_id", "required"}},
		{"missing variant", domain.SpawnTicketWorkerRequest{TicketID: ticket.ID}, []string{"no default", "claude (codex)", "codex (codex)"}},
		{"unknown variant", domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "gpt"}, []string{`"gpt" is not a configured agent variant`, "claude (codex), codex (codex)"}},
		{"unknown workflow", domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"AUTONOMOUS_COMMIT", "NOPE.md"}}, []string{`workflows[1]: "NOPE.md" is not a workflow`, "available workflows: AUTONOMOUS_COMMIT, BROKEN"}},
		{"relative path", domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"../PROJECT_STATE.md"}}, []string{"is a path"}},
		{"absolute path", domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{f.workspace + "/workflows/AUTONOMOUS_COMMIT.md"}}, []string{"is a path"}},
		{"invalid workflow", domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"BROKEN"}}, []string{"not launchable", "BROKEN.md"}},
		{"not backlog", domain.SpawnTicketWorkerRequest{TicketID: done.ID, Variant: "codex"}, []string{"backlog"}},
		{"other board", domain.SpawnTicketWorkerRequest{TicketID: "2026-01-01-0000-elsewhere", Variant: "codex"}, []string{"not found"}},
	}
	for _, tc := range cases {
		_, err := f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, tc.req)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not mention %q", tc.name, err, want)
			}
		}
	}
	if pending := f.service.intents.PendingForSession(f.architect.ID); len(pending) != 0 {
		t.Fatalf("a refused request was shown: %v", pending)
	}

	// Only architects may spawn workers.
	worker := f.backlogTicket(t, "Worker caller")
	params, err := f.service.ticketSessionParams(ctx, "hiveryn", f.service.cfg.Architects["hiveryn"], worker.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := f.sessions.CreateSession(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestSpawnTicketWorker(ctx, ws.ID, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex"}); err == nil || !strings.Contains(err.Error(), "only architect sessions") {
		t.Fatalf("worker spawn = %v", err)
	}
}

// What changed while the request was pending is checked again on approval,
// and a launch that cannot happen is an error with its cause — never success.
func TestSpawnTicketWorkerRevalidatesOnApproval(t *testing.T) {
	f := newSpawnFixture(t)
	path := writeWorkflow(t, f.workspace, "AUTONOMOUS_COMMIT.md", "---\nattach: manual\n---\n\nA.\n")
	ticket := f.backlogTicket(t, "Revalidate")

	in, done := f.spawn(t, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"AUTONOMOUS_COMMIT"}})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ApproveIntent(context.Background(), f.architect.ID, in.ID, nil); err == nil {
		t.Fatal("approval of an unlaunchable request reported success to the desktop")
	}
	out := wait(t, done)
	if out.err != nil || out.res.Outcome != domain.IntentOutcomeError || out.res.Worker != nil || !strings.Contains(out.res.Reason, "AUTONOMOUS_COMMIT") {
		t.Fatalf("spawn = %+v, %v", out.res, out.err)
	}
	if len(f.ticketSessions(t)) != 0 || f.ticketStatus(t, ticket.ID) != domain.TicketStatusBacklog {
		t.Fatal("unlaunchable request left a session or claimed the ticket")
	}
}

func TestSpawnTicketWorkerLaunchFailureKeepsCauseAndRemovesSession(t *testing.T) {
	f := newSpawnFixture(t)
	f.terminal.startErr = errors.New("pty launch failed")
	ticket := f.backlogTicket(t, "Fail launch")

	in, done := f.spawn(t, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex"})
	if _, err := f.service.ApproveIntent(context.Background(), f.architect.ID, in.ID, nil); err == nil || !strings.Contains(err.Error(), "pty launch failed") {
		t.Fatalf("approve = %v", err)
	}
	out := wait(t, done)
	if out.err != nil || out.res.Outcome != domain.IntentOutcomeError || out.res.Worker != nil || !strings.Contains(out.res.Reason, "pty launch failed") {
		t.Fatalf("spawn = %+v, %v", out.res, out.err)
	}
	if len(f.ticketSessions(t)) != 0 || f.ticketStatus(t, ticket.ID) != domain.TicketStatusBacklog {
		t.Fatal("failed launch left a session or claimed the ticket")
	}
}

// A retry is the same request — NAME and NAME.md alike — and never a second
// worker; another request for a ticket that already has one is refused.
func TestSpawnTicketWorkerNeverDuplicatesWorkers(t *testing.T) {
	f := newSpawnFixture(t)
	writeWorkflow(t, f.workspace, "AUTONOMOUS_COMMIT.md", "---\nattach: manual\n---\n\nA.\n")
	ticket := f.backlogTicket(t, "Once")
	ctx := context.Background()

	in, first := f.spawn(t, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"AUTONOMOUS_COMMIT"}})
	retry := make(chan spawnOutcome, 1)
	go func() {
		res, err := f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"AUTONOMOUS_COMMIT.md"}})
		retry <- spawnOutcome{res, err}
	}()
	time.Sleep(50 * time.Millisecond)
	if pending := f.service.intents.PendingForSession(f.architect.ID); len(pending) != 1 {
		t.Fatalf("retry minted another request: %v", pending)
	}
	if _, err := f.service.ApproveIntent(ctx, f.architect.ID, in.ID, nil); err != nil {
		t.Fatal(err)
	}
	a, b := wait(t, first), wait(t, retry)
	if a.err != nil || b.err != nil || a.res.Worker == nil || b.res.Worker == nil || a.res.Worker.SessionID != b.res.Worker.SessionID {
		t.Fatalf("first %+v %v / retry %+v %v", a.res, a.err, b.res, b.err)
	}
	// After resolution an identical call replays rather than launching again.
	replay, err := f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex", Workflows: []string{"AUTONOMOUS_COMMIT"}})
	if err != nil || replay.Worker == nil || replay.Worker.SessionID != a.res.Worker.SessionID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	// A different request for the claimed ticket is refused up front.
	if _, err := f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "claude"}); err == nil || !strings.Contains(err.Error(), "backlog") {
		t.Fatalf("second worker request = %v", err)
	}
	if n := len(f.ticketSessions(t)); n != 1 {
		t.Fatalf("ticket sessions = %d, want 1", n)
	}
}

// A desktop launch that claimed the ticket while the request was pending wins:
// the approved request reports the conflict and launches nothing.
func TestSpawnTicketWorkerLosesToDesktopLaunchWhilePending(t *testing.T) {
	f := newSpawnFixture(t)
	ticket := f.backlogTicket(t, "Race")
	ctx := context.Background()

	in, done := f.spawn(t, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "codex"})
	desktop, err := f.service.CreateSession(ctx, domain.CreateSessionRequest{SessionType: domain.SessionTypeTicket, ArchitectKey: "hiveryn", TicketID: ticket.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ApproveIntent(ctx, f.architect.ID, in.ID, nil); err == nil {
		t.Fatal("approval reported success")
	}
	out := wait(t, done)
	if out.err != nil || out.res.Outcome != domain.IntentOutcomeError || out.res.Worker != nil || !strings.Contains(out.res.Reason, "already has an active ticket session") {
		t.Fatalf("spawn = %+v, %v", out.res, out.err)
	}
	sessions := f.ticketSessions(t)
	if len(sessions) != 1 || sessions[0].ID != desktop.ID {
		t.Fatalf("ticket sessions = %+v, want only the desktop's", sessions)
	}
}
