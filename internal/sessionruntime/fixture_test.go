package sessionruntime

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/store"
)

// serviceFixture is a Service over a real SQLite store, a fake codex adapter
// and a fake terminal manager.
type serviceFixture struct {
	service  *Service
	sessions *store.SessionStore
	messages *store.AgentMessageStore
	adapter  *fakeAdapter
	terminal *fakeTerminalManager
	dbPath   string
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	f := &serviceFixture{dbPath: filepath.Join(t.TempDir(), "daemon.db")}
	f.open(t)
	return f
}

// open (re)builds the service over the fixture's database, as a daemon
// restart would.
func (f *serviceFixture) open(t *testing.T) {
	t.Helper()
	db, err := store.Open(context.Background(), f.dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f.sessions = store.NewSessionStore(db)
	f.adapter = &fakeAdapter{}
	f.terminal = &fakeTerminalManager{}
	f.service = &Service{
		intents:  newIntentStore(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:      config.Config{Variants: map[string]config.VariantConfig{"codex": {Agent: "codex"}}},
		repo:     f.sessions,
		receiver: ingest.NewReceiver(f.adapter),
		adapters: map[agentruntime.AgentKind]agentruntime.Adapter{
			agentruntime.AgentCodex: f.adapter,
		},
		terminal:       f.terminal,
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
		baseURL:        "http://127.0.0.1:4200",
		executablePath: func() (string, error) { return "/tmp/hiverynd", nil },
	}
	f.messages = store.NewAgentMessageStore(db)
	f.service.SetAgentMessageRepository(f.messages)
}

// architect configures an architect and records a running session for it.
func (f *serviceFixture) architect(t *testing.T, key string) domain.Session {
	t.Helper()
	if f.service.cfg.Architects == nil {
		f.service.cfg.Architects = map[string]config.ArchitectConfig{}
	}
	f.service.cfg.Architects[key] = config.ArchitectConfig{Name: key, Path: t.TempDir(), Repos: map[string]string{}}
	ctx := context.Background()
	session, err := f.sessions.CreateSession(ctx, domain.CreateSessionParams{
		ArchitectKey: key,
		SessionType:  domain.SessionTypeArchitect,
		ContextID:    "architect-" + key,
		Prompt:       "kickoff",
		Workdir:      t.TempDir(),
		CreatedBy:    domain.SessionCreatedByDesktop,
	})
	if err != nil {
		t.Fatalf("create architect session: %v", err)
	}
	if _, err := f.sessions.CreateRun(ctx, domain.CreateSessionRunParams{SessionID: session.ID, ProfileName: "codex", ProfileSnapshot: domain.AgentProfileSnapshot{Agent: "codex"}, Workdir: session.Workdir}); err != nil {
		t.Fatalf("create architect run: %v", err)
	}
	session, err = f.sessions.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
