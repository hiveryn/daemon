package architectfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

const (
	// remoteInspectTimeout bounds one detached SSH inspection of a machine. It
	// runs apart from the reads that need it, so it can allow for a slow SSH
	// connection setup without holding up a board read.
	remoteInspectTimeout = 15 * time.Second
	// remoteWaitBudget is the most a ticket read waits for remote paths that
	// have no usable answer yet, so a slow or unreachable machine leaves the
	// board responsive with those paths unverified.
	remoteWaitBudget = 2 * time.Second
	// remoteInspectTTL is how long a remote answer — including a machine that
	// could not be inspected — is reused as is, so repeated board reads
	// neither open a connection each nor wait on a dead host each time.
	remoteInspectTTL = 15 * time.Second
	// remoteStaleLimit is how long an answer past remoteInspectTTL is still
	// served while a background inspection refreshes it.
	remoteStaleLimit = 10 * time.Minute
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

// get returns the newest known state of key — an answer for the path or a
// failure to inspect its machine, whichever is more recent — and whether it
// is still fresh. A stale state (older than remoteInspectTTL but younger than
// remoteStaleLimit) is served while it is refreshed in the background.
func (c *remoteCache) get(key remoteKey, now time.Time) (state pathState, fresh, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	answer, haveAnswer := c.answers[key]
	failure, haveFailure := c.failures[key.alias]
	if haveFailure && (!haveAnswer || failure.at.After(answer.at)) {
		answer, haveAnswer = failure, true
	}
	age := now.Sub(answer.at)
	if !haveAnswer || age >= remoteStaleLimit {
		return pathState{}, false, false
	}
	return answer.state, age < remoteInspectTTL, true
}

// put records an inspection's answers, or its failure for the whole alias,
// and reports whether any path's newest known state changed (or was
// previously unknown).
func (c *remoteCache) put(alias string, paths []string, states []pathState, failure error, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answers == nil {
		c.answers = map[remoteKey]remoteAnswer{}
		c.failures = map[string]remoteAnswer{}
	}
	for key, answer := range c.answers {
		if now.Sub(answer.at) >= remoteStaleLimit {
			delete(c.answers, key)
		}
	}
	previous := func(p string) (pathState, bool) {
		answer, haveAnswer := c.answers[remoteKey{alias, p}]
		failure, haveFailure := c.failures[alias]
		if haveFailure && (!haveAnswer || failure.at.After(answer.at)) {
			return failure.state, true
		}
		return answer.state, haveAnswer
	}
	changed := false
	for i, p := range paths {
		next := pathState{err: failure}
		if failure == nil {
			next = states[i]
		}
		if prev, ok := previous(p); !ok || !prev.equal(next) {
			changed = true
		}
	}
	if failure != nil {
		c.failures[alias] = remoteAnswer{state: pathState{err: failure}, at: now}
		return changed
	}
	delete(c.failures, alias)
	for i, p := range paths {
		c.answers[remoteKey{alias, p}] = remoteAnswer{state: states[i], at: now}
	}
	return changed
}

func (p pathState) equal(o pathState) bool {
	if p.kind != o.kind || p.missing != o.missing || (p.err == nil) != (o.err == nil) {
		return false
	}
	return p.err == nil || p.err.Error() == o.err.Error()
}

// inspection is one detached SSH inspection of a set of paths on an alias,
// shared by every read that needs exactly those paths while it runs.
type inspection struct {
	alias string
	paths []string
	done  chan struct{}
	// Set before done closes.
	states map[string]pathState
	err    error
	// Guarded by TicketService.inflightMu.
	finished bool
	// late holds the workspaces of reads that gave up waiting for the answer
	// (always told when it arrives); stale those of reads that were served an
	// older answer meanwhile (told only when the answer changed).
	late  map[string]struct{}
	stale map[string]struct{}
}

func (i *inspection) state(p string) pathState {
	if i.err != nil {
		return pathState{err: i.err}
	}
	if state, ok := i.states[p]; ok {
		return state
	}
	return pathState{err: fmt.Errorf("SSH %s: path inspection did not include %s", i.alias, p)}
}

// inspect returns the running inspection of exactly these paths on alias, or
// starts one. It runs detached from any request, bounded by
// remoteInspectTimeout, caches its answer and, when it ends, tells the
// workspaces whose reads did not see it through referencesChanged.
func (s *TicketService) inspect(alias string, paths []string) *inspection {
	paths = slices.Clone(paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	key := alias + "\x00" + strings.Join(paths, "\x00")

	s.inflightMu.Lock()
	if running, ok := s.inflight[key]; ok {
		s.inflightMu.Unlock()
		return running
	}
	if s.inflight == nil {
		s.inflight = map[string]*inspection{}
	}
	in := &inspection{alias: alias, paths: paths, done: make(chan struct{}), late: map[string]struct{}{}, stale: map[string]struct{}{}}
	s.inflight[key] = in
	s.inflightMu.Unlock()

	s.background.Add(1)
	go func() {
		defer s.background.Done()
		ctx, cancel := context.WithTimeout(context.Background(), remoteInspectTimeout)
		states, err := s.inspectRemote(ctx, alias, paths)
		cancel()
		changed := s.cache.put(alias, paths, states, err, s.now())
		in.err = err
		if err == nil {
			in.states = make(map[string]pathState, len(paths))
			for n, p := range paths {
				in.states[p] = states[n]
			}
		}

		s.inflightMu.Lock()
		delete(s.inflight, key)
		in.finished = true
		notify := in.late
		if changed {
			for workspace := range in.stale {
				notify[workspace] = struct{}{}
			}
		}
		s.inflightMu.Unlock()
		close(in.done)

		if s.referencesChanged != nil {
			for workspace := range notify {
				s.referencesChanged(workspace)
			}
		}
	}()
	return in
}

// SetReferencesChanged registers fn to be told, with the architect workspace
// path, when a remote inspection answers after a read of that workspace
// stopped waiting for it, or changes an answer such a read was served from
// the cache — so clients can read the board again.
func (s *TicketService) SetReferencesChanged(fn func(architectPath string)) {
	s.referencesChanged = fn
}

// waitForBackground waits until every detached inspection has ended.
func (s *TicketService) waitForBackground() { s.background.Wait() }

// resolveReferences fills resolvedReferences and reference warnings for the
// targets. Ticket-ID references are checked against every ticket on the
// board; absolute paths are inspected on the target ticket's primary-repo
// machine — locally with os.Stat, remotely from the cache or one detached
// SSH inspection per machine for all of the targets' paths, waited for at
// most waitBudget — and never on another machine as a fallback.
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

	// Answer remote paths from the cache, collecting per alias the paths to
	// inspect: those with no usable answer (needed) and those whose answer
	// is stale (refreshed in the background).
	remote := map[remoteKey]pathState{}
	needed := map[string][]string{}
	refresh := map[string][]string{}
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
			if strings.ContainsAny(ref, "\n\x00") {
				// Not representable in the line protocol.
				remote[key] = pathState{err: errors.New("path contains a newline or NUL and cannot be inspected over SSH")}
				continue
			}
			state, fresh, ok := s.cache.get(key, s.now())
			remote[key] = state
			switch {
			case !ok:
				needed[key.alias] = append(needed[key.alias], ref)
			case !fresh:
				refresh[key.alias] = append(refresh[key.alias], ref)
			}
		}
	}
	for key, state := range s.inspectPending(ctx, architectPath, needed, refresh) {
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

// inspectPending inspects each alias's needed and stale paths together in one
// detached inspection. Aliases with needed paths are waited for, all
// together, at most remoteWaitBudget (and never past ctx): a needed path
// whose machine has not answered by then is unverified, and its workspace is
// told when the answer arrives. Stale-only aliases are not waited for; the
// caller serves their cached answers.
func (s *TicketService) inspectPending(ctx context.Context, architectPath string, needed, refresh map[string][]string) map[remoteKey]pathState {
	answers := map[remoteKey]pathState{}
	waiting := map[string]*inspection{}
	for alias, paths := range needed {
		waiting[alias] = s.inspect(alias, append(paths, refresh[alias]...))
	}
	for alias, paths := range refresh {
		if _, ok := needed[alias]; ok {
			continue
		}
		in := s.inspect(alias, paths)
		s.inflightMu.Lock()
		finished := in.finished
		if !finished {
			in.stale[architectPath] = struct{}{}
		}
		s.inflightMu.Unlock()
		if finished {
			// Answered already, so it would not tell this read: use it.
			for _, p := range paths {
				answers[remoteKey{alias, p}] = in.state(p)
			}
		}
	}
	if len(waiting) == 0 {
		return answers
	}

	budget := time.NewTimer(s.waitBudget)
	defer budget.Stop()
	expired := false
	for alias, in := range waiting {
		if !expired {
			select {
			case <-in.done:
			case <-budget.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		s.inflightMu.Lock()
		finished := in.finished
		if !finished {
			in.late[architectPath] = struct{}{}
		}
		s.inflightMu.Unlock()
		for _, p := range needed[alias] {
			if finished {
				answers[remoteKey{alias, p}] = in.state(p)
				continue
			}
			answers[remoteKey{alias, p}] = pathState{err: fmt.Errorf(
				"no answer within %s; the inspection continues in the background (up to %s), read again for its answer",
				s.waitBudget, remoteInspectTimeout)}
		}
	}
	return answers
}
