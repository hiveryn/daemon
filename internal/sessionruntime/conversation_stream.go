package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

// Conversation stream observation (GET /api/sessions/{id}/stream).
//
// The stream never buffers events per client. Each subscriber holds a
// one-slot "changed" signal; on a signal its handler re-reads the durable
// sources itself (the conversation version feed after its cursor, the
// session's question/intent events, the session record). Signals coalesce,
// so a slow or stalled reader only lags behind and catches up from the store:
// it never blocks native record ingestion, the observer, sends or other
// sessions, and nothing is dropped silently.
//
// Terminal sessions have no push source for their native record, so one
// observer per session (shared by all of its subscribers, started with the
// first and stopped with the last) reads it through syncTerminalHistory — the
// same throttled, serialized read GET /conversation uses, so a stream and a
// recovery read never overlap and a remote session keeps using its one
// OpenSSH master and incremental mirror. The observer waits observeInterval
// after each read completes (never a fixed tick that could stack reads), backs
// off on failure, and reports a status change when the session record or the
// read outcome changes. It only reads: it never touches Claude activity, the
// hand-off slot or submissions beyond the reconciliation GET /conversation
// already triggers.

var (
	// observeInterval is the longest pause after an observer pass; session
	// activity (hook and status events) ends it early, but never before the
	// minimum gap. Locally a pass re-parses the native record (milliseconds);
	// remotely it is one SSH command over the session's master (~1.3 s
	// measured), so a busy remote session is read about every 2.3 s and an
	// idle one about every 3.3 s.
	observeInterval     = 2 * time.Second
	observeMinGap       = 250 * time.Millisecond
	observeRemoteMinGap = time.Second
	// observeBackoffMax bounds the pause after consecutive failed reads.
	observeBackoffMax = 15 * time.Second
	// observeReadBound bounds one native read; the read is detached from
	// subscriber lifetime so a disconnect never cancels it halfway (its
	// result is cached for GET /conversation too).
	observeReadBound = 30 * time.Second
)

// ConversationWatch is one stream subscriber of a session.
type ConversationWatch struct {
	changed chan struct{}
	obs     *conversationObserver
	release func()
	once    sync.Once
}

// Changed signals (coalesced) that a durable source may have changed.
func (w *ConversationWatch) Changed() <-chan struct{} { return w.changed }

// Done is closed when the daemon shuts down; the client reconnects later.
func (w *ConversationWatch) Done() <-chan struct{} { return w.obs.stopped }

// HistoryError is the latest native read failure (nil after a success).
func (w *ConversationWatch) HistoryError() error {
	w.obs.mu.Lock()
	defer w.obs.mu.Unlock()
	return w.obs.historyErr
}

// Close releases the subscription; the last one stops the observer.
func (w *ConversationWatch) Close() { w.once.Do(w.release) }

type conversationObserver struct {
	id      string
	stopped <-chan struct{}
	cancel  context.CancelFunc
	readCtx context.Context
	wake    chan struct{}

	mu         sync.Mutex
	subs       map[*ConversationWatch]struct{}
	historyErr error
}

func (o *conversationObserver) signal() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for w := range o.subs {
		select {
		case w.changed <- struct{}{}:
		default:
		}
	}
}

type conversationStreams struct {
	mu        sync.Mutex
	observers map[string]*conversationObserver
	// stop is closed at shutdown; readCtx (cancelled then) bounds native reads,
	// and running counts observer loops so shutdown waits for in-flight reads
	// before the remote conversation masters are closed.
	stop       chan struct{}
	stopOnce   sync.Once
	readCtx    context.Context
	readCancel context.CancelFunc
	running    sync.WaitGroup
}

// init lazily prepares the stop channel and read context (c.mu held).
func (c *conversationStreams) init() {
	if c.stop == nil {
		c.stop = make(chan struct{})
		c.readCtx, c.readCancel = context.WithCancel(context.Background())
	}
}

// WatchConversation subscribes to a session's stream sources. The
// subscription exists before the caller's first read, so any change after
// that read is signalled (no snapshot-to-live gap). A session that is not
// known (neither live nor retained history) is NotFound.
func (s *Service) WatchConversation(ctx context.Context, id string) (domain.ConversationWatch, error) {
	if session, err := s.repo.GetSession(ctx, id); err == nil {
		// Fail before streaming instead of a reconnect loop around a route
		// that can never exist.
		if session.InteractionMode == domain.InteractionChat && session.Machine != "" {
			return nil, &domain.ConversationError{Code: domain.HistoryUnsupported, Message: "Remote SSH chat sessions are unsupported"}
		}
		if session.InteractionMode != domain.InteractionChat && s.terminalConversationUnsupported(session) {
			return nil, &domain.ConversationError{Code: domain.HistoryUnsupported, Message: "History unsupported: " + s.terminalCapabilities(session).Limitation}
		}
	} else {
		var missing *domain.NotFoundError
		if !errors.As(err, &missing) {
			return nil, err
		}
		repo, repoErr := s.conversationRepo()
		if repoErr != nil {
			return nil, repoErr
		}
		archived, archErr := repo.IsArchivedChat(ctx, id)
		if archErr != nil {
			return nil, archErr
		}
		if !archived {
			return nil, err
		}
	}
	c := &s.streams
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	stop := c.stop
	select {
	case <-stop:
		return nil, &domain.ConflictError{Resource: "conversation stream", Message: "the daemon is shutting down"}
	default:
	}
	if c.observers == nil {
		c.observers = map[string]*conversationObserver{}
	}
	o := c.observers[id]
	if o == nil {
		obsCtx, cancel := context.WithCancel(context.Background())
		o = &conversationObserver{id: id, stopped: stop, cancel: cancel, readCtx: c.readCtx, wake: make(chan struct{}, 1), subs: map[*ConversationWatch]struct{}{}}
		c.observers[id] = o
		c.running.Add(1)
		go func() {
			defer c.running.Done()
			s.observeConversation(obsCtx, o)
		}()
	}
	w := &ConversationWatch{changed: make(chan struct{}, 1), obs: o}
	w.release = func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		o.mu.Lock()
		delete(o.subs, w)
		empty := len(o.subs) == 0
		o.mu.Unlock()
		if empty && c.observers[id] == o {
			delete(c.observers, id)
			o.cancel()
		}
	}
	o.mu.Lock()
	o.subs[w] = struct{}{}
	o.mu.Unlock()
	return w, nil
}

// ReadConversationFeed reads stored feed revisions without reading the native
// record (the observer does that); ended sessions keep their retained feed.
func (s *Service) ReadConversationFeed(ctx context.Context, id string, q domain.ConversationQuery) (domain.ConversationPage, error) {
	repo, err := s.conversationRepo()
	if err != nil {
		return domain.ConversationPage{}, err
	}
	return repo.ReadConversation(ctx, id, q)
}

// conversationChanged signals a session's subscribers after a durable write
// to its conversation feed or controls.
func (s *Service) conversationChanged(id string) {
	c := &s.streams
	c.mu.Lock()
	o := c.observers[id]
	c.mu.Unlock()
	if o != nil {
		o.signal()
	}
}

// stopConversationStreams ends every stream at shutdown (handlers return so
// the HTTP server can stop; clients reconnect to the next daemon) and waits,
// bounded by ctx, for observer reads in flight.
func (s *Service) stopConversationStreams(ctx context.Context) {
	c := &s.streams
	c.mu.Lock()
	c.init()
	for id, o := range c.observers {
		o.cancel()
		delete(c.observers, id)
	}
	c.readCancel()
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.stop) })
	done := make(chan struct{})
	go func() {
		c.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("conversation stream observers still reading at shutdown", "error", ctx.Err())
	}
}

// sessionActivity wakes a session's observer early: hook and status events
// arrive as the provider writes its record (a tool step, a turn end), so the
// record is read right away instead of at the next interval.
func (s *Service) sessionActivity(id string) {
	c := &s.streams
	c.mu.Lock()
	o := c.observers[id]
	c.mu.Unlock()
	if o != nil {
		select {
		case o.wake <- struct{}{}:
		default:
		}
	}
}

// observeConversation is the per-session observer loop.
func (s *Service) observeConversation(ctx context.Context, o *conversationObserver) {
	var lastStatus string
	failures := 0
	for {
		remote := false
		session, err := s.GetSession(ctx, o.id)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Ended (or unreadable): subscribers re-read and report it; an
			// ended session has nothing left to observe.
			o.signal()
			var missing *domain.NotFoundError
			if errors.As(err, &missing) {
				return
			}
			s.logger.Warn("conversation stream: read session", "session_id", o.id, "error", err)
			failures++
		} else {
			remote = session.Machine != ""
			readErr := s.observeNativeRecord(session, o.readCtx)
			if readErr != nil {
				failures++
			} else {
				failures = 0
			}
			status := streamStatusKey(session, readErr)
			o.mu.Lock()
			o.historyErr = readErr
			o.mu.Unlock()
			if status != lastStatus {
				lastStatus = status
				o.signal()
			}
		}
		if failures > 0 {
			// Failing reads back off and ignore activity wake-ups.
			delay := observeInterval
			for i := 1; i < failures && delay < observeBackoffMax; i++ {
				delay *= 2
			}
			if !sleepContext(ctx, min(delay, observeBackoffMax)) {
				return
			}
			continue
		}
		// At least the minimum gap between passes (one SSH command per pass
		// remotely), then the next activity or the interval, whichever is first.
		gap := observeMinGap
		if remote {
			gap = observeRemoteMinGap
		}
		if !sleepContext(ctx, gap) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-time.After(observeInterval - gap):
		}
	}
}

// observeNativeRecord reads a terminal session's native record into the
// feed (signalling subscribers through the write). Chat sessions write their
// feed as transport events arrive; sessions without a history route now have
// nothing to read (their status says why).
func (s *Service) observeNativeRecord(session domain.Session, readCtx context.Context) error {
	if session.InteractionMode == domain.InteractionChat || !s.terminalCapabilities(session).History || s.sessionEnding(session.ID) {
		// An ending session gets its final read from the teardown itself;
		// reading now could reopen a remote master the teardown just closed.
		return nil
	}
	ctx, cancel := context.WithTimeout(readCtx, observeReadBound)
	defer cancel()
	if _, err := s.syncTerminalHistory(ctx, session, false); err != nil {
		return historyError(err)
	}
	s.kickMessages()
	return nil
}

// streamStatusKey fingerprints what a status event shows.
func streamStatusKey(session domain.Session, readErr error) string {
	data, _ := json.Marshal(struct {
		Session *domain.Session
		Err     string
	}{&session, errorText(readErr)})
	return string(data)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) sessionEnding(id string) bool {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	_, ending := s.ending[id]
	return ending
}
