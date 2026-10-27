package contextref

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

// State is the result of inspecting one target where it lives. Exactly one
// of Kind (present), Missing (confirmed absent), Outside (escapes its root)
// or Err (could not be inspected; existence unknown) describes it.
type State struct {
	Kind    domain.PathKind
	Missing bool
	Outside bool
	Err     error
}

func (s State) Equal(o State) bool {
	if s.Kind != o.Kind || s.Missing != o.Missing || s.Outside != o.Outside || (s.Err == nil) != (o.Err == nil) {
		return false
	}
	return s.Err == nil || s.Err.Error() == o.Err.Error()
}

// canonicalLocal resolves p with symlinks evaluated and checks that it stays
// within root (itself canonicalized). A missing p is os.ErrNotExist.
func canonicalLocal(root, p string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %s: %w", root, err)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !within(realRoot, real, string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	return real, nil
}

// InspectLocal inspects target below a local root.
func InspectLocal(root Root, target string) State {
	real, err := canonicalLocal(root.Path, root.Join(target))
	switch {
	case errors.Is(err, ErrOutsideRoot):
		return State{Outside: true}
	case err != nil && (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)):
		return State{Missing: true}
	case err != nil:
		return State{Err: err}
	}
	info, err := os.Stat(real)
	switch {
	case err != nil:
		return State{Err: err}
	case info.IsDir():
		return State{Kind: domain.PathKindDirectory}
	default:
		return State{Kind: domain.PathKindFile}
	}
}

// remoteShellPrelude defines rp (canonical path with symlinks resolved; fails
// when the path does not exist) and ancestor_state (m when the nearest
// existing ancestor of $1 is searchable — confirmed missing — u otherwise).
const remoteShellPrelude = `if command -v realpath >/dev/null 2>&1; then rp() { realpath "$1" 2>/dev/null; }; else rp() { readlink -f "$1" 2>/dev/null; }; fi
inside() { case "$2" in "$1"|"$1"/*) return 0;; esac; [ "$1" = / ]; }
ancestor_state() {
  q=$1
  while [ -n "$q" ] && [ ! -e "$q" ] && [ ! -L "$q" ]; do q=${q%/*}; done
  if [ -n "$q" ] && [ -d "$q" ] && [ ! -x "$q" ]; then echo u; else echo m; fi
}
`

// InspectRemote inspects targets on one SSH alias in a single invocation.
// Each item is root and the absolute joined path; answers come back one line
// each in order: d(irectory), f(ile), m(issing), o(utside its root) or u
// (permission prevents knowing).
func InspectRemote(ctx context.Context, alias string, items [][2]string) ([]State, error) {
	if len(items) == 0 {
		return nil, nil
	}
	const script = remoteShellPrelude + `tab=$(printf '\t')
while IFS=$tab read -r root p; do
  r=$(rp "$root") || r=
  if [ ! -e "$p" ]; then
    if [ -L "$p" ]; then echo m; else ancestor_state "$p"; fi
    continue
  fi
  c=$(rp "$p") || { echo u; continue; }
  if [ -z "$r" ] || ! inside "$r" "$c"; then echo o; continue; fi
  if [ -d "$c" ]; then echo d; else echo f; fi
done`
	var input strings.Builder
	for _, item := range items {
		if strings.ContainsAny(item[0]+item[1], "\t\n\x00") {
			return nil, fmt.Errorf("path %s cannot be sent to the SSH inspection", item[1])
		}
		input.WriteString(item[0] + "\t" + item[1] + "\n")
	}
	out, err := remoteexec.Run(ctx, alias, script, strings.NewReader(input.String()))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(items) {
		return nil, fmt.Errorf("SSH %s: path inspection returned %d answers for %d paths", alias, len(lines), len(items))
	}
	states := make([]State, len(items))
	for i, line := range lines {
		switch line {
		case "d":
			states[i].Kind = domain.PathKindDirectory
		case "f":
			states[i].Kind = domain.PathKindFile
		case "m":
			states[i].Missing = true
		case "o":
			states[i].Outside = true
		case "u":
			states[i].Err = errors.New("permission denied searching an ancestor directory")
		default:
			return nil, fmt.Errorf("SSH %s: unexpected path inspection answer %q", alias, line)
		}
	}
	return states, nil
}
