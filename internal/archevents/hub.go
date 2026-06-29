package archevents

import (
	"log/slog"
	"sync"

	"github.com/hiveryn/daemon/internal/domain"
)

// subscriberBuffer is the per-subscriber channel depth. A conclude emits a tiny
// burst, so 64 comfortably absorbs transient slowness; a subscriber that still
// overflows is closed (see Publish) rather than silently dropping events.
const subscriberBuffer = 64

type subscriber struct {
	ch     chan domain.ArchitectEvent
	closed bool
}

type Hub struct {
	mu          sync.RWMutex
	logger      *slog.Logger
	nextID      uint64
	subscribers map[string]map[uint64]*subscriber
}

type Subscription struct {
	ch     <-chan domain.ArchitectEvent
	close  func()
	closed sync.Once
}

func New(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		logger:      logger,
		subscribers: map[string]map[uint64]*subscriber{},
	}
}

func (h *Hub) Subscribe(key string) *Subscription {
	sub := &subscriber{ch: make(chan domain.ArchitectEvent, subscriberBuffer)}

	h.mu.Lock()
	h.nextID++
	subID := h.nextID
	subs := h.subscribers[key]
	if subs == nil {
		subs = map[uint64]*subscriber{}
		h.subscribers[key] = subs
	}
	subs[subID] = sub
	h.mu.Unlock()

	out := &Subscription{ch: sub.ch}
	out.close = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.closeSubLocked(key, subID)
	}
	return out
}

// closeSubLocked closes and removes a subscriber's channel. The caller must hold
// h.mu for writing. It is idempotent: closing the channel exactly once (guarded
// by subscriber.closed) and a no-op once the subscriber has been removed, so the
// overflow path and the handler's deferred Close never double-close.
func (h *Hub) closeSubLocked(key string, subID uint64) {
	subs := h.subscribers[key]
	if subs == nil {
		return
	}
	sub := subs[subID]
	if sub == nil {
		return
	}
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
	delete(subs, subID)
	if len(subs) == 0 {
		delete(h.subscribers, key)
	}
}

func (h *Hub) Publish(key string, event domain.ArchitectEvent) {
	h.mu.RLock()
	count := len(h.subscribers[key])
	var overflowed []uint64
	for id, sub := range h.subscribers[key] {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			overflowed = append(overflowed, id)
		}
	}
	h.mu.RUnlock()

	h.logger.Debug("[archevents] publish",
		"key", key,
		"type", event.Type,
		"reason", event.Reason,
		"ticket_id", event.TicketID,
		"subscribers", count,
	)

	if len(overflowed) == 0 {
		return
	}

	// A full buffer means the consumer can't keep up. Rather than silently drop
	// the event, close the subscriber: the SSE handler returns on a closed
	// channel, ending the HTTP stream, so the desktop client reconnects and
	// reconciles board state — recovering whatever it would have missed.
	h.mu.Lock()
	for _, id := range overflowed {
		h.logger.Warn("[archevents] subscriber overflow, closing",
			"key", key,
			"reason", event.Reason,
			"ticket_id", event.TicketID,
			"sub_id", id,
		)
		h.closeSubLocked(key, id)
	}
	h.mu.Unlock()
}

func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, subs := range h.subscribers {
		for id, sub := range subs {
			if !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
			delete(subs, id)
		}
		delete(h.subscribers, key)
	}
}

func (s *Subscription) C() <-chan domain.ArchitectEvent {
	return s.ch
}

func (s *Subscription) Close() {
	s.closed.Do(func() {
		if s.close != nil {
			s.close()
		}
	})
}
