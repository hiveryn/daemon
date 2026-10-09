package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// withRemoteVariant registers machine bk and a codex variant assigned to it.
func withRemoteVariant(cfg *config.Config) {
	cfg.Machines = map[string]config.MachineConfig{"bk": {SSH: "bk"}}
	cfg.Variants["bk-codex"] = config.VariantConfig{Agent: "codex", Machine: "bk"}
}

func requireErr(t *testing.T, label string, err error, wants ...string) {
	t.Helper()
	if !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("%s: err = %v, want a validation error", label, err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err %q does not mention %q", label, err, want)
		}
	}
}

// A worker's variant must belong to the machine its ticket's repositories
// are on; the choices offered are only that machine's.
func TestSpawnTicketWorkerVariantMatchesTicketMachine(t *testing.T) {
	f := newSpawnFixture(t)
	withRemoteVariant(&f.service.cfg)
	arch := f.service.cfg.Architects["hiveryn"]
	arch.Repos["remote"] = "/srv/remote"
	arch.RepoMachines = map[string]string{"remote": "bk"}
	f.service.cfg.Architects["hiveryn"] = arch
	local := f.backlogTicket(t, "Local")
	remote, err := f.tickets.CreateTicket(context.Background(), f.workspace, domain.CreateTicketParams{Title: "Remote", Repo: "remote", Body: "Do it."})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err = f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: local.ID})
	requireErr(t, "local missing", err, "Variants configured for local: claude (codex), codex (codex)")
	if strings.Contains(err.Error(), "bk-codex") {
		t.Fatalf("a remote variant was offered for a local worker: %v", err)
	}
	_, err = f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: local.ID, Variant: "bk-codex"})
	requireErr(t, "remote variant for local ticket", err, `"bk-codex" is configured for machine bk`, "runs on local")
	_, err = f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: remote.ID, Variant: "codex"})
	requireErr(t, "local variant for remote ticket", err, `"codex" is configured for local`, "runs on machine bk", "Variants configured for machine bk: bk-codex (codex)")

	delete(f.service.cfg.Variants, "bk-codex")
	_, err = f.service.RequestSpawnTicketWorker(ctx, f.architect.ID, domain.SpawnTicketWorkerRequest{TicketID: remote.ID, Variant: "codex"})
	requireErr(t, "no remote variants", err, "no agent variant is configured for machine bk", "machine: bk")
	if pending := f.service.intents.PendingForSession(f.architect.ID); len(pending) != 0 {
		t.Fatalf("a refused request was shown: %v", pending)
	}
}

// A variant reassigned to another machine while the request was pending is
// refused when the approved request launches.
func TestSpawnTicketWorkerRechecksVariantMachineOnApproval(t *testing.T) {
	f := newSpawnFixture(t)
	withRemoteVariant(&f.service.cfg)
	ticket := f.backlogTicket(t, "Reassigned")

	in, done := f.spawn(t, domain.SpawnTicketWorkerRequest{TicketID: ticket.ID, Variant: "claude"})
	f.service.cfg.Variants["claude"] = config.VariantConfig{Agent: "codex", Machine: "bk"}
	if _, err := f.service.ApproveIntent(context.Background(), f.architect.ID, in.ID, nil); err == nil {
		t.Fatal("approval of an incompatible variant reported success")
	}
	out := wait(t, done)
	if out.err != nil || out.res.Outcome != domain.IntentOutcomeError || !strings.Contains(out.res.Reason, "configured for machine bk") {
		t.Fatalf("spawn = %+v, %v", out.res, out.err)
	}
	if len(f.ticketSessions(t)) != 0 || f.ticketStatus(t, ticket.ID) != domain.TicketStatusBacklog {
		t.Fatal("incompatible variant launched a worker")
	}
}

// Direct desktop/API launches go through CreateRun, which enforces the same
// match and records the machine in the run's snapshot.
func TestCreateRunEnforcesVariantMachine(t *testing.T) {
	f := newSpawnFixture(t)
	withRemoteVariant(&f.service.cfg)
	ctx := context.Background()
	ticket := f.backlogTicket(t, "Direct")
	params, err := f.service.ticketSessionParams(ctx, "hiveryn", f.service.cfg.Architects["hiveryn"], ticket.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.sessions.CreateSession(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.CreateRun(ctx, session.ID, domain.CreateSessionRunRequest{ProfileName: "bk-codex"})
	requireErr(t, "remote variant", err, "configured for machine bk", "runs on local", "codex (codex)")
	_, err = f.service.CreateRun(ctx, f.architect.ID, domain.CreateSessionRunRequest{ProfileName: "bk-codex"})
	requireErr(t, "architect", err, "architect session runs on local")
	run, err := f.service.CreateRun(ctx, session.ID, domain.CreateSessionRunRequest{ProfileName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.ProfileSnapshot == nil || run.Run.ProfileSnapshot.Machine != "" {
		t.Fatalf("local snapshot = %+v", run.Run.ProfileSnapshot)
	}
	if snap := snapshotVariant(config.VariantConfig{Agent: "codex", Machine: "bk"}, "bk"); snap.Machine != "bk" {
		t.Fatalf("remote snapshot = %+v", snap)
	}
}

// A relaunch uses the frozen variant; one frozen for another machine never
// starts locally, and a snapshot without a machine is not reinterpreted.
func TestStoredRunRefusesSnapshotForAnotherMachine(t *testing.T) {
	f := newSpawnFixture(t)
	session := domain.Session{ArchitectKey: "hiveryn", SessionType: domain.SessionTypeTicket}
	run := domain.SessionRun{ID: "r", ProfileName: "codex", Workdir: f.repoPath, ProfileSnapshot: &domain.AgentProfileSnapshot{Agent: "codex", Machine: "bk"}}
	if _, _, err := f.service.resolveStoredRunLaunchContext(session, run); err == nil || !strings.Contains(err.Error(), "variant for machine bk") {
		t.Fatalf("err = %v", err)
	}
	run.ProfileSnapshot.Machine = ""
	if _, _, err := f.service.resolveStoredRunLaunchContext(session, run); err != nil {
		t.Fatal(err)
	}
}

// Actions run locally whoever requests them, so only local variants qualify.
func TestActionsUseLocalVariants(t *testing.T) {
	f := newActionFixture(t)
	withRemoteVariant(&f.service.cfg)
	f.writeValidAction(t, "demo")
	arch := f.architect(t, "alpha", "demo")
	ctx := context.Background()

	_, err := f.service.LaunchAction(ctx, "demo", domain.LaunchActionRequest{Prompt: "go", ProfileName: "bk-codex"})
	requireErr(t, "manual", err, "configured for machine bk", "Action runs on local", "codex (codex)")
	_, err = f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x", Variant: "bk-codex"})
	requireErr(t, "requested", err, "configured for machine bk", "Variants configured for local: codex (codex)")
	_, err = f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "demo", Prompt: "x"})
	requireErr(t, "missing", err, "Variants configured for local: codex (codex)")
	if strings.Contains(err.Error(), "bk-codex") {
		t.Fatalf("a remote variant was offered for an Action: %v", err)
	}
	if runs, _ := f.service.ListActionRuns(ctx, "", 0); len(runs) != 0 {
		t.Fatalf("refused launches left records: %+v", runs)
	}
}
