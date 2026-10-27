// Package sshtest installs a fake `ssh` for tests that runs each remote
// script locally, as the SSH machine named by its alias: in a home directory
// of its own (HOME=<root>/<alias>), with the caller's stdin and stdout. It
// never contacts a host, and the tmux it provides has no servers. A machine can be taken offline, which makes every
// command to it fail the way an unreachable host does.
package sshtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type Fake struct{ root string }

// warmupAlias is the machine Install's own warm-up command runs on.
const warmupAlias = ".warmup"

// Install puts the fake ssh first on PATH for the rest of the test.
func Install(t *testing.T) *Fake {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, ".bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The alias follows the "--" that ends the options; the remote command is
	// the final argument, which ssh hands to the remote shell. A master (-N
	// with ControlMaster=yes) creates its control path and waits to be killed;
	// a command with ControlMaster=no whose control path exists is "shared".
	// Fresh connections, masters included, sleep .latency-fresh seconds and
	// shared ones .latency-shared, so tests can model handshake cost.
	script := `#!/bin/sh
root=` + shellQuote(root) + `
alias= master= mux= control=
while [ $# -gt 1 ]; do
  case "$1" in
  --) alias=$2 ;;
  -N) master=1 ;;
  ControlMaster=no) mux=1 ;;
  ControlPath=*) control=${1#ControlPath=} ;;
  esac
  shift
done
mode=fresh
if [ -n "$master" ]; then mode=master; elif [ -n "$mux" ] && [ -e "$control" ]; then mode=shared; fi
printf '%s\t%s\t%s\036' "$alias" "$mode" "$1" >> "$root/.log"
latency=.latency-fresh
if [ "$mode" = shared ]; then latency=.latency-shared; fi
if [ -f "$root/$latency" ]; then
  # Exit on TERM as ssh does, rather than finishing the modelled delay.
  sleep "$(cat "$root/$latency")" & pid=$!
  trap 'kill $pid 2>/dev/null; exit 143' TERM
  wait $pid
  trap - TERM
fi
if [ -e "$root/.down-$alias" ]; then
  echo "ssh: connect to host $alias port 22: Connection refused" >&2
  exit 255
fi
if [ -n "$master" ]; then
  if [ -e "$root/.no-master" ]; then echo "mux_listener_setup: fixture refuses masters" >&2; exit 255; fi
  : > "$control"
  exec sleep 3600
fi
mkdir -p "$root/$alias"
HOME="$root/$alias"
export HOME
cd "$HOME" || exit 255
exec /bin/sh -c "$1"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// A tmux with no servers: Hiveryn's owned sockets are always absent, so
	// confirmed termination succeeds without touching a real tmux.
	tmux := `#!/bin/sh
for arg; do
  case "$arg" in
  kill-server|has-session|display-message) echo "no server running on /tmp/tmux-fake/default" >&2; exit 1 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(tmux), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Run each fake once: a new executable's first launch can be scanned by
	// the OS for hundreds of milliseconds, which would distort timed tests.
	for _, name := range []string{"ssh", "tmux"} {
		_ = exec.Command(filepath.Join(bin, name), "--", warmupAlias, "true").Run()
	}
	return &Fake{root: root}
}

// Home is the alias's home directory: its "remote" filesystem root.
func (f *Fake) Home(alias string) string {
	home := filepath.Join(f.root, alias)
	_ = os.MkdirAll(home, 0o755)
	return home
}

// SetDown takes the alias's machine offline (true) or back online.
func (f *Fake) SetDown(t *testing.T, alias string, down bool) {
	t.Helper()
	marker := filepath.Join(f.root, ".down-"+alias)
	if down {
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// Commands returns the scripts run on alias so far, in order; masters are
// connections, not commands, and are not included.
func (f *Fake) Commands(alias string) []string {
	var out []string
	for _, r := range f.records(alias) {
		if r.mode != "master" {
			out = append(out, r.script)
		}
	}
	return out
}

// Connections counts alias's invocations by mode: fresh commands and masters
// each paid a handshake; shared commands ran over an existing master.
func (f *Fake) Connections(alias string) (fresh, masters, shared int) {
	for _, r := range f.records(alias) {
		switch r.mode {
		case "fresh":
			fresh++
		case "master":
			masters++
		case "shared":
			shared++
		}
	}
	return fresh, masters, shared
}

// SetLatency makes every fresh connection (masters included) take fresh and
// every command over a master take shared, modelling a slow handshake.
func (f *Fake) SetLatency(t *testing.T, fresh, shared time.Duration) {
	t.Helper()
	for name, d := range map[string]time.Duration{".latency-fresh": fresh, ".latency-shared": shared} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(fmt.Sprintf("%.3f", d.Seconds())), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// RefuseMasters makes every master fail to start, as an SSH configuration
// that cannot multiplex would; plain commands still work.
func (f *Fake) RefuseMasters(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, ".no-master"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

type record struct{ mode, script string }

func (f *Fake) records(alias string) []record {
	data, _ := os.ReadFile(filepath.Join(f.root, ".log"))
	var out []record
	for _, line := range strings.Split(string(data), "\x1e") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) == 3 && fields[0] == alias {
			out = append(out, record{mode: fields[1], script: fields[2]})
		}
	}
	return out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
