package architectfs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/contextref"
	"github.com/hiveryn/daemon/internal/domain"
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

// ScopeLocator returns the context scope — workspace and configured repos
// with their machines — of the architect whose workspace is at architectPath.
type ScopeLocator func(architectPath string) (contextref.Scope, error)

// ConfigLocator locates scopes through the current configuration.
func ConfigLocator(source config.Source) ScopeLocator {
	return func(architectPath string) (contextref.Scope, error) {
		cfg, err := source.Current()
		if err != nil {
			return contextref.Scope{}, fmt.Errorf("read configuration: %w", err)
		}
		return contextref.ScopeAt(cfg, architectPath)
	}
}

// remoteInspector inspects items (root and absolute path, joined by NUL) in
// one SSH round trip to alias.
type remoteInspector func(ctx context.Context, alias string, items []string) ([]contextref.State, error)

func inspectRemoteItems(ctx context.Context, alias string, items []string) ([]contextref.State, error) {
	pairs := make([][2]string, len(items))
	for i, item := range items {
		root, p, _ := strings.Cut(item, "\x00")
		pairs[i] = [2]string{root, p}
	}
	return contextref.InspectRemote(ctx, alias, pairs)
}

type remoteKey struct{ alias, path string }

type remoteAnswer struct {
	state contextref.State
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
func (c *remoteCache) get(key remoteKey, now time.Time) (state contextref.State, fresh, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	answer, haveAnswer := c.answers[key]
	failure, haveFailure := c.failures[key.alias]
	if haveFailure && (!haveAnswer || failure.at.After(answer.at)) {
		answer, haveAnswer = failure, true
	}
	age := now.Sub(answer.at)
	if !haveAnswer || age >= remoteStaleLimit {
		return contextref.State{}, false, false
	}
	return answer.state, age < remoteInspectTTL, true
}

// put records an inspection's answers, or its failure for the whole alias,
// and reports whether any path's newest known state changed (or was
// previously unknown).
func (c *remoteCache) put(alias string, paths []string, states []contextref.State, failure error, now time.Time) bool {
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
	previous := func(p string) (contextref.State, bool) {
		answer, haveAnswer := c.answers[remoteKey{alias, p}]
		failure, haveFailure := c.failures[alias]
		if haveFailure && (!haveAnswer || failure.at.After(answer.at)) {
			return failure.state, true
		}
		return answer.state, haveAnswer
	}
	changed := false
	for i, p := range paths {
		next := contextref.State{Err: failure}
		if failure == nil {
			next = states[i]
		}
		if prev, ok := previous(p); !ok || !prev.Equal(next) {
			changed = true
		}
	}
	if failure != nil {
		c.failures[alias] = remoteAnswer{state: contextref.State{Err: failure}, at: now}
		return changed
	}
	delete(c.failures, alias)
	for i, p := range paths {
		c.answers[remoteKey{alias, p}] = remoteAnswer{state: states[i], at: now}
	}
	return changed
}

// inspection is one detached SSH inspection of a set of paths on an alias,
// shared by every read that needs exactly those paths while it runs.
type inspection struct {
	alias string
	paths []string
	done  chan struct{}
	// Set before done closes.
	states map[string]contextref.State
	err    error
	// Guarded by TicketService.inflightMu.
	finished bool
	// late holds the workspaces of reads that gave up waiting for the answer
	// (always told when it arrives); stale those of reads that were served an
	// older answer meanwhile (told only when the answer changed).
	late  map[string]struct{}
	stale map[string]struct{}
}

func (i *inspection) state(p string) contextref.State {
	if i.err != nil {
		return contextref.State{Err: i.err}
	}
	if state, ok := i.states[p]; ok {
		return state
	}
	return contextref.State{Err: fmt.Errorf("SSH %s: path inspection did not include %s", i.alias, p)}
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
			in.states = make(map[string]contextref.State, len(paths))
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
// targets. Every entry is parsed strictly (domain.ParseReference); a value
// that is not a valid reference here is reported on its own without hiding
// the ticket or its other references. Ticket IDs are checked against the
// board; architect references are inspected on this machine; repo references
// on that repo's configured machine — locally with containment-checked stat,
// remotely from the cache or one detached SSH inspection per machine for all
// of the targets' paths, waited for at most waitBudget — and never on another
// machine as a fallback.
func (s *TicketService) resolveReferences(ctx context.Context, architectPath string, entries map[domain.TicketStatus][]ticketEntry, targets []*ticketEntry) {
	ids := ticketIDs(entries)
	var scope contextref.Scope
	var scopeErr error
	if s.locate == nil {
		scopeErr = errors.New("no architect scope locator is configured")
	} else {
		scope, scopeErr = s.locate(architectPath)
	}
	if scopeErr != nil {
		scopeErr = fmt.Errorf("cannot locate this architect's workspace and repos: %w", scopeErr)
	}

	type resolvedRef struct {
		parsed domain.ParsedReference
		root   contextref.Root
		err    error // parse or owner error: invalid
	}
	resolve := func(value string) resolvedRef {
		parsed, err := domain.ParseReference(value)
		if err != nil || parsed.Kind == domain.ReferenceKindTicket {
			return resolvedRef{parsed: parsed, err: err}
		}
		if scopeErr != nil {
			return resolvedRef{parsed: parsed, root: contextref.Root{Owner: parsed.Owner, Err: scopeErr}}
		}
		root, err := scope.Resolve(parsed)
		return resolvedRef{parsed: parsed, root: root, err: err}
	}
	remoteItem := func(r resolvedRef) (remoteKey, bool) {
		if r.err != nil || r.parsed.Kind == domain.ReferenceKindTicket || r.root.Err != nil || r.root.Machine == "" {
			return remoteKey{}, false
		}
		return remoteKey{r.root.SSH, r.root.Path + "\x00" + r.root.Join(r.parsed.Target)}, true
	}

	// Answer remote targets from the cache, collecting per alias the items to
	// inspect: those with no usable answer (needed) and those whose answer is
	// stale (refreshed in the background).
	remote := map[remoteKey]contextref.State{}
	needed := map[string][]string{}
	refresh := map[string][]string{}
	for _, entry := range targets {
		for _, value := range entry.metadata.References {
			key, ok := remoteItem(resolve(value))
			if !ok {
				continue
			}
			if _, ok := remote[key]; ok {
				continue
			}
			state, fresh, ok := s.cache.get(key, s.now())
			remote[key] = state
			switch {
			case !ok:
				needed[key.alias] = append(needed[key.alias], key.path)
			case !fresh:
				refresh[key.alias] = append(refresh[key.alias], key.path)
			}
		}
	}
	for key, state := range s.inspectPending(ctx, architectPath, needed, refresh) {
		remote[key] = state
	}

	for _, entry := range targets {
		seen := map[string]bool{}
		for i, value := range entry.metadata.References {
			at := entry.referenceAt(i)
			warn := func(code, message string) {
				entry.warnings = append(entry.warnings, domain.TicketWarning{Code: code, Message: at + message})
			}
			r := resolve(value)
			out := domain.TicketReference{Value: value}
			switch {
			case seen[value]:
				out.Status = domain.ReferenceInvalid
				out.Error = "duplicate reference; list it once"
				warn(warningInvalidReference, "duplicate reference "+value+"; list it once")
			case r.err != nil:
				out.Status = domain.ReferenceInvalid
				out.Error = r.err.Error()
				if r.parsed.Kind != "" {
					out.Kind, out.Owner, out.Target = r.parsed.Kind, r.parsed.Owner, r.parsed.Target
				}
				warn(warningInvalidReference, r.err.Error())
			case r.parsed.Kind == domain.ReferenceKindTicket:
				out.Kind = domain.ReferenceKindTicket
				out.Access = domain.ReferenceAccessReadTicket
				if _, ok := ids[value]; ok {
					out.Status, out.Exists = domain.ReferencePresent, true
				} else {
					out.Status = domain.ReferenceMissing
					warn(warningBrokenReference, "references unknown ticket "+value)
				}
			default:
				out.Kind, out.Owner, out.Target = r.parsed.Kind, r.parsed.Owner, r.parsed.Target
				out.Location, out.Machine = r.root.Location(), r.root.Machine
				out.Access = domain.ReferenceAccessReadContext
				var state contextref.State
				switch key, isRemote := remoteItem(r); {
				case r.root.Err != nil:
					state.Err = r.root.Err
				case isRemote:
					state = remote[key]
				default:
					state = contextref.InspectLocal(r.root, r.parsed.Target)
				}
				switch {
				case state.Kind != "":
					out.Status, out.Exists, out.PathKind = domain.ReferencePresent, true, state.Kind
					if state.Kind == domain.PathKindDirectory {
						out.Access = domain.ReferenceAccessListContext
					}
				case state.Missing:
					out.Status = domain.ReferenceMissing
					warn(warningMissingReference, value+" does not exist on "+r.root.Where())
				case state.Outside:
					out.Status = domain.ReferenceInvalid
					out.Error = "the target " + contextref.ErrOutsideRoot.Error() + "; reference the file inside its own root instead"
					warn(warningInvalidReference, value+": "+out.Error)
				default:
					out.Status = domain.ReferenceUnverified
					out.Error = state.Err.Error()
					where := ""
					if r.root.Err == nil {
						where = " on " + r.root.Where()
					}
					warn(warningUnverifiedReference, "cannot inspect "+value+where+": "+state.Err.Error())
				}
			}
			seen[value] = true
			entry.resolvedReferences = append(entry.resolvedReferences, out)
		}
	}
}

// inspectPending inspects each alias's needed and stale paths together in one
// detached inspection. Aliases with needed paths are waited for, all
// together, at most remoteWaitBudget (and never past ctx): a needed path
// whose machine has not answered by then is unverified, and its workspace is
// told when the answer arrives. Stale-only aliases are not waited for; the
// caller serves their cached answers.
func (s *TicketService) inspectPending(ctx context.Context, architectPath string, needed, refresh map[string][]string) map[remoteKey]contextref.State {
	answers := map[remoteKey]contextref.State{}
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
			answers[remoteKey{alias, p}] = contextref.State{Err: fmt.Errorf(
				"no answer within %s; the inspection continues in the background (up to %s), read again for its answer",
				s.waitBudget, remoteInspectTimeout)}
		}
	}
	return answers
}
