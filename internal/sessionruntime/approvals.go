package sessionruntime

import (
	"fmt"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

type pendingApproval struct {
	params    domain.ConcludeSessionParams
	resultCh  chan approvalResult
	createdAt time.Time
}

type approvalResult struct {
	concludeResult domain.ConcludeSessionResult
	err            error
}

type approvalStore struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func newApprovalStore() *approvalStore {
	return &approvalStore{
		pending: map[string]*pendingApproval{},
	}
}

func (s *approvalStore) Store(id string, params domain.ConcludeSessionParams) (chan approvalResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing := s.pending[id]; existing != nil {
		return nil, &domain.ConflictError{
			Resource: "pending_approval",
			Field:    "session_id",
			Message:  fmt.Sprintf("session %s already has a pending conclusion approval", id),
		}
	}

	ch := make(chan approvalResult, 1)
	s.pending[id] = &pendingApproval{
		params:    params,
		resultCh:  ch,
		createdAt: time.Now().UTC(),
	}
	return ch, nil
}

func (s *approvalStore) Claim(id string) (*pendingApproval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pending, ok := s.pending[id]
	if !ok {
		return nil, false
	}
	delete(s.pending, id)
	return pending, true
}

func (s *approvalStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}
