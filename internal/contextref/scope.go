// Package contextref resolves context references (domain.ParseReference)
// against an architect's configuration and inspects or reads their targets
// where they live: the architect workspace on this machine, each repo on its
// own configured machine. It never falls back to another machine and never
// reads outside a root, including through symlinks.
package contextref

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Root is one readable root: the architect workspace or a configured repo.
type Root struct {
	// Owner is domain.ArchitectReferencePrefix or the repo key.
	Owner string
	// Path is absolute on the root's machine.
	Path string
	// Machine is empty for this machine; SSH is its alias otherwise.
	Machine string
	SSH     string
	// Err is set when the root cannot be located (for example a repo whose
	// machine is not configured); references to it are unverified.
	Err error
}

// Location reports where the root lives.
func (r Root) Location() domain.ReferenceLocation {
	switch {
	case r.Err != nil:
		return domain.ReferenceUnknown
	case r.Machine == "":
		return domain.ReferenceLocal
	default:
		return domain.ReferenceRemote
	}
}

// Where names the root's machine for messages.
func (r Root) Where() string {
	if r.Machine == "" {
		return "this machine (local)"
	}
	return fmt.Sprintf("machine %q (SSH %s)", r.Machine, r.SSH)
}

// Join returns the absolute, not yet canonicalized, path of target.
func (r Root) Join(target string) string {
	if target == domain.ReferenceRootTarget {
		return r.Path
	}
	if r.Machine == "" {
		return filepath.Join(r.Path, filepath.FromSlash(target))
	}
	return strings.TrimRight(r.Path, "/") + "/" + target
}

// Scope is everything one architect's sessions may read as context: its
// workspace and its configured repos.
type Scope struct {
	ArchitectKey string
	Architect    Root
	Repos        map[string]Root
}

// ScopeFor builds the scope of the architect registered under key.
func ScopeFor(cfg config.Config, key string) (Scope, error) {
	architect, ok := cfg.Architects[key]
	if !ok {
		return Scope{}, &domain.NotFoundError{Resource: "architect", ID: key}
	}
	return scopeOf(cfg, key, architect), nil
}

// ScopeAt builds the scope of the architect whose workspace is at path.
func ScopeAt(cfg config.Config, path string) (Scope, error) {
	want := filepath.Clean(path)
	for key, architect := range cfg.Architects {
		if filepath.Clean(architect.Path) == want {
			return scopeOf(cfg, key, architect), nil
		}
	}
	return Scope{}, fmt.Errorf("no architect is registered at %s", path)
}

func scopeOf(cfg config.Config, key string, architect config.ArchitectConfig) Scope {
	scope := Scope{
		ArchitectKey: key,
		Architect:    Root{Owner: domain.ArchitectReferencePrefix, Path: filepath.Clean(architect.Path)},
		Repos:        make(map[string]Root, len(architect.Repos)),
	}
	for repoKey, path := range architect.Repos {
		root := Root{Owner: repoKey, Path: path}
		if machine := architect.RepoMachines[repoKey]; machine != "" {
			root.Machine = machine
			m, ok := cfg.Machines[machine]
			if !ok || m.SSH == "" {
				root.Err = fmt.Errorf("repo %q is on machine %q, which machines.yaml does not define", repoKey, machine)
			}
			root.SSH = m.SSH
		} else {
			root.Path = filepath.Clean(path)
		}
		if strings.IndexFunc(root.Path, unicode.IsControl) >= 0 {
			root.Err = fmt.Errorf("repo %q path contains a control character and cannot be inspected", repoKey)
		}
		scope.Repos[repoKey] = root
	}
	return scope
}

// RepoKeys lists the configured repo keys in order.
func (s Scope) RepoKeys() []string {
	keys := make([]string, 0, len(s.Repos))
	for key := range s.Repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// UnknownOwnerError is a well-formed reference whose prefix is neither
// architect nor a repo key of this architect.
type UnknownOwnerError struct {
	Reference string
	Owner     string
	Known     []string
}

func (e *UnknownOwnerError) Error() string {
	known := "none are configured"
	if len(e.Known) > 0 {
		known = "configured: " + strings.Join(e.Known, ", ")
	}
	return fmt.Sprintf("reference %q: %q is not architect or a repo key in this architect's hiveryn.yaml (%s)", e.Reference, e.Owner, known)
}

// Resolve returns the root a parsed architect or repo reference addresses.
func (s Scope) Resolve(ref domain.ParsedReference) (Root, error) {
	switch ref.Kind {
	case domain.ReferenceKindArchitect:
		return s.Architect, nil
	case domain.ReferenceKindRepo:
		if root, ok := s.Repos[ref.Owner]; ok {
			return root, nil
		}
		return Root{}, &UnknownOwnerError{Reference: ref.String(), Owner: ref.Owner, Known: s.RepoKeys()}
	default:
		return Root{}, fmt.Errorf("reference %q is a ticket, not a file or directory; read it with readTicket", ref.String())
	}
}

// ParseContext parses a reference that must address a file or directory and
// resolves its root.
func (s Scope) ParseContext(value string) (domain.ParsedReference, Root, error) {
	ref, err := domain.ParseReference(value)
	if err != nil {
		return ref, Root{}, err
	}
	if ref.Kind == domain.ReferenceKindTicket {
		return ref, Root{}, fmt.Errorf("reference %q is a ticket id; read it with readTicket", value)
	}
	root, err := s.Resolve(ref)
	return ref, root, err
}

// Child returns the reference of entry name inside directory reference ref.
func Child(ref domain.ParsedReference, name string) string {
	if ref.Target == domain.ReferenceRootTarget {
		return ref.Owner + ":" + name
	}
	return ref.Owner + ":" + ref.Target + "/" + name
}

// within reports whether canonical path p is root or below it.
func within(root, p string, sep string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, sep)+sep)
}

// ErrOutsideRoot marks a target that resolves outside its root.
var ErrOutsideRoot = errors.New("resolves outside its root through a symlink")
