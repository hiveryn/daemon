package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/adapter/claude"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

// Latencies are ka's measured costs scaled by 1/40: a fresh SSH connection
// 7.5 s, a command over an established master 1.3 s, the launch bound 2 min.
const (
	scaledFresh  = 7500 * time.Millisecond / 40
	scaledShared = 1300 * time.Millisecond / 40
	scaledBudget = 2 * time.Minute / 40
)

type remoteLaunchFixture struct {
	*spawnFixture
	ssh   *sshtest.Fake
	home  string
	stubs string
	paths []string
}

// newRemoteLaunchFixture configures machine ka with a ticket spanning repos
// remote repositories (one with a space, quotes and shell metacharacters in its
// path), a Claude variant for ka and stub tmux/claude/node on the "remote".
func newRemoteLaunchFixture(t *testing.T, repos int) *remoteLaunchFixture {
	t.Helper()
	f := &remoteLaunchFixture{spawnFixture: newSpawnFixture(t), ssh: sshtest.Install(t)}
	f.home = f.ssh.Home("ka")
	f.stubs = t.TempDir()
	for _, name := range []string{"claude", "node"} {
		if err := os.WriteFile(filepath.Join(f.stubs, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only the fake ssh/tmux, the stubs and the system tools: a provider CLI
	// installed on the machine running the tests must not satisfy the check.
	fakeBin := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	t.Setenv("PATH", strings.Join([]string{f.stubs, fakeBin, "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	// Control sockets go in a short directory of the test's own.
	tmp, err := os.MkdirTemp("/tmp", "hvl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)

	architect := config.ArchitectConfig{Name: "hiveryn", Repos: map[string]string{}, RepoMachines: map[string]string{}}
	var yaml strings.Builder
	yaml.WriteString("name: Hiveryn\nrepos:\n")
	for i := 0; i < repos; i++ {
		key := fmt.Sprintf("r%02d", i)
		name := key
		if i == 1 {
			name = `odd dir 'quoted' "$(x)";` + key
		}
		p := filepath.Join(f.home, "repos", name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		architect.Repos[key], architect.RepoMachines[key] = p, "ka"
		fmt.Fprintf(&yaml, "  %s:\n    path: %q\n    machine: ka\n", key, p)
		f.paths = append(f.paths, p)
	}
	architect.Path = testWorkspace(t, nil)
	writeWorkspaceFile(t, architect.Path, workspacefs.ConfigFileName, yaml.String())
	f.workspace = architect.Path
	f.service.cfg.Architects["hiveryn"] = architect
	f.service.cfg.Machines = map[string]config.MachineConfig{"ka": {SSH: "ka"}}
	f.service.cfg.Variants["ka-claude"] = config.VariantConfig{Agent: "claude", Machine: "ka"}
	f.service.adapters[agentruntime.AgentClaude] = claude.New(claude.DefaultOptions())
	f.service.remotes = map[string]*remoteWorker{}
	return f
}

func (f *remoteLaunchFixture) session(t *testing.T, title string) domain.Session {
	t.Helper()
	ctx := context.Background()
	architect := f.service.cfg.Architects["hiveryn"]
	var keys []string
	for i := range f.paths {
		keys = append(keys, fmt.Sprintf("r%02d", i))
	}
	ticket, err := f.tickets.CreateTicket(ctx, f.workspace, domain.CreateTicketParams{Title: title, Repo: keys[0], AdditionalRepos: keys[1:], Body: "Do it.", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	params, err := f.service.ticketSessionParams(ctx, "hiveryn", architect, ticket.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.sessions.CreateSession(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if session.Machine != "ka" || len(session.AdditionalWorkdirs) != len(f.paths)-1 {
		t.Fatalf("remote session scope: machine=%q workdirs=%d", session.Machine, len(session.AdditionalWorkdirs))
	}
	return session
}

func (f *remoteLaunchFixture) launch(t *testing.T, session domain.Session) (domain.CreateSessionRunResult, time.Duration, error) {
	t.Helper()
	started := time.Now()
	result, err := f.service.CreateRun(context.Background(), session.ID, domain.CreateSessionRunRequest{ProfileName: "ka-claude"})
	return result, time.Since(started), err
}

func (f *remoteLaunchFixture) count(substr string) int {
	n := 0
	for _, script := range f.ssh.Commands("ka") {
		if strings.Contains(script, substr) {
			n++
		}
	}
	return n
}

// The incident: an 11-repository Claude worker on a host where each fresh SSH
// connection costs ~7.5 s. Preparation ran ~24 sequential connections and
// exhausted the 2-minute bound. With the launch's shared connection and
// batched probes it prepares in one handshake and a fixed number of commands,
// at the unchanged bound.
func TestElevenRepoRemoteLaunchFitsItsBudget(t *testing.T) {
	f := newRemoteLaunchFixture(t, 11)
	f.ssh.SetLatency(t, scaledFresh, scaledShared)
	f.service.launchTimeout = scaledBudget
	session := f.session(t, "Eleven")

	result, elapsed, err := f.launch(t, session)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	fresh, masters, shared := f.ssh.Connections("ka")
	t.Logf("11 repos: %s of %s budget; fresh=%d masters=%d shared=%d", elapsed.Round(time.Millisecond), scaledBudget, fresh, masters, shared)
	if fresh != 0 || masters != 1 {
		t.Fatalf("handshakes: fresh=%d masters=%d, want one master only", fresh, masters)
	}
	// 9 preparation commands plus the per-launch waiter settings write that
	// lets conversation input reach a remote Claude worker.
	if shared > 10 {
		t.Fatalf("%d commands over the shared connection, want at most 10", shared)
	}
	if elapsed > scaledBudget/2 {
		t.Fatalf("launch took %s, more than half its %s budget", elapsed, scaledBudget)
	}
	if result.MainTerminalID == "" || f.count("new-session") != 1 {
		t.Fatalf("result=%+v tmux creations=%d", result, f.count("new-session"))
	}
	if _, err := os.Stat(filepath.Join(f.home, ".claude", "settings.json")); err != nil {
		t.Fatalf("claude setup not written: %v", err)
	}
	if entries, _ := os.ReadDir(os.Getenv("TMPDIR")); len(entries) != 0 {
		t.Fatalf("launch left its control directory: %v", entries)
	}
	if f.ticketStatus(t, session.ContextID) != domain.TicketStatusProgress {
		t.Fatal("ticket not moved to progress")
	}

	// A second worker on the same machine: setup is unchanged (no settings
	// write), and its command count does not grow with the repository count.
	before := f.count("mktemp \"${p}.")
	other := f.session(t, "Second")
	_, elapsed, err = f.launch(t, other)
	if err != nil {
		t.Fatalf("second launch: %v", err)
	}
	if writes := f.count("mktemp \"${p}.") - before; writes != 0 {
		t.Fatalf("unchanged setup rewrote settings %d times", writes)
	}
	// Unchanged setup is one read: no directory creation, no write.
	fresh, masters2, shared2 := f.ssh.Connections("ka")
	t.Logf("unchanged setup: %s; fresh=%d masters=%d shared=%d", elapsed.Round(time.Millisecond), fresh, masters2-masters, shared2-shared)
	if fresh != 0 || masters2-masters != 1 || shared2-shared != shared-2 {
		t.Fatalf("second launch: fresh=%d masters=%d shared=%d, want 1 master and %d commands", fresh, masters2-masters, shared2-shared, shared-2)
	}
}

// The command count is fixed: one repository and eleven cost the same.
func TestRemoteLaunchCommandsDoNotScaleWithRepositories(t *testing.T) {
	counts := map[int]int{}
	for _, repos := range []int{1, 11} {
		f := newRemoteLaunchFixture(t, repos)
		if _, _, err := f.launch(t, f.session(t, "Scale")); err != nil {
			t.Fatalf("%d repos: %v", repos, err)
		}
		fresh, masters, shared := f.ssh.Connections("ka")
		if fresh != 0 || masters != 1 {
			t.Fatalf("%d repos: fresh=%d masters=%d", repos, fresh, masters)
		}
		counts[repos] = shared
	}
	if counts[1] != counts[11] {
		t.Fatalf("commands by repository count: %v", counts)
	}
}

// An SSH configuration that cannot multiplex still launches, over direct
// connections, and batching alone keeps the connection count fixed.
func TestRemoteLaunchWithoutSharedConnectionStillBatches(t *testing.T) {
	f := newRemoteLaunchFixture(t, 11)
	f.ssh.RefuseMasters(t)
	f.ssh.SetLatency(t, scaledFresh, scaledShared)
	_, elapsed, err := f.launch(t, f.session(t, "Direct"))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	fresh, masters, shared := f.ssh.Connections("ka")
	t.Logf("11 repos without multiplexing: %s; fresh=%d masters=%d shared=%d", elapsed.Round(time.Millisecond), fresh, masters, shared)
	if masters != 1 || shared != 0 || fresh > 10 {
		t.Fatalf("fresh=%d masters=%d shared=%d", fresh, masters, shared)
	}
}

func TestRemoteLaunchNamesEveryMissingRepository(t *testing.T) {
	f := newRemoteLaunchFixture(t, 11)
	session := f.session(t, "Missing")
	if err := os.RemoveAll(f.paths[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.paths[7]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.paths[7], nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.launch(t, session)
	var validation *domain.ValidationError
	if !errors.As(err, &validation) || !strings.Contains(err.Error(), "path does not exist: "+f.paths[1]) || !strings.Contains(err.Error(), "path is not a directory: "+f.paths[7]) || strings.Contains(err.Error(), f.paths[2]) {
		t.Fatalf("missing repositories: %v", err)
	}
	if got := f.count("test -d"); got != 1 {
		t.Fatalf("repository checks ran in %d commands", got)
	}
	if f.count("new-session") != 0 || f.ticketStatus(t, session.ContextID) != domain.TicketStatusBacklog {
		t.Fatal("a launch with a missing repository prepared a worker")
	}
}

// An unreachable host is a failed SSH command, not a missing repository.
func TestRemoteLaunchReportsUnreachableHostAsSSHFailure(t *testing.T) {
	f := newRemoteLaunchFixture(t, 2)
	session := f.session(t, "Down")
	f.ssh.SetDown(t, "ka", true)
	_, _, err := f.launch(t, session)
	if err == nil || errors.As(err, new(*domain.ValidationError)) || !strings.Contains(err.Error(), "check remote repositories on ka") || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestRemoteLaunchChangedMachineRunsNoSSH(t *testing.T) {
	f := newRemoteLaunchFixture(t, 2)
	session := f.session(t, "Moved")
	f.service.cfg.Machines["ka"] = config.MachineConfig{SSH: "ka-new"}
	_, _, err := f.launch(t, session)
	if err == nil || !strings.Contains(err.Error(), "changed since session creation") {
		t.Fatalf("changed machine: %v", err)
	}
	if fresh, masters, shared := f.ssh.Connections("ka"); fresh+masters+shared != 0 {
		t.Fatalf("SSH ran for a stale location: %d %d %d", fresh, masters, shared)
	}
}

// A missing prerequisite fails after the run exists: the never-started worker
// is cleaned up, and once the host is fixed the same session launches exactly
// one worker.
func TestRemoteLaunchMissingPrerequisiteCleansUpAndRetries(t *testing.T) {
	f := newRemoteLaunchFixture(t, 3)
	session := f.session(t, "Prereq")
	if err := os.Remove(filepath.Join(f.stubs, "claude")); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.launch(t, session)
	if err == nil || !strings.Contains(err.Error(), "remote prerequisite claude is unavailable on ka") {
		t.Fatalf("missing prerequisite: %v", err)
	}
	// The probe fails before a runtime directory is recorded or created.
	if f.count("new-session") != 0 || f.count("kill-server") != 1 || f.count("rmdir") != 0 {
		t.Fatalf("creations=%d kills=%d removals=%d", f.count("new-session"), f.count("kill-server"), f.count("rmdir"))
	}
	if err := os.WriteFile(filepath.Join(f.stubs, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.launch(t, session); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.count("new-session") != 1 {
		t.Fatalf("tmux creations after retry: %d", f.count("new-session"))
	}
}

// A setup write that fails leaves the existing settings untouched, cleans up
// and can be retried.
func TestRemoteLaunchFailedSetupWriteKeepsSettingsAndRetries(t *testing.T) {
	f := newRemoteLaunchFixture(t, 2)
	session := f.session(t, "Setup")
	dir := filepath.Join(f.home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"theme": "dark"}`)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, _, err := f.launch(t, session)
	if err == nil || !strings.Contains(err.Error(), "remote claude setup") {
		t.Fatalf("failed setup: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "settings.json")); string(got) != string(original) {
		t.Fatalf("settings changed by a failed write: %s", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("failed write left files: %v", entries)
	}
	if f.count("new-session") != 0 || f.count("kill-server") != 1 {
		t.Fatalf("creations=%d kills=%d", f.count("new-session"), f.count("kill-server"))
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.launch(t, session); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(got), `"theme": "dark"`) || !strings.Contains(string(got), setupMarker) {
		t.Fatalf("retried setup lost or skipped settings: %s", got)
	}
}

// A launch whose bound runs out says it was the budget, and in which stage,
// and still removes what it prepared over its shared connection.
func TestRemoteLaunchBudgetExhaustionNamesStageAndCleansUp(t *testing.T) {
	f := newRemoteLaunchFixture(t, 2)
	f.ssh.SetLatency(t, 10*time.Millisecond, 150*time.Millisecond)
	f.service.launchTimeout = 500 * time.Millisecond
	session := f.session(t, "Budget")
	_, _, err := f.launch(t, session)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "launch did not complete within 500ms: ") || !strings.Contains(err.Error(), "launch budget ran out during remote stage") {
		t.Fatalf("exhausted budget: %v", err)
	}
	if strings.Contains(err.Error(), "remote stage \"cleanup\"") {
		t.Fatalf("stage should be where the budget ran out: %v", err)
	}
	if f.count("kill-server") != 1 || f.count("new-session") != 0 {
		t.Fatalf("cleanup: kills=%d creations=%d", f.count("kill-server"), f.count("new-session"))
	}
	if entries, _ := os.ReadDir(os.Getenv("TMPDIR")); len(entries) != 0 {
		t.Fatalf("control directory left: %v", entries)
	}
}
