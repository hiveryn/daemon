package sessionruntime

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// intentReplayTTL is the default idempotency window. A retry of the same tool
// call with the same normalized args, from the same session, inside this window
// resolves to the original intent instead of minting a second one. The window
// is anchored at resolution (Finish), not at the request.
const intentReplayTTL = time.Hour

// intentResult is the resolved verdict, broadcast to every waiter and cached
// for replay. IntentID is stamped by Finish so a replayed retry can still name
// the intent it resolved to.
type intentResult struct {
	IntentID string
	Outcome  domain.IntentOutcome
	Result   any    // the tool's typed result; nil unless Outcome.Approved()
	Reason   string // denial reason, or error detail
	Err      error  // non-nil only when Outcome == error
}

// pendingIntent is one in-flight intent. exec is the tool's actual side effect,
// captured with its typed params at creation time — which is why the store
// itself never needs to know the payload type.
type pendingIntent struct {
	intent   domain.Intent
	dedupKey string
	exec     func(context.Context) (any, error)
	claimed  bool                // CAS'd under mu: exactly one resolver wins
	waiters  []chan intentResult // each buffered 1, so a broadcast never blocks
}

type replayEntry struct {
	result  intentResult
	expires time.Time // resolution time + the tool's replay window
}

// intentStore holds pending intents and recently-resolved outcomes.
//
// Deliberately NOT generic: a single map must hold a conclude intent next to a
// createWorkTicket intent, and Go has no existential types. The core is
// type-erased and the one type assertion lives in awaitIntent[R].
//
// Lifetime is in-memory only, matching the approval store it replaces. A daemon
// restart empties it; ReconcileIntents converges the durable event log so no
// stale popup replays.
type intentStore struct {
	mu        sync.Mutex
	pending   map[string]*pendingIntent      // intentID → pending
	byDedup   map[string]string              // dedupKey → intentID (pending only)
	bySession map[string]map[string]struct{} // sessionID → intentIDs (pending only)
	replay    map[string]replayEntry         // dedupKey → resolved outcome, TTL-swept
	now       func() time.Time
}

func newIntentStore() *intentStore {
	return &intentStore{
		pending:   map[string]*pendingIntent{},
		byDedup:   map[string]string{},
		bySession: map[string]map[string]struct{}{},
		replay:    map[string]replayEntry{},
		now:       func() time.Time { return time.Now().UTC() },
	}
}

type intentDisposition int

const (
	// intentCreated: this caller owns publishing the event and starting the
	// policy goroutine, then waits.
	intentCreated intentDisposition = iota
	// intentAttached: an identical intent is already in flight; just wait.
	intentAttached
	// intentReplayed: an identical intent already resolved inside the TTL;
	// return its outcome without blocking.
	intentReplayed
)

// Begin resolves a tool call to one of: replay a resolved outcome, attach to an
// in-flight intent, or create a new one.
//
// All three cases share ONE critical section on purpose. Splitting the replay
// lookup from the dedup lookup would let two concurrent retries both miss and
// both create a ticket — which is the exact bug this store exists to prevent.
func (s *intentStore) Begin(
	dedupKey string,
	in domain.Intent,
	exec func(context.Context) (any, error),
) (id string, ch <-chan intentResult, replayed intentResult, d intentDisposition) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if res, ok := s.lookupReplayLocked(dedupKey); ok {
		return "", nil, res, intentReplayed
	}

	// Attaching is legal even when the entry is already claimed: the entry
	// stays registered until Finish, so a retry landing mid-exec joins the
	// broadcast instead of minting a duplicate.
	if p := s.lookupPendingLocked(dedupKey); p != nil {
		w := make(chan intentResult, 1)
		p.waiters = append(p.waiters, w)
		return p.intent.ID, w, intentResult{}, intentAttached
	}

	w := make(chan intentResult, 1)
	s.registerLocked(&pendingIntent{
		intent:   in,
		dedupKey: dedupKey,
		exec:     exec,
		waiters:  []chan intentResult{w},
	})
	return in.ID, w, intentResult{}, intentCreated
}

// Known reports whether dedupKey has an intent in flight or a resolved
// outcome still inside its replay window — that is, whether Begin would
// attach or replay rather than create. A tool uses it to let a retry reach
// that outcome even when the request would no longer pass its pre-approval
// checks because the original already took effect.
func (s *intentStore) Known(dedupKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lookupReplayLocked(dedupKey); ok {
		return true
	}
	return s.lookupPendingLocked(dedupKey) != nil
}

func (s *intentStore) lookupReplayLocked(dedupKey string) (intentResult, bool) {
	// Lazy sweep on access is the replay cache's entire lifecycle — no
	// goroutine, no timer. Safe given the daemon's load profile (1 desktop,
	// 0-2 MCP sessions): an hour of intents is a handful of entries.
	s.sweepLocked()
	e, ok := s.replay[dedupKey]
	return e.result, ok
}

func (s *intentStore) lookupPendingLocked(dedupKey string) *pendingIntent {
	existingID, ok := s.byDedup[dedupKey]
	if !ok {
		return nil
	}
	if p := s.pending[existingID]; p != nil {
		return p
	}
	// byDedup and pending are always mutated together; a dangling key is an
	// invariant violation. Drop it rather than attach to nothing.
	delete(s.byDedup, dedupKey)
	return nil
}

func (s *intentStore) registerLocked(p *pendingIntent) {
	s.pending[p.intent.ID] = p
	s.byDedup[p.dedupKey] = p.intent.ID
	if s.bySession[p.intent.Origin.SessionID] == nil {
		s.bySession[p.intent.Origin.SessionID] = map[string]struct{}{}
	}
	s.bySession[p.intent.Origin.SessionID][p.intent.ID] = struct{}{}
}

func (s *intentStore) unregisterLocked(p *pendingIntent) {
	delete(s.byDedup, p.dedupKey)
	delete(s.pending, p.intent.ID)
	if set, ok := s.bySession[p.intent.Origin.SessionID]; ok {
		delete(set, p.intent.ID)
		if len(set) == 0 {
			delete(s.bySession, p.intent.Origin.SessionID)
		}
	}
}

// Claim takes exclusive ownership of resolving an intent. It does NOT remove
// the entry — Finish does. If Claim removed it, a retry arriving between Claim
// and Finish would find neither a pending entry nor a replay entry and would
// create a duplicate. This is the subtlest duplicate path in the design.
func (s *intentStore) Claim(intentID string) (*pendingIntent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimLocked(intentID)
}

// ClaimForSession is Claim, scoped to a session so one session cannot resolve
// another's intent. A mismatch is reported as "not found" rather than a
// distinct error: the caller has no business learning the intent exists.
func (s *intentStore) ClaimForSession(sessionID, intentID string) (*pendingIntent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[intentID]
	if !ok || p.intent.Origin.SessionID != sessionID {
		return nil, false
	}
	return s.claimLocked(intentID)
}

func (s *intentStore) claimLocked(intentID string) (*pendingIntent, bool) {
	p, ok := s.pending[intentID]
	if !ok || p.claimed {
		return nil, false
	}
	p.claimed = true
	return p, true
}

// Finish broadcasts the result to every waiter, records it for replay, and
// unregisters the intent. exec must already have run: Finish holds the mutex
// and must never span I/O.
func (s *intentStore) Finish(intentID string, res intentResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.pending[intentID]
	if !ok {
		return
	}
	res.IntentID = p.intent.ID
	for _, w := range p.waiters {
		// Buffered 1 and written exactly once, so this never blocks. The
		// default arm guards a detached-but-not-removed waiter.
		select {
		case w <- res:
		default:
		}
	}
	p.waiters = nil

	// An error is not replayed: the agent is told a single retry is reasonable,
	// and replaying the failure would make that retry a no-op for the whole
	// window. The retry is a new request that the user approves afresh — a
	// failed approval is never carried over as authorization. Every
	// mutation re-checks its own state on execution (a concluded session, a
	// ticket no longer in backlog), so an error that did take effect is
	// refused rather than repeated.
	if res.Outcome != domain.IntentOutcomeError {
		s.replay[p.dedupKey] = replayEntry{result: res, expires: s.now().Add(intentReplayTTL)}
	}
	s.unregisterLocked(p)
}

// Detach drops one waiter whose caller walked away (its ctx died). The intent
// itself lives on and still resolves — that is what makes the retry replayable.
func (s *intentStore) Detach(intentID string, ch <-chan intentResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.pending[intentID]
	if !ok {
		return
	}
	for i, w := range p.waiters {
		if (<-chan intentResult)(w) == ch {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return
		}
	}
}

// ResolvingForSession reports whether the session's intent is claimed and its
// resolution still running: no longer answerable, but not resolved either.
func (s *intentStore) ResolvingForSession(sessionID, intentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[intentID]
	return ok && p.claimed && p.intent.Origin.SessionID == sessionID
}

// PendingForSession lists the session's unresolved intent ids, sorted for
// deterministic teardown.
func (s *intentStore) PendingForSession(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.bySession[sessionID]))
	for id := range s.bySession[sessionID] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *intentStore) sweepLocked() {
	now := s.now()
	for k, e := range s.replay {
		if e.expires.Before(now) {
			delete(s.replay, k)
		}
	}
}
