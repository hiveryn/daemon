package archevents

import (
	"sync"

	"github.com/hiveryn/daemon/internal/domain"
)

type Hub struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[string]map[uint64]chan domain.ArchitectEvent
}

type Subscription struct {
	ch     <-chan domain.ArchitectEvent
	close  func()
	closed sync.Once
}

func New() *Hub {
	return &Hub{
		subscribers: map[string]map[uint64]chan domain.ArchitectEvent{},
	}
}

func (h *Hub) Subscribe(key string) *Subscription {
	ch := make(chan domain.ArchitectEvent, 16)

	h.mu.Lock()
	h.nextID++
	subID := h.nextID
	subs := h.subscribers[key]
	if subs == nil {
		subs = map[uint64]chan domain.ArchitectEvent{}
		h.subscribers[key] = subs
	}
	subs[subID] = ch
	h.mu.Unlock()

	sub := &Subscription{
		ch: ch,
	}
	sub.close = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		subs := h.subscribers[key]
		if subs == nil {
			return
		}
		if existing := subs[subID]; existing != nil {
			delete(subs, subID)
			close(existing)
		}
		if len(subs) == 0 {
			delete(h.subscribers, key)
		}
	}
	return sub
}

func (h *Hub) Publish(key string, event domain.ArchitectEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subscribers[key] {
		select {
		case ch <- event:
		default:
		}
	}
}

func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, subs := range h.subscribers {
		for id, ch := range subs {
			delete(subs, id)
			close(ch)
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
