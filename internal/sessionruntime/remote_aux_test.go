package sessionruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// remoteResourceRepository adds in-memory remote ownership records to the
// fake repository, the way the SQLite store keeps them.
type remoteResourceRepository struct {
	*fakeSessionRepository
	mu        sync.Mutex
	resources map[string]bool // ssh + "/" + socket
}

func (r *remoteResourceRepository) AddRemoteResource(_ context.Context, _, ssh, socket, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources[ssh+"/"+socket] = true
	return nil
}

func (r *remoteResourceRepository) RemoveRemoteResource(_ context.Context, _, ssh, socket string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.resources, ssh+"/"+socket)
	return nil
}

func (r *remoteResourceRepository) RemoteResources(context.Context, string) (map[string][]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]string{}
	for key := range r.resources {
		ssh, socket, _ := strings.Cut(key, "/")
		out[ssh] = append(out[ssh], socket)
	}
	return out, nil
}

func (r *remoteResourceRepository) RemoteDirectories(context.Context, string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

func (r *remoteResourceRepository) owned() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.resources)
}

// fakeSSH puts an `ssh` on PATH that logs each remote script and behaves per
// the files in its directory: create-delay (seconds), create-fail and
// kill-fail. It never contacts a host.
type fakeSSH struct{ dir string }

func installFakeSSH(t *testing.T) fakeSSH {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
for last; do :; done
printf '%s\n' "$last" >> "` + dir + `/log"
case "$last" in
*new-session*)
  if [ -f "` + dir + `/create-delay" ]; then sleep "$(cat "` + dir + `/create-delay")"; fi
  if [ -f "` + dir + `/create-fail" ]; then echo "tmux: create failed on fixture" >&2; exit 1; fi ;;
*kill-server*)
  if [ -f "` + dir + `/kill-fail" ]; then echo "ssh: connect to host box: Connection refused" >&2; exit 255; fi ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fakeSSH{dir: dir}
}

func (f fakeSSH) set(t *testing.T, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f fakeSSH) count(substr string) int {
	data, _ := os.ReadFile(filepath.Join(f.dir, "log"))
	return strings.Count(string(data), substr)
}

func newRemoteAuxService(t *testing.T, terminal terminalManager) (*Service, *remoteResourceRepository) {
	t.Helper()
	repo := &remoteResourceRepository{fakeSessionRepository: newFakeSessionRepository(), resources: map[string]bool{}}
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "hiveryn",
		Workdir:      t.TempDir(),
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}
	return &Service{
		intents: newIntentStore(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.Config{
			Shell:    "/bin/sh",
			Machines: map[string]config.MachineConfig{"box": {SSH: "box"}},
			Architects: map[string]config.ArchitectConfig{"hiveryn": {
				Path:         t.TempDir(),
				Repos:        map[string]string{"remote": "/srv/remote"},
				RepoMachines: map[string]string{"remote": "box"},
			}},
		},
		repo:           repo,
		terminal:       terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}, repo
}

var remoteTerminal = domain.CreateTerminalParams{WorkdirID: "repo:remote"}

// A slow remote creation completes even when the requester stops waiting, and
// a retry while it is in flight is refused instead of opening a second shell.
func TestRemoteTerminalCreationOutlivesRequesterAndRejectsRetry(t *testing.T) {
	ssh := installFakeSSH(t)
	ssh.set(t, "create-delay", "0.5")
	terminal := &fakeTerminalManager{}
	service, repo := newRemoteAuxService(t, terminal)

	requester, stopWaiting := context.WithCancel(context.Background())
	type outcome struct {
		info domain.TerminalInfo
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		info, err := service.CreateTerminal(requester, "session-1", remoteTerminal)
		done <- outcome{info, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for ssh.count("new-session") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stopWaiting()

	_, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal)
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "still being opened") {
		t.Fatalf("retry during creation: %v", err)
	}

	got := <-done
	if got.err != nil || got.info.Command != "ssh" {
		t.Fatalf("creation after requester left: %+v", got)
	}
	if tabs := service.sessionTabs("session-1"); len(tabs) != 1 || tabs[0].ID != got.info.TerminalID {
		t.Fatalf("created terminal is not a tab: %#v", tabs)
	}
	if repo.owned() != 1 || ssh.count("kill-server") != 0 || ssh.count("new-session") != 1 {
		t.Fatalf("owned=%d kills=%d creates=%d", repo.owned(), ssh.count("kill-server"), ssh.count("new-session"))
	}

	// The guard is released, so the next request is a fresh terminal.
	if _, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal); err != nil {
		t.Fatalf("create after completion: %v", err)
	}
}

func TestFailedRemoteTerminalCreationIsRemovedWithItsOwnership(t *testing.T) {
	ssh := installFakeSSH(t)
	ssh.set(t, "create-fail", "")
	service, repo := newRemoteAuxService(t, &fakeTerminalManager{})

	_, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal)
	if err == nil || !strings.Contains(err.Error(), "create remote repository terminal on machine box") || !strings.Contains(err.Error(), "tmux: create failed on fixture") {
		t.Fatalf("create failure: %v", err)
	}
	if ssh.count("kill-server") != 1 || repo.owned() != 0 || len(service.sessionTabs("session-1")) != 0 {
		t.Fatalf("kills=%d owned=%d tabs=%#v", ssh.count("kill-server"), repo.owned(), service.sessionTabs("session-1"))
	}
}

func TestUnconfirmedCleanupKeepsRemoteTerminalOwnership(t *testing.T) {
	ssh := installFakeSSH(t)
	ssh.set(t, "create-fail", "")
	ssh.set(t, "kill-fail", "")
	service, repo := newRemoteAuxService(t, &fakeTerminalManager{})

	_, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal)
	if err == nil || !strings.Contains(err.Error(), "tmux: create failed on fixture") || !strings.Contains(err.Error(), "was not confirmed") || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("unconfirmed cleanup: %v", err)
	}
	if repo.owned() != 1 {
		t.Fatalf("ownership dropped without confirmed removal: %d", repo.owned())
	}
}

// A creation that outlives its bound is killed by its own deadline, and the
// possibly created server is still removed: a timeout is not proof that no
// remote shell exists. The fake's sleep outlives the killed ssh process and
// holds its output open, as a ProxyCommand child can; the bound still holds.
func TestTimedOutRemoteTerminalCreationIsCleanedUp(t *testing.T) {
	ssh := installFakeSSH(t)
	ssh.set(t, "create-delay", "5")
	service, repo := newRemoteAuxService(t, &fakeTerminalManager{})
	service.terminalCreateTimeout = 200 * time.Millisecond

	started := time.Now()
	_, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "terminal creation did not complete within 200ms: ") {
		t.Fatalf("deadline error: %v", err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("creation was not bounded: %s", time.Since(started))
	}
	if ssh.count("kill-server") != 1 || repo.owned() != 0 {
		t.Fatalf("kills=%d owned=%d", ssh.count("kill-server"), repo.owned())
	}
}

func TestRemoteTerminalWithoutAttachmentIsRemoved(t *testing.T) {
	ssh := installFakeSSH(t)
	service, repo := newRemoteAuxService(t, &fakeTerminalManager{startErr: errors.New("pty start failed")})

	_, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal)
	if err == nil || err.Error() != "pty start failed" {
		t.Fatalf("attach failure: %v", err)
	}
	if ssh.count("new-session") != 1 || ssh.count("kill-server") != 1 || repo.owned() != 0 {
		t.Fatalf("creates=%d kills=%d owned=%d", ssh.count("new-session"), ssh.count("kill-server"), repo.owned())
	}
}

func TestClosingRemoteTerminalDropsItsOwnership(t *testing.T) {
	ssh := installFakeSSH(t)
	service, repo := newRemoteAuxService(t, &fakeTerminalManager{})
	info, err := service.CreateTerminal(context.Background(), "session-1", remoteTerminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.stopRemoteAux(context.Background(), "session-1", info.TerminalID); err != nil {
		t.Fatal(err)
	}
	if ssh.count("kill-server") != 1 || repo.owned() != 0 {
		t.Fatalf("kills=%d owned=%d", ssh.count("kill-server"), repo.owned())
	}
}
