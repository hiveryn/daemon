package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

// worker starts a running ticket session under an already configured
// architect, as the desktop's worker launch would.
func (f *actionFixture) worker(t *testing.T, architectKey, ticketID string) domain.Session {
	t.Helper()
	ctx := context.Background()
	session, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{
		ArchitectKey: architectKey,
		SessionType:  domain.SessionTypeTicket,
		ContextID:    ticketID,
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	if err != nil {
		t.Fatalf("create ticket session: %v", err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "codex", Workdir: session.Workdir}); err != nil {
		t.Fatalf("create ticket run: %v", err)
	}
	session, err = f.sessions.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestWorkerRequestsActionsOfItsProject(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	f.writeValidAction(t, "other")
	arch := f.architect(t, "alpha", "demo", "missing")
	w := f.worker(t, "alpha", "ticket-1")
	ctx := context.Background()

	// Discovery is the parent project's availableActions, missing ones included.
	list, err := f.service.AvailableActions(ctx, w.ID)
	if err != nil || len(list.Actions) != 2 || list.Actions[0].Name != "demo" || !list.Actions[0].Valid || list.Actions[1].Valid {
		t.Fatalf("worker discovery = %+v, %v", list, err)
	}
	if _, err := f.service.RequestExecuteAction(ctx, w.ID, domain.ExecuteActionRequest{Name: "other", Prompt: "x", Variant: "codex"}); !errors.As(err, new(*domain.ValidationError)) || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("unlisted action err = %v, want not available", err)
	}

	pending := f.request(t, w.ID, "demo", "compare")
	if pending.Status != domain.ActionRunPendingApproval {
		t.Fatalf("pending = %+v", pending)
	}
	run, _ := f.service.GetActionRun(ctx, pending.ExecutionID)
	if run.Trigger != domain.ActionRunTriggerWorker || run.ArchitectKey != "alpha" || run.RequesterSessionID != w.ID || run.RequesterTicketID != "ticket-1" {
		t.Fatalf("worker request record = %+v, want trigger worker attributed to its ticket", run)
	}
	in, ok := f.service.intents.Get(pending.ExecutionID)
	if !ok || in.Policy != domain.IntentPolicyWaitThenAllow || in.Origin.SessionType != domain.SessionTypeTicket || in.Origin.TicketID != "ticket-1" || in.Origin.ArchitectKey != "alpha" {
		t.Fatalf("intent = %+v, %v; want a wait-then-allow request from the ticket session", in, ok)
	}

	// Same execution id throughout; the architect of the project reads it too.
	if err := f.approve(w.ID, pending.ExecutionID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := f.result(t, arch.ID, pending.ExecutionID); got.Status != domain.ActionRunRunning || got.ExecutionID != pending.ExecutionID {
		t.Fatalf("architect read = %+v, want running", got)
	}
	stranger := f.architect(t, "beta", "demo")
	if _, err := f.service.GetActionResult(ctx, stranger.ID, pending.ExecutionID); !errors.As(err, new(*domain.NotFoundError)) {
		t.Fatalf("other project read err = %v, want not found", err)
	}

	// The worker ending leaves the approved execution to its own lifecycle.
	f.endArchitect(t, w)
	run, _ = f.service.GetActionRun(ctx, pending.ExecutionID)
	if run.Status != domain.ActionRunRunning {
		t.Fatalf("after worker end = %+v, want still running", run)
	}
	if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conclude(t, run.SessionID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "delivered"}); err != nil {
		t.Fatal(err)
	}
	later := f.worker(t, "alpha", "ticket-2")
	if got := f.result(t, later.ID, pending.ExecutionID); got.Status != domain.ActionRunCompleted || got.Summary != "delivered" {
		t.Fatalf("later worker read = %+v, want completed", got)
	}

	// A worker's pending request fails as never run when the worker ends.
	abandoned := f.request(t, later.ID, "demo", "abandoned")
	f.endArchitect(t, later)
	if got := f.result(t, arch.ID, abandoned.ExecutionID); got.Status != domain.ActionRunFailed || got.StartedAt != nil || !strings.Contains(got.Error, "session ended") {
		t.Fatalf("abandoned = %+v, want failed as never run", got)
	}
}

func TestAddAvailableActionIsArchitectOnly(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha")
	path := filepath.Join(f.service.cfg.Architects["alpha"].Path, "hiveryn.yaml")
	if err := os.WriteFile(path, []byte("name: alpha\nrepos:\n    daemon: /tmp/daemon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	added, err := f.service.AddAvailableAction(ctx, arch.ID, domain.AddAvailableActionRequest{Name: " demo "})
	if err != nil || !added.Changed || !slices.Equal(added.AvailableActions, []string{"demo"}) {
		t.Fatalf("add = %+v, %v", added, err)
	}
	// A name missing from the library is allowed; discovery reports it.
	added, err = f.service.AddAvailableAction(ctx, arch.ID, domain.AddAvailableActionRequest{Name: "later"})
	if err != nil || !slices.Equal(added.AvailableActions, []string{"demo", "later"}) {
		t.Fatalf("add missing = %+v, %v", added, err)
	}
	again, err := f.service.AddAvailableAction(ctx, arch.ID, domain.AddAvailableActionRequest{Name: "demo"})
	if err != nil || again.Changed || !slices.Equal(again.AvailableActions, []string{"demo", "later"}) {
		t.Fatalf("repeat = %+v, %v; want unchanged", again, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "daemon: /tmp/daemon") {
		t.Fatalf("unrelated config lost: %s", data)
	}

	for _, bad := range []string{"", "Bad Name"} {
		if _, err := f.service.AddAvailableAction(ctx, arch.ID, domain.AddAvailableActionRequest{Name: bad}); !errors.As(err, new(*domain.ValidationError)) {
			t.Errorf("name %q err = %v, want validation error", bad, err)
		}
	}

	w := f.worker(t, "alpha", "ticket-1")
	if _, err := f.service.AddAvailableAction(ctx, w.ID, domain.AddAvailableActionRequest{Name: "other"}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("worker add err = %v, want validation error", err)
	}
	launched := f.launch(t, "demo", "go")
	if _, err := f.service.AddAvailableAction(ctx, launched.Session.ID, domain.AddAvailableActionRequest{Name: "other"}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("action agent add err = %v, want validation error", err)
	}

	// A config that does not load is reported and left for repair.
	broken := "name: alpha\nprompts: {}\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AddAvailableAction(ctx, arch.ID, domain.AddAvailableActionRequest{Name: "other"}); !errors.As(err, new(*domain.ValidationError)) || !strings.Contains(err.Error(), "repair") {
		t.Fatalf("broken config err = %v, want validation error asking for repair", err)
	}
	if data, _ := os.ReadFile(path); string(data) != broken {
		t.Fatalf("broken config rewritten: %s", data)
	}
}
