package architectfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

const (
	// remoteInspectTimeout bounds one board read's SSH inspection of a machine,
	// so an unreachable host costs a board read seconds, not a TCP timeout.
	remoteInspectTimeout = 5 * time.Second
	// remoteInspectTTL is how long a remote answer — including a machine that
	// could not be inspected — is reused, so repeated board reads neither open
	// a connection each nor wait on a dead host each time.
	remoteInspectTTL = 15 * time.Second
)

// PathLocation is where a ticket's absolute path references are interpreted:
// the machine of its primary repo. An empty Machine is this machine.
type PathLocation struct {
	Machine string
	SSH     string
}

func (l PathLocation) String() string {
	if l.Machine == "" {
		return "this machine (local)"
	}
	return fmt.Sprintf("machine %q (SSH %s)", l.Machine, l.SSH)
}

// RepoLocator resolves a ticket's primary repo key, in the architect
// workspace at architectPath, to the location its path references live at.
type RepoLocator func(architectPath, repo string) (PathLocation, error)

// ConfigLocator locates repos through the current configuration: the
// architect registered at architectPath and its repo's configured machine.
func ConfigLocator(source config.Source) RepoLocator {
	return func(architectPath, repo string) (PathLocation, error) {
		if strings.TrimSpace(repo) == "" {
			return PathLocation{}, errors.New("ticket has no primary repo to locate its path references")
		}
		cfg, err := source.Current()
		if err != nil {
			return PathLocation{}, fmt.Errorf("read configuration: %w", err)
		}
		architect, ok := architectAt(cfg, architectPath)
		if !ok {
			return PathLocation{}, fmt.Errorf("no architect is registered at %s", architectPath)
		}
		if _, ok := architect.Repos[repo]; !ok {
			return PathLocation{}, fmt.Errorf("primary repo %q is not configured in %s", repo, filepath.Join(architect.Path, "hiveryn.yaml"))
		}
		machine := architect.RepoMachines[repo]
		if machine == "" {
			return PathLocation{}, nil
		}
		m, ok := cfg.Machines[machine]
		if !ok || m.SSH == "" {
			return PathLocation{}, fmt.Errorf("primary repo %q references unknown machine %q", repo, machine)
		}
		return PathLocation{Machine: machine, SSH: m.SSH}, nil
	}
}

func architectAt(cfg config.Config, architectPath string) (config.ArchitectConfig, bool) {
	want := filepath.Clean(architectPath)
	for _, architect := range cfg.Architects {
		if filepath.Clean(architect.Path) == want {
			return architect, true
		}
	}
	return config.ArchitectConfig{}, false
}

// pathState is the result of inspecting one path on its location.
type pathState struct {
	kind domain.PathReferenceKind // set when the path exists
	// missing is confirmed absence; with neither kind nor missing set the path
	// could not be inspected and err says why.
	missing bool
	err     error
}

// remoteInspector inspects paths in one SSH round trip to alias.
type remoteInspector func(ctx context.Context, alias string, paths []string) ([]pathState, error)

// inspectRemotePaths stats every path on the SSH machine in one invocation.
// Paths travel on stdin, one per line, and answers come back one line each in
// order: d(irectory), f(ile), m(issing) or u (an ancestor that cannot be
// searched, so existence is unknown). It follows symlinks like os.Stat.
func inspectRemotePaths(ctx context.Context, alias string, paths []string) ([]pathState, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	const script = `while IFS= read -r p; do
if [ -d "$p" ]; then echo d
elif [ -e "$p" ]; then echo f
else
  q=$p
  while [ -n "$q" ] && [ ! -e "$q" ]; do q=${q%/*}; done
  if [ -n "$q" ] && [ -d "$q" ] && [ ! -x "$q" ]; then echo u; else echo m; fi
fi
done`
	var input strings.Builder
	for _, p := range paths {
		input.WriteString(p)
		input.WriteByte('\n')
	}
	out, err := remoteexec.Run(ctx, alias, script, strings.NewReader(input.String()))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("SSH %s: path inspection returned %d answers for %d paths", alias, len(lines), len(paths))
	}
	states := make([]pathState, len(paths))
	for i, line := range lines {
		switch line {
		case "d":
			states[i].kind = domain.PathReferenceDirectory
		case "f":
			states[i].kind = domain.PathReferenceFile
		case "m":
			states[i].missing = true
		case "u":
			states[i].err = errors.New("permission denied searching an ancestor directory")
		default:
			return nil, fmt.Errorf("SSH %s: unexpected path inspection answer %q", alias, line)
		}
	}
	return states, nil
}

func inspectLocalPath(p string) pathState {
	info, err := os.Stat(p)
	switch {
	case err == nil && info.IsDir():
		return pathState{kind: domain.PathReferenceDirectory}
	case err == nil:
		return pathState{kind: domain.PathReferenceFile}
	case os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR):
		return pathState{missing: true}
	default:
		return pathState{err: err}
	}
}

type remoteKey struct{ alias, path string }

type remoteAnswer struct {
	state pathState
	at    time.Time
}

// remoteCache remembers recent remote answers per alias and path, and recent
// failures to reach an alias at all.
type remoteCache struct {
	mu       sync.Mutex
	answers  map[remoteKey]remoteAnswer
	failures map[string]remoteAnswer
}

func (c *remoteCache) get(key remoteKey, now time.Time) (pathState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if failure, ok := c.failures[key.alias]; ok && now.Sub(failure.at) < remoteInspectTTL {
		return failure.state, true
	}
	answer, ok := c.answers[key]
	if !ok || now.Sub(answer.at) >= remoteInspectTTL {
		return pathState{}, false
	}
	return answer.state, true
}

func (c *remoteCache) put(alias string, paths []string, states []pathState, failure error, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answers == nil {
		c.answers = map[remoteKey]remoteAnswer{}
		c.failures = map[string]remoteAnswer{}
	}
	for key, answer := range c.answers {
		if now.Sub(answer.at) >= remoteInspectTTL {
			delete(c.answers, key)
		}
	}
	if failure != nil {
		c.failures[alias] = remoteAnswer{state: pathState{err: failure}, at: now}
		return
	}
	delete(c.failures, alias)
	for i, p := range paths {
		c.answers[remoteKey{alias, p}] = remoteAnswer{state: states[i], at: now}
	}
}

// resolveReferences fills resolvedReferences and reference warnings for the
// targets. Ticket-ID references are checked against every ticket on the
// board; absolute paths are inspected on the target ticket's primary-repo
// machine — locally with os.Stat, remotely with one SSH call per machine for
// all of the targets' paths — and never on another machine as a fallback.
func (s *TicketService) resolveReferences(ctx context.Context, architectPath string, entries map[domain.TicketStatus][]ticketEntry, targets []*ticketEntry) {
	ids := ticketIDs(entries)

	type located struct {
		loc PathLocation
		err error
	}
	locations := map[string]located{}
	locate := func(repo string) located {
		if l, ok := locations[repo]; ok {
			return l
		}
		var l located
		if s.locate == nil {
			l.err = errors.New("no repository locator is configured")
		} else {
			l.loc, l.err = s.locate(architectPath, repo)
		}
		if l.err != nil {
			l.err = fmt.Errorf("cannot locate the machine of primary repo %q: %w", repo, l.err)
		}
		locations[repo] = l
		return l
	}

	// Answer remote paths from the cache, collecting the rest per alias.
	remote := map[remoteKey]pathState{}
	pending := map[string][]string{}
	for _, entry := range targets {
		l := locate(entry.metadata.Repo)
		if l.err != nil || l.loc.Machine == "" {
			continue
		}
		for _, ref := range entry.metadata.References {
			key := remoteKey{l.loc.SSH, ref}
			if !filepath.IsAbs(ref) {
				continue
			}
			if _, ok := remote[key]; ok {
				continue
			}
			state, ok := s.cache.get(key, s.now())
			remote[key] = state
			if !ok {
				pending[l.loc.SSH] = append(pending[l.loc.SSH], ref)
			}
		}
	}
	for key, state := range s.inspectPending(ctx, pending) {
		remote[key] = state
	}

	for _, entry := range targets {
		l := locate(entry.metadata.Repo)
		for _, ref := range entry.metadata.References {
			if !filepath.IsAbs(ref) {
				resolved := domain.TicketReference{Value: ref, Type: domain.TicketReferenceTicket}
				if _, ok := ids[ref]; !ok {
					entry.warnings = append(entry.warnings, domain.TicketWarning{
						Code:    warningBrokenReference,
						Message: "references unknown ticket " + ref,
					})
				} else {
					resolved.Exists = true
				}
				entry.resolvedReferences = append(entry.resolvedReferences, resolved)
				continue
			}

			resolved := domain.TicketReference{Value: ref, Type: domain.TicketReferencePath}
			var state pathState
			switch {
			case l.err != nil:
				state.err = l.err
				resolved.Location = domain.PathReferenceUnknown
			case l.loc.Machine == "":
				state = inspectLocalPath(ref)
				resolved.Location = domain.PathReferenceLocal
			default:
				state = remote[remoteKey{l.loc.SSH, ref}]
				resolved.Location = domain.PathReferenceRemote
				resolved.Machine = l.loc.Machine
			}
			switch {
			case state.kind != "":
				resolved.Exists = true
				resolved.Kind = state.kind
				resolved.Status = domain.PathReferencePresent
			case state.missing:
				resolved.Status = domain.PathReferenceMissing
				entry.warnings = append(entry.warnings, domain.TicketWarning{
					Code:    warningMissingPathReference,
					Message: "referenced path does not exist on " + l.loc.String() + ": " + ref,
				})
			default:
				resolved.Status = domain.PathReferenceUnverified
				resolved.Error = state.err.Error()
				where := ""
				if l.err == nil {
					where = " on " + l.loc.String()
				}
				entry.warnings = append(entry.warnings, domain.TicketWarning{
					Code:    warningUnverifiedPathReference,
					Message: "cannot inspect referenced path " + ref + where + ": " + state.err.Error(),
				})
			}
			entry.resolvedReferences = append(entry.resolvedReferences, resolved)
		}
	}
}

// inspectPending runs one bounded inspection per alias, concurrently, and
// returns (and caches) each answer, or the alias's failure for its paths.
func (s *TicketService) inspectPending(ctx context.Context, pending map[string][]string) map[remoteKey]pathState {
	answers := map[remoteKey]pathState{}
	if len(pending) == 0 {
		return answers
	}
	ctx, cancel := context.WithTimeout(ctx, remoteInspectTimeout)
	defer cancel()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for alias, paths := range pending {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := map[remoteKey]pathState{}
			inspectable := make([]string, 0, len(paths))
			for _, p := range paths {
				if strings.ContainsAny(p, "\n\x00") {
					// Not representable in the line protocol.
					result[remoteKey{alias, p}] = pathState{err: errors.New("path contains a newline or NUL and cannot be inspected over SSH")}
					continue
				}
				inspectable = append(inspectable, p)
			}
			if len(inspectable) > 0 {
				states, err := s.inspectRemote(ctx, alias, inspectable)
				s.cache.put(alias, inspectable, states, err, s.now())
				for i, p := range inspectable {
					if err != nil {
						result[remoteKey{alias, p}] = pathState{err: err}
					} else {
						result[remoteKey{alias, p}] = states[i]
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for key, state := range result {
				answers[key] = state
			}
		}()
	}
	wg.Wait()
	return answers
}
