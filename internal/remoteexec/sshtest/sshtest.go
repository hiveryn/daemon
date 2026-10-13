// Package sshtest installs a fake `ssh` for tests that runs each remote
// script locally, as the SSH machine named by its alias: in a home directory
// of its own (HOME=<root>/<alias>), with the caller's stdin and stdout. It
// never contacts a host, and the tmux it provides has no servers. A machine can be taken offline, which makes every
// command to it fail the way an unreachable host does.
package sshtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type Fake struct{ root string }

// Install puts the fake ssh first on PATH for the rest of the test.
func Install(t *testing.T) *Fake {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, ".bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The alias follows the "--" that ends the options; the remote command is
	// the final argument, which ssh hands to the remote shell.
	script := `#!/bin/sh
root=` + shellQuote(root) + `
alias=
while [ $# -gt 1 ]; do
  if [ "$1" = "--" ]; then alias=$2; fi
  shift
done
printf '%s\t%s\036' "$alias" "$1" >> "$root/.log"
if [ -e "$root/.down-$alias" ]; then
  echo "ssh: connect to host $alias port 22: Connection refused" >&2
  exit 255
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

// Commands returns the scripts run on alias so far, in order.
func (f *Fake) Commands(alias string) []string {
	data, _ := os.ReadFile(filepath.Join(f.root, ".log"))
	var out []string
	for _, record := range strings.Split(string(data), "\x1e") {
		if a, script, ok := strings.Cut(record, "\t"); ok && a == alias {
			out = append(out, script)
		}
	}
	return out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
