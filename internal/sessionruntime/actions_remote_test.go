package sessionruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

// These tests run remote Actions against sshtest's fake machines: every SSH
// command runs locally in the machine's own home directory. They cover the
// daemon's remote logic — registration, inspection, conclusion snapshots,
// cross-machine delivery and cleanup — not a real SSH server or provider.

// remoteActionFixture registers machines "box" (where the Action lives) and
// "other" (a remote worker's machine), with a variant for each and a local
// one.
func remoteActionFixture(t *testing.T) (*actionFixture, *sshtest.Fake) {
	t.Helper()
	fake := sshtest.Install(t)
	f := newActionFixture(t)
	f.service.cfg.Machines = map[string]config.MachineConfig{"box": {SSH: "box"}, "other": {SSH: "other"}}
	f.service.cfg.Variants["box-codex"] = config.VariantConfig{Agent: "codex", Machine: "box"}
	f.service.cfg.Variants["other-codex"] = config.VariantConfig{Agent: "codex", Machine: "other"}
	return f, fake
}

// writeRemoteAction creates a valid checkout in box's home and registers it
// there.
func (f *actionFixture) writeRemoteAction(t *testing.T, fake *sshtest.Fake, name string) string {
	t.Helper()
	dir := filepath.Join(fake.Home("box"), "actions", name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		"action.yaml": "name: " + name + "\ndescription: Collects evidence remotely.\nartifacts: summary.md\nsuggestions: [Collect synthetic evidence]\n",
		"KICKOFF.md":  testActionKickoff,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.registry[name] = config.ActionRegistration{Path: dir, Machine: "box"}
	return dir
}

// remoteExecution records a running execution of name on box with a live
// remote action session, as an approved launch leaves it (the provider launch
// itself is a remote worker's and is covered there).
func (f *actionFixture) remoteExecution(t *testing.T, fake *sshtest.Fake, name, architectKey string) (domain.ActionRun, domain.Session) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	id := "exec-" + strings.ReplaceAll(t.Name(), "/", "-")
	output := filepath.Join(fake.Home("box"), remoteActionsStateDir, "runs", name, id)
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	run := domain.ActionRun{
		ID: id, Action: name, Trigger: domain.ActionRunTriggerArchitect, Status: domain.ActionRunRunning, Prompt: "collect",
		ArtifactContract: "evidence", ProfileName: "box-codex", RepoPath: f.registry[name].Path, OutputDir: output, Machine: "box",
		CreatedAt: now, StartedAt: &now, ArchitectKey: architectKey,
	}
	if err := f.runs.CreateActionRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	session, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{
		Machine: "box", SSH: "box", RemoteToken: strings.Repeat("a", 64), RemotePort: 20001,
		SessionType: domain.SessionTypeAction, ContextID: id, Prompt: "kickoff", Workdir: run.RepoPath,
		AdditionalRepos: []string{actionOutputAdditionalRepoKey}, AdditionalWorkdirs: []string{output},
		CreatedBy: domain.SessionCreatedByDesktop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "box-codex", Workdir: run.RepoPath, AdditionalRepos: session.AdditionalRepos, AdditionalWorkdirs: session.AdditionalWorkdirs}); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.SetActionRunSession(ctx, id, session.ID); err != nil {
		t.Fatal(err)
	}
	run.SessionID = session.ID
	session, _ = f.sessions.GetSession(ctx, session.ID)
	return run, session
}

// remoteWorker is a ticket worker of architectKey running on machine.
func (f *actionFixture) remoteWorker(t *testing.T, architectKey, ticketID, machine string) domain.Session {
	t.Helper()
	ctx := context.Background()
	session, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{
		Machine: machine, SSH: machine, ArchitectKey: architectKey, SessionType: domain.SessionTypeTicket,
		ContextID: ticketID, Prompt: "kickoff", Workdir: "/srv/" + ticketID, CreatedBy: domain.SessionCreatedByDesktop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: machine + "-codex", Workdir: session.Workdir}); err != nil {
		t.Fatal(err)
	}
	session, _ = f.sessions.GetSession(ctx, session.ID)
	return session
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(data)
}

func TestRemoteActionsAreDiscoveredWhereTheyAreRegistered(t *testing.T) {
	f, fake := remoteActionFixture(t)
	dir := f.writeRemoteAction(t, fake, "evidence")
	f.writeValidAction(t, "local")
	arch := f.architect(t, "alpha", "evidence", "local")
	ctx := context.Background()

	list, err := f.service.ListActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]domain.ActionDefinition{}
	for _, def := range list.Actions {
		byName[def.Name] = def
	}
	remote := byName["evidence"]
	if !remote.Valid || remote.Machine != "box" || remote.Path != dir || strings.Join(remote.Variants, ",") != "box-codex" {
		t.Fatalf("remote definition = %+v", remote)
	}
	if local := byName["local"]; !local.Valid || local.Machine != "" || strings.Join(local.Variants, ",") != "codex" {
		t.Fatalf("local definition = %+v", local)
	}

	available, err := f.service.AvailableActions(ctx, arch.ID)
	if err != nil || len(available.Actions) != 2 || available.Actions[0].Machine != "box" || strings.Join(available.Actions[0].Suggestions, ",") != "Collect synthetic evidence" {
		t.Fatalf("available = %+v, %v", available, err)
	}

	// The variant must suit the Action's machine, wherever the caller is.
	_, err = f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "evidence", Prompt: "x", Variant: "codex"})
	requireErr(t, "local variant for a remote Action", err, `"codex" is configured for local`, "runs on machine box", "box-codex (codex)")
	_, err = f.service.LaunchAction(ctx, "evidence", domain.LaunchActionRequest{Prompt: "x", ProfileName: "other-codex"})
	requireErr(t, "other machine's variant", err, "configured for machine other", "Action runs on machine box")

	// An unreachable machine is reported on the definition, not hidden, and
	// a request for it is refused before any approval.
	fake.SetDown(t, "box", true)
	list, err = f.service.ListActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range list.Actions {
		if def.Name == "evidence" && (def.Valid || len(def.Problems) != 1 || !strings.Contains(def.Problems[0].Message, "cannot inspect the checkout on machine box")) {
			t.Fatalf("unreachable definition = %+v", def)
		}
	}
	_, err = f.service.RequestExecuteAction(ctx, arch.ID, domain.ExecuteActionRequest{Name: "evidence", Prompt: "x", Variant: "box-codex"})
	if err == nil || !strings.Contains(err.Error(), "cannot inspect") {
		t.Fatalf("request for an unreachable Action: %v", err)
	}
	if runs, _ := f.service.ListActionRuns(ctx, "", 0); len(runs) != 0 {
		t.Fatalf("refused requests left records: %+v", runs)
	}
}

func TestRemoteConclusionFreezesThePackageAndDeliversItToEveryCaller(t *testing.T) {
	for _, outcome := range []domain.ActionConclusionOutcome{domain.ActionConclusionCompleted, domain.ActionConclusionFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			f, fake := remoteActionFixture(t)
			f.writeRemoteAction(t, fake, "evidence")
			arch := f.architect(t, "alpha", "evidence")
			ctx := context.Background()
			run, session := f.remoteExecution(t, fake, "evidence", "alpha")

			// The folder is checked on the execution machine before the popup.
			_, err := f.service.DeliverActionArtifacts(ctx, session.ID, run.OutputDir)
			requireErr(t, "empty folder", err, "is empty")
			_, err = f.service.DeliverActionArtifacts(ctx, session.ID, "relative/out")
			requireErr(t, "relative folder", err, "absolute path on machine box")
			if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("v1"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc/hosts", filepath.Join(run.OutputDir, "hosts")); err != nil {
				t.Fatal(err)
			}
			_, err = f.service.DeliverActionArtifacts(ctx, session.ID, run.OutputDir)
			requireErr(t, "symlink", err, "only regular files and directories", "hosts")
			if err := os.Remove(filepath.Join(run.OutputDir, "hosts")); err != nil {
				t.Fatal(err)
			}
			_, err = f.service.DeliverActionArtifacts(ctx, session.ID, run.OutputDir)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := f.service.DeliverActionArtifacts(context.Background(), session.ID, run.OutputDir); err != nil {
				t.Fatal(err)
			}
			concluded, err := f.conclude(t, session.ID, domain.ConcludeActionRequest{Outcome: outcome, Summary: "collected"})
			if err != nil {
				t.Fatal(err)
			}
			pkgPath := concluded.Package.Path
			if concluded.Status != domain.ActionRunStatus(outcome) || concluded.Package == nil || concluded.Package.Machine != "box" || concluded.Package.Path != pkgPath ||
				concluded.Package.State != domain.ActionArtifactsReady || concluded.Package.Files != 1 {
				t.Fatalf("concluded = %+v (package %+v)", concluded, concluded.Package)
			}
			if _, err := f.sessions.GetSession(ctx, session.ID); err == nil {
				t.Fatal("the remote action session survived its conclusion")
			}

			// The original folder is preserved; later edits never reach the package.
			if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("v2"), 0o644); err != nil {
				t.Fatal(err)
			}

			// A local architect gets a verified copy on the laptop.
			result := f.result(t, arch.ID, run.ID)
			localCopy := filepath.Join(f.artRoot, "deliveries", run.ID)
			if result.Artifacts.State != domain.ActionArtifactsReady || result.OutputDir != localCopy || result.Machine != "box" ||
				result.Artifacts.Source == nil || result.Artifacts.Source.Machine != "box" || result.Artifacts.ExpiresAt == nil {
				t.Fatalf("local caller result = %+v", result)
			}
			if got := readFile(t, filepath.Join(localCopy, "summary.md")); got != "v1" {
				t.Fatalf("local copy holds %q, want the frozen v1", got)
			}

			// A worker on the execution machine reads the package in place.
			same := f.remoteWorker(t, "alpha", "t-same", "box")
			if got := f.result(t, same.ID, run.ID); got.Artifacts.State != domain.ActionArtifactsReady || got.OutputDir != pkgPath {
				t.Fatalf("same-machine caller = %+v", got)
			}

			// A worker on another machine gets a copy there, through the laptop; a
			// repeated read reuses it without copying again.
			other := f.remoteWorker(t, "alpha", "t-other", "other")
			otherCopy := filepath.Join(fake.Home("other"), remoteActionsStateDir, "artifacts", "deliveries", run.ID)
			got := f.result(t, other.ID, run.ID)
			if got.Artifacts.State != domain.ActionArtifactsReady || got.OutputDir != otherCopy || readFile(t, filepath.Join(otherCopy, "summary.md")) != "v1" {
				t.Fatalf("other-machine caller = %+v", got)
			}
			copies := len(fake.Commands("other"))
			if again := f.result(t, other.ID, run.ID); again.OutputDir != otherCopy || len(fake.Commands("other")) != copies {
				t.Fatalf("repeat read = %+v, ssh commands %d -> %d", again, copies, len(fake.Commands("other")))
			}

			view, err := f.service.GetActionRun(ctx, run.ID)
			if err != nil || view.Package == nil || len(view.Deliveries) != 2 {
				t.Fatalf("run view = %+v, %v", view, err)
			}
		})
	}
}

func TestDeliveryFailuresAreSeparateFromCompletionAndRetried(t *testing.T) {
	f, fake := remoteActionFixture(t)
	f.writeRemoteAction(t, fake, "evidence")
	f.architect(t, "alpha", "evidence")
	ctx := context.Background()
	run, session := f.remoteExecution(t, fake, "evidence", "alpha")
	if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.DeliverActionArtifacts(context.Background(), session.ID, run.OutputDir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conclude(t, session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	worker := f.remoteWorker(t, "alpha", "t-1", "other")

	// The caller's machine is unreachable: the copy fails, the execution
	// stays completed, and no partial copy is offered.
	fake.SetDown(t, "other", true)
	failed := f.result(t, worker.ID, run.ID)
	if failed.Status != domain.ActionRunCompleted || failed.Artifacts.State != domain.ActionArtifactsFailed || failed.OutputDir != "" ||
		!strings.Contains(failed.Artifacts.Error, "Connection refused") {
		t.Fatalf("failed delivery = %+v", failed)
	}

	// Reading again retries, and succeeds once the machine is back.
	fake.SetDown(t, "other", false)
	wait, err := f.service.WaitForActionResult(ctx, worker.ID, run.ID, 10*time.Second)
	if err != nil || wait.Result.Artifacts.State != domain.ActionArtifactsReady || wait.Result.OutputDir == "" || !wait.Changed {
		t.Fatalf("retried delivery = %+v, %v", wait, err)
	}
	if _, err := os.Stat(wait.Result.OutputDir + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial copy left behind: %v", err)
	}
	// A wait on a final execution whose copy is ready returns at once.
	if again, err := f.service.WaitForActionResult(ctx, worker.ID, run.ID, 10*time.Second); err != nil || again.TimedOut || again.Result.OutputDir != wait.Result.OutputDir {
		t.Fatalf("second wait = %+v, %v", again, err)
	}
}

func TestExpiredArtifactsAreReportedAndCleanedAtStartupWithoutBlocking(t *testing.T) {
	f, fake := remoteActionFixture(t)
	f.writeRemoteAction(t, fake, "evidence")
	arch := f.architect(t, "alpha", "evidence")
	ctx := context.Background()
	run, session := f.remoteExecution(t, fake, "evidence", "alpha")
	if err := os.WriteFile(filepath.Join(run.OutputDir, "summary.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.DeliverActionArtifacts(context.Background(), session.ID, run.OutputDir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conclude(t, session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	local := f.result(t, arch.ID, run.ID)
	if local.Artifacts.State != domain.ActionArtifactsReady {
		t.Fatalf("delivery = %+v", local.Artifacts)
	}

	// An interrupted copy left by a previous daemon, and the retention period
	// ending for the package and its copy.
	past := time.Now().UTC().Add(-time.Hour)
	recs, _ := f.runs.ActionArtifacts(ctx, run.ID)
	for _, rec := range recs {
		rec.ExpiresAt = &past
		if err := f.runs.PutActionArtifact(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	interrupted := filepath.Join(fake.Home("other"), remoteActionsStateDir, "artifacts", "deliveries", run.ID)
	if err := os.MkdirAll(interrupted+".partial", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.PutActionArtifact(ctx, domain.ActionArtifactRecord{
		ExecutionID: run.ID, Kind: domain.ActionArtifactDelivery, Machine: "other", SSH: "other", Path: interrupted,
		ManagedRoot: filepath.Join(fake.Home("other"), remoteActionsStateDir, "artifacts"), State: domain.ActionArtifactsCopying, CreatedAt: past,
	}); err != nil {
		t.Fatal(err)
	}

	// Reads report expiry at once; the record and summary stay.
	expired := f.result(t, arch.ID, run.ID)
	if expired.Status != domain.ActionRunCompleted || expired.Summary != "ok" || expired.Artifacts.State != domain.ActionArtifactsExpired || expired.OutputDir != "" {
		t.Fatalf("expired result = %+v", expired)
	}

	// Startup cleanup with box offline: the local copy goes, box's package is
	// kept with a recorded failure, and the pass ends quickly.
	fake.SetDown(t, "box", true)
	started := time.Now()
	f.service.CleanupActionArtifacts(ctx)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("cleanup took %s with a machine offline", elapsed)
	}
	if _, err := os.Stat(local.OutputDir); !os.IsNotExist(err) {
		dbg, _ := f.runs.ActionArtifacts(ctx, run.ID)
		t.Fatalf("expired local copy remains: %v %+v", err, dbg)
	}
	if _, err := os.Stat(interrupted + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("interrupted partial copy remains: %v", err)
	}
	pkgPath := local.Artifacts.Source.Path
	if _, err := os.Stat(pkgPath); err != nil {
		t.Fatalf("package on the offline machine was not kept for retry: %v", err)
	}
	view, _ := f.service.GetActionRun(ctx, run.ID)
	if view.Package == nil || view.Package.State != domain.ActionArtifactsExpired || !strings.Contains(view.Package.CleanupError, "Connection refused") {
		t.Fatalf("package after failed cleanup = %+v", view.Package)
	}
	for _, d := range view.Deliveries {
		switch d.Machine {
		case "":
			if d.State != domain.ActionArtifactsExpired || d.CleanupError != "" {
				t.Fatalf("local delivery = %+v", d)
			}
		case "other":
			if d.State != domain.ActionArtifactsFailed || d.Error != interruptedCopyError {
				t.Fatalf("interrupted delivery = %+v", d)
			}
		}
	}

	// The next startup retries the failed removal. The agent's original
	// folder is never touched.
	fake.SetDown(t, "box", false)
	f.service.CleanupActionArtifacts(ctx)
	if _, err := os.Stat(pkgPath); !os.IsNotExist(err) {
		t.Fatalf("package remains after the retried cleanup: %v", err)
	}
	if got := readFile(t, filepath.Join(run.OutputDir, "summary.md")); got != "v1" {
		t.Fatalf("original folder changed: %q", got)
	}
	view, _ = f.service.GetActionRun(ctx, run.ID)
	if view.Status != domain.ActionRunCompleted || view.Package.State != domain.ActionArtifactsExpired || view.Package.CleanupError != "" {
		t.Fatalf("after retried cleanup = %+v (package %+v)", view, view.Package)
	}
}

// A remote Action launches like a remote worker — on the Action's machine,
// with its own output directory there and the Action role behind the worker
// gateway — and a launch that cannot prepare the agent fails the execution
// without leaving the Action busy.
func TestRemoteActionLaunchesOnItsMachine(t *testing.T) {
	f, fake := remoteActionFixture(t)
	f.service.remotes = map[string]*remoteWorker{}
	f.service.remoteStop = make(chan struct{})
	dir := f.writeRemoteAction(t, fake, "evidence")
	ctx := context.Background()

	// The remote account has the provider CLIs; first its tmux refuses to
	// create the agent's session.
	clis := t.TempDir()
	for name, script := range map[string]string{
		"codex": "#!/bin/sh\nexit 0\n",
		"node":  "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(clis, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "tmux"), []byte("#!/bin/sh\nfor a; do [ \"$a\" = new-session ] && { echo 'tmux: create failed' >&2; exit 1; }; done\necho 'no server running on /tmp/x' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", broken+string(os.PathListSeparator)+clis+string(os.PathListSeparator)+path)
	_, err := f.service.LaunchAction(ctx, "evidence", domain.LaunchActionRequest{Prompt: "collect", ProfileName: "box-codex"})
	if err == nil || !strings.Contains(err.Error(), "tmux: create failed") {
		t.Fatalf("launch with a failing tmux: %v", err)
	}
	runs, _ := f.service.ListActionRuns(ctx, "evidence", 0)
	if len(runs) != 1 || runs[0].Status != domain.ActionRunFailed || runs[0].Machine != "box" || !strings.Contains(runs[0].Error, "launch failed") {
		t.Fatalf("failed launch = %+v", runs)
	}
	wantOut := filepath.Join(fake.Home("box"), remoteActionsStateDir, "runs", "evidence", runs[0].ID)
	if runs[0].OutputDir != wantOut || runs[0].RepoPath != dir {
		t.Fatalf("paths = %s %s", runs[0].OutputDir, runs[0].RepoPath)
	}
	if info, err := os.Stat(wantOut); err != nil || !info.IsDir() {
		t.Fatalf("remote output directory: %v", err)
	}

	// With a working tmux the launch proceeds through the remote worker
	// path: the session lives on box under its alias.
	t.Setenv("PATH", clis+string(os.PathListSeparator)+path)
	result, err := f.service.LaunchAction(ctx, "evidence", domain.LaunchActionRequest{Prompt: "collect", ProfileName: "box-codex"})
	if err != nil {
		t.Fatal(err)
	}
	session := result.Session
	if session.Machine != "box" || session.SSH != "box" || len(session.RemoteToken) != 64 || session.RemotePort == 0 || session.Workdir != dir {
		t.Fatalf("session = %+v", session)
	}
	req := f.adapter.launchRequest
	if len(req.MCPServers) == 0 || !strings.HasSuffix(req.MCPServers[0].URL, "/mcp") || req.Workdir != dir ||
		len(req.AdditionalWorkdirs) != 1 || req.AdditionalWorkdirs[0] != result.Run.OutputDir {
		t.Fatalf("remote launch request = %+v", req)
	}
	if !strings.Contains(req.Prompt, "Machine: on machine box") {
		t.Fatalf("kickoff does not say where it runs: %q", req.Prompt)
	}
	workdirs, err := f.service.ListTerminalWorkdirs(ctx, session.ID)
	if err != nil || len(workdirs) != 2 || workdirs[0].Machine != "box" || workdirs[1].Machine != "box" {
		t.Fatalf("terminal workdirs = %+v, %v", workdirs, err)
	}

	// Stopping it confirms the remote side before the execution ends.
	fake.SetDown(t, "box", true)
	if _, err := f.service.CancelActionRun(ctx, result.Run.ID); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("cancel with box offline: %v", err)
	}
	if run, _ := f.service.GetActionRun(ctx, result.Run.ID); run.Status != domain.ActionRunRunning {
		t.Fatalf("unconfirmed stop ended the execution: %+v", run)
	}
	fake.SetDown(t, "box", false)
	if run, err := f.service.CancelActionRun(ctx, result.Run.ID); err != nil || run.Status != domain.ActionRunFailed {
		t.Fatalf("cancel = %+v, %v", run, err)
	}
}
