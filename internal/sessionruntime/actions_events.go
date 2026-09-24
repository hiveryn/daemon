package sessionruntime

import (
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

const actionEventBuffer = 64

// actionEventHub fans action execution changes out to the desktop's actions
// stream. Like the architect stream it has no backlog, and a subscriber that
// cannot keep up is closed rather than silently skipped, so its client
// reconnects and refetches.
type actionEventHub struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]chan domain.ActionEvent
}

type actionEventSubscription struct {
	ch    <-chan domain.ActionEvent
	close func()
	once  sync.Once
}

func (s *actionEventSubscription) C() <-chan domain.ActionEvent { return s.ch }

func (s *actionEventSubscription) Close() {
	s.once.Do(s.close)
}

// SubscribeActionEvents subscribes to execution status changes.
func (s *Service) SubscribeActionEvents() domain.ActionEventSubscription {
	h := &s.actionEvents
	ch := make(chan domain.ActionEvent, actionEventBuffer)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[uint64]chan domain.ActionEvent{}
	}
	h.nextID++
	id := h.nextID
	h.subs[id] = ch
	h.mu.Unlock()
	return &actionEventSubscription{ch: ch, close: func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if existing, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(existing)
		}
	}}
}

func (s *Service) publishActionEvent(run domain.ActionRun, status domain.ActionRunStatus) {
	event := domain.ActionEvent{
		Type:        domain.ActionEventType,
		Action:      run.Action,
		ExecutionID: run.ID,
		Status:      status,
		SessionID:   run.SessionID,
		At:          time.Now().UTC(),
	}
	h := &s.actionEvents
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		select {
		case ch <- event:
		default:
			s.logger.Warn("[actions] event subscriber overflow, closing", "sub_id", id)
			delete(h.subs, id)
			close(ch)
		}
	}
}

func (s *Service) closeActionEventSubscribers() {
	h := &s.actionEvents
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}
