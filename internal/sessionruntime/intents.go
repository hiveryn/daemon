package sessionruntime

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// intentReplayTTL is the idempotency window. A retry of the same tool call with
// the same normalized args, from the same session, inside this window resolves
// to the original intent instead of minting a second one.
const intentReplayTTL = time.Hour

// intentResult is the resolved verdict, broadcast to every waiter and cached
// for replay. IntentID is stamped by Finish so a replayed retry can still name
// the intent it resolved to.
type intentResult struct {
	IntentID string
	Outcome  domain.IntentOutcome
	Result   any                         // the tool's typed result; nil unless Outcome.Approved()
	Inputs   domain.IntentInputValues    // validated values the exec ran with; nil unless approved
	Reason   string                      // denial reason, or error detail
	Err      error                       // non-nil only when Outcome == error
	Status   domain.DeferredIntentStatus // deferred only: the record's terminal status
}

// pendingIntent is one in-flight intent. exec is the tool's actual side effect,
// captured with its typed params at creation time — which is why the store
// itself never needs to know the payload type. It receives the validated
// approval inputs (nil for an intent without inputs).
//
// A deferred intent has no waiters: its request already returned. ready is
// closed once its creator has persisted and announced it, so a retry that
// attaches meanwhile never reads a record that is not written yet.
type pendingIntent struct {
	intent   domain.Intent
	dedupKey string
	exec     func(context.Context, domain.IntentInputValues) (any, error)
	hooks    deferredHooks       // deferred only
	claimed  bool                // CAS'd under mu: exactly one resolver wins
	waiters  []chan intentResult // each buffered 1, so a broadcast never blocks
	ready    chan struct{}       // deferred only; closed by MarkReady
}

type replayEntry struct {
	result intentResult
	at     time.Time
}

// intentStore holds pending intents and recently-resolved outcomes.
//
// Deliberately NOT generic: a single map must hold a conclude intent next to a
// createWorkTicket intent, and Go has no existential types. The core is
// type-erased and the one type assertion lives in awaitIntent[R].
//
// Lifetime is in-memory only, matching the approval store it replaces. A daemon
// restart empties it; ReconcileIntents converges the durable event log so no
// stale popup replays, and fails the durable record of every deferred intent
// that was still open — its captured exec cannot survive the restart.
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
	exec func(context.Context, domain.IntentInputValues) (any, error),
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

// BeginDeferred is Begin for a deferred intent, in the same single critical
// section and with the same dedup semantics, but without waiters: nobody
// blocks on a deferred intent. On intentAttached the caller must wait on ready
// before reading the persisted record; on intentCreated it must call
// MarkReady (or Discard) once the record is written and announced.
func (s *intentStore) BeginDeferred(
	dedupKey string,
	in domain.Intent,
	exec func(context.Context, domain.IntentInputValues) (any, error),
	hooks deferredHooks,
) (id string, ready <-chan struct{}, d intentDisposition) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if res, ok := s.lookupReplayLocked(dedupKey); ok {
		return res.IntentID, nil, intentReplayed
	}
	if p := s.lookupPendingLocked(dedupKey); p != nil {
		return p.intent.ID, p.ready, intentAttached
	}
	p := &pendingIntent{intent: in, dedupKey: dedupKey, exec: exec, hooks: hooks, ready: make(chan struct{})}
	s.registerLocked(p)
	return in.ID, p.ready, intentCreated
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
	if p.ready != nil {
		select {
		case <-p.ready:
		default:
			close(p.ready)
		}
	}
}

// MarkReady releases retries that attached to a deferred intent while its
// creator was persisting and announcing it. It reports false when the intent
// was resolved meanwhile (a session teardown claimed it before its record
// existed), so the creator can bring the record in line.
func (s *intentStore) MarkReady(intentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[intentID]
	if !ok {
		return false
	}
	if p.ready != nil {
		select {
		case <-p.ready:
		default:
			close(p.ready)
		}
	}
	return true
}

// Discard unregisters a deferred intent whose record could not be written,
// without caching a replay: there is no record for a retry to replay, so the
// retry must be free to try again.
func (s *intentStore) Discard(intentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[intentID]; ok {
		s.unregisterLocked(p)
	}
}

// Release gives up a claim whose resolution could not be recorded, so the
// intent is answerable again. Nothing ran: callers release only before exec.
func (s *intentStore) Release(intentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[intentID]; ok {
		p.claimed = false
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

	s.replay[p.dedupKey] = replayEntry{result: res, at: s.now()}
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

// Get returns a pending intent without claiming it.
func (s *intentStore) Get(intentID string) (domain.Intent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[intentID]
	if !ok {
		return domain.Intent{}, false
	}
	return p.intent, true
}

// GetForSession is Get, scoped like ClaimForSession. A claimed intent is
// reported as not found: it is already resolving and no longer answerable.
func (s *intentStore) GetForSession(sessionID, intentID string) (domain.Intent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[intentID]
	if !ok || p.claimed || p.intent.Origin.SessionID != sessionID {
		return domain.Intent{}, false
	}
	return p.intent, true
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
	cutoff := s.now().Add(-intentReplayTTL)
	for k, e := range s.replay {
		if e.at.Before(cutoff) {
			delete(s.replay, k)
		}
	}
}
