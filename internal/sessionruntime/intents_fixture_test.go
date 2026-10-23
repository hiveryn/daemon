package sessionruntime

import (
	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"io"
	"log/slog"
	"testing"
)

func newIntentTestService(t *testing.T) (*Service, *fakeSessionRepository, string) {
	t.Helper()

	architectPath := t.TempDir()
	repo := newFakeSessionRepository()
	repo.createdSession = domain.Session{
		ID:           "session-1",
		ArchitectKey: "hiveryn",
		SessionType:  domain.SessionTypeArchitect,
		CurrentRun:   &domain.SessionRun{ID: "run-1", Status: domain.SessionRunStatusRunning},
	}

	service := &Service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		cfg: config.Config{
			IntentWaitTimeout: 20,
			Architects: map[string]config.ArchitectConfig{
				"hiveryn": {Path: architectPath, Repos: map[string]string{"daemon": t.TempDir()}},
			},
		},
		tickets:      architectfs.NewTicketService(nil),
		intents:      newIntentStore(),
		eventStreams: map[string]map[uint64]chan domain.SessionEvent{},
	}
	return service, repo, architectPath
}
