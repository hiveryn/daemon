package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/agentruntime/adapter/claude"
	artcodex "github.com/hiveryn/agentruntime/adapter/codex"
	"github.com/hiveryn/agentruntime/adapter/opencode"
	"github.com/hiveryn/agentruntime/ingest"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

const (
	ingestPathPrefix = "/internal/agentruntime"
	setupMarker      = "hiveryn-daemon"
	defaultPTYCols   = 80
	defaultPTYRows   = 24
)

type Service struct {
	logger     *slog.Logger
	cfg        config.Config
	repo       domain.SessionRepository
	baseURL    string
	receiver   *ingest.Receiver
	ingestHTTP http.Handler

	adapters map[agentruntime.AgentKind]agentruntime.Adapter
	terminal terminalManager

	eventMu      sync.RWMutex
	eventNextID  uint64
	eventStreams map[string]map[uint64]chan domain.SessionEvent

	bridgeMu      sync.Mutex
	bridgeCancels map[string]func()
}

type eventSubscription struct {
	ch     <-chan domain.SessionEvent
	close  func()
	closed sync.Once
}

func New(ctx context.Context, cfg config.Config, repo domain.SessionRepository, logger *slog.Logger, baseURL string) (*Service, error) {
	claudeAdapter := claude.New(claude.DefaultOptions())
	codexAdapter := artcodex.New(artcodex.DefaultOptions())
	openCodeAdapter := opencode.New(opencode.DefaultOptions())

	adapters := map[agentruntime.AgentKind]agentruntime.Adapter{
		agentruntime.AgentClaude:   claudeAdapter,
		agentruntime.AgentCodex:    codexAdapter,
		agentruntime.AgentOpenCode: openCodeAdapter,
	}
	receiver := ingest.NewReceiver(claudeAdapter, codexAdapter, openCodeAdapter)

	mux := http.NewServeMux()
	mux.Handle("/claude", receiver.Handler(agentruntime.AgentClaude))
	mux.Handle("/codex", receiver.Handler(agentruntime.AgentCodex))
	mux.Handle("/opencode", receiver.Handler(agentruntime.AgentOpenCode))

	return &Service{
		logger:        logger,
		cfg:           cfg,
		repo:          repo,
		baseURL:       baseURL,
		receiver:      receiver,
		ingestHTTP:    http.StripPrefix(ingestPathPrefix, mux),
		adapters:      adapters,
		terminal:      newPTYTerminalManager(logger),
		eventStreams:  map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels: map[string]func(){},
	}, nil
}

func (s *Service) IngestHandler() http.Handler {
	return s.ingestHTTP
}

func (s *Service) SpawnArchitectSession(ctx context.Context, req domain.SpawnArchitectSessionRequest) (domain.SpawnArchitectSessionResult, error) {
	architect, ok := s.cfg.Architects[req.ArchitectKey]
	if !ok {
		return domain.SpawnArchitectSessionResult{}, &domain.NotFoundError{Resource: "architect", ID: req.ArchitectKey}
	}

	profile, ok := s.cfg.AgentProfiles[req.ProfileName]
	if !ok {
		return domain.SpawnArchitectSessionResult{}, &domain.NotFoundError{Resource: "agent_profile", ID: req.ProfileName}
	}

	agentKind, err := parseAgentKind(profile.Agent)
	if err != nil {
		return domain.SpawnArchitectSessionResult{}, err
	}

	systemContent, kickoffContent, err := loadArchitectPrompts(req.ArchitectKey, architect, s.cfg)
	if err != nil {
		return domain.SpawnArchitectSessionResult{}, err
	}

	sessionID := uuid.NewString()
	session, err := s.repo.CreateSession(ctx, domain.CreateSessionParams{
		ID:           sessionID,
		ProfileName:  req.ProfileName,
		ArchitectKey: req.ArchitectKey,
		Prompt:       kickoffContent,
		Instructions: systemContent,
		Status:       domain.SessionStatusRunning,
	})
	if err != nil {
		return domain.SpawnArchitectSessionResult{}, err
	}

	adapter := s.adapters[agentKind]
	if _, err := adapter.EnsureSetup(ctx, setupRequestForAgent(agentKind, s.baseURL+ingestPathPrefix, profile.Env)); err != nil {
		s.markSessionFailed(session.ID, "ensure setup")
		return domain.SpawnArchitectSessionResult{}, fmt.Errorf("ensure %s setup: %w", agentKind, err)
	}
	spec, err := adapter.PrepareLaunch(ctx, agentruntime.StartRequest{
		ID:           session.ID,
		Agent:        agentKind,
		Prompt:       kickoffContent,
		Instructions: systemContent,
		Workdir:      architect.Path,
		Args:         append([]string(nil), profile.Args...),
		Env:          cloneStringMap(profile.Env),
	})
	if err != nil {
		s.markSessionFailed(session.ID, "prepare launch")
		return domain.SpawnArchitectSessionResult{}, fmt.Errorf("prepare launch: %w", err)
	}

	cancelBridge := s.startReceiverBridge(session.ID)
	s.storeBridgeCancel(session.ID, cancelBridge)

	if err := s.terminal.Start(ctx, terminalStartSpec{
		ID:           session.ID,
		Command:      spec.Command,
		Args:         append([]string(nil), spec.Args...),
		Env:          cloneStringMap(spec.Env),
		Workdir:      spec.Workdir,
		Size:         terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows},
		CleanupPaths: append([]string(nil), spec.CleanupPaths...),
		OnExit:       s.handleTerminalExit,
	}); err != nil {
		s.cancelReceiverBridge(session.ID)
		s.markSessionFailed(session.ID, "terminal start")
		return domain.SpawnArchitectSessionResult{}, err
	}

	return domain.SpawnArchitectSessionResult{Session: session}, nil
}

func setupRequestForAgent(agentKind agentruntime.AgentKind, endpoint string, env map[string]string) agentruntime.SetupRequest {
	return agentruntime.SetupRequest{
		Marker:     setupMarker,
		ConfigRoot: configRootForAgent(agentKind, env),
		Hook:       hookCommandForAgent(agentKind, endpoint),
	}
}

func configRootForAgent(agentKind agentruntime.AgentKind, env map[string]string) string {
	switch agentKind {
	case agentruntime.AgentCodex:
		return env["CODEX_HOME"]
	case agentruntime.AgentClaude:
		return ""
	case agentruntime.AgentOpenCode:
		return ""
	default:
		return ""
	}
}

func hookCommandForAgent(agentKind agentruntime.AgentKind, endpoint string) agentruntime.HookCommand {
	switch agentKind {
	case agentruntime.AgentClaude:
		return claude.HookCommand(endpoint)
	case agentruntime.AgentCodex:
		return artcodex.HookCommand(endpoint)
	case agentruntime.AgentOpenCode:
		return agentruntime.HookCommand{Endpoint: endpoint}
	default:
		return agentruntime.HookCommand{Endpoint: endpoint}
	}
}

func (s *Service) TerminateSession(ctx context.Context, id string) error {
	if _, err := s.repo.GetSession(ctx, id); err != nil {
		return err
	}

	if err := s.terminal.Kill(ctx, id); err != nil && !errors.Is(err, errTerminalNotFound) {
		return err
	}

	s.cancelReceiverBridge(id)
	s.closeEventSubscribers(id)
	return s.repo.DeleteSession(ctx, id)
}

func (s *Service) GetSession(ctx context.Context, id string) (domain.Session, error) {
	return s.repo.GetSession(ctx, id)
}

func (s *Service) ListSessions(ctx context.Context, filter domain.SessionListFilter) ([]domain.Session, error) {
	return s.repo.ListSessions(ctx, filter)
}

func (s *Service) ListSessionEvents(ctx context.Context, id string) ([]domain.SessionEvent, error) {
	return s.repo.ListSessionEvents(ctx, id)
}

func (s *Service) SubscribeSessionEvents(ctx context.Context, id string) (domain.SessionEventSubscription, error) {
	if _, err := s.repo.GetSession(ctx, id); err != nil {
		return nil, err
	}

	ch := make(chan domain.SessionEvent, 32)

	s.eventMu.Lock()
	s.eventNextID++
	subID := s.eventNextID
	subs := s.eventStreams[id]
	if subs == nil {
		subs = map[uint64]chan domain.SessionEvent{}
		s.eventStreams[id] = subs
	}
	subs[subID] = ch
	s.eventMu.Unlock()

	return &eventSubscription{
		ch: ch,
		close: func() {
			s.eventMu.Lock()
			defer s.eventMu.Unlock()
			subs := s.eventStreams[id]
			if subs == nil {
				return
			}
			if existing := subs[subID]; existing != nil {
				delete(subs, subID)
				close(existing)
			}
			if len(subs) == 0 {
				delete(s.eventStreams, id)
			}
		},
	}, nil
}

func (s *Service) AttachTerminal(ctx context.Context, id string) (domain.TerminalAttachment, error) {
	if _, err := s.repo.GetSession(ctx, id); err != nil {
		return nil, err
	}
	return s.terminal.Attach(ctx, id)
}

func (s *Service) FailRunningSessions(ctx context.Context) error {
	return s.repo.FailRunningSessions(ctx)
}

func (s *Service) Shutdown(ctx context.Context) error {
	terminalErr := s.terminal.Shutdown(ctx)
	s.cancelReceiverBridges()
	s.closeEventSubscribers("")
	if err := s.repo.FailRunningSessions(ctx); err != nil {
		return err
	}
	return terminalErr
}

func (s *Service) startReceiverBridge(sessionID string) func() {
	sub := s.receiver.Hub().Subscribe(ingest.Filter{ID: sessionID})
	done := make(chan struct{})
	var once sync.Once

	go func() {
		defer sub.Close()
		for {
			select {
			case <-done:
				return
			case event, ok := <-sub.Events:
				if !ok {
					return
				}
				s.handleReceiverEvent(event)
			}
		}
	}()

	return func() {
		once.Do(func() {
			close(done)
			sub.Close()
		})
	}
}

func (s *Service) handleReceiverEvent(event agentruntime.Event) {
	persisted, err := s.repo.AppendSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionID:         event.ID,
		Type:              "status",
		Status:            string(event.Status),
		Tool:              event.Tool,
		Message:           event.Message,
		NativeID:          event.NativeID,
		PrimaryNativeID:   event.PrimaryNativeID,
		NativeSessionRole: string(event.NativeSessionRole),
		Metadata:          cloneStringMap(event.Metadata),
		Raw:               cloneAnyMap(event.Raw),
		At:                event.At,
	})
	if err != nil {
		s.logger.Error("persist session event failed", "session_id", event.ID, "error", err)
		return
	}

	primaryNativeID := event.PrimaryNativeID
	if primaryNativeID == "" {
		primaryNativeID = event.NativeID
	}
	if primaryNativeID != "" {
		if err := s.repo.UpdateSessionNativeID(context.Background(), event.ID, primaryNativeID); err != nil {
			s.logger.Warn("update session native id failed", "session_id", event.ID, "error", err)
		}
	}

	s.publishEvent(event.ID, persisted)
}

func (s *Service) handleTerminalExit(exit terminalExit) {
	defer s.cancelReceiverBridge(exit.ID)

	status := domain.SessionStatusCompleted
	message := "process exited successfully"
	if exit.Err != nil {
		status = domain.SessionStatusFailed
		message = exit.Err.Error()
	}

	if err := s.repo.UpdateSessionStatus(context.Background(), exit.ID, status); err != nil {
		s.logger.Error("update session status failed", "session_id", exit.ID, "error", err)
		return
	}

	event, err := s.repo.AppendSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionID: exit.ID,
		Type:      "status",
		Status:    "ended",
		Message:   message,
		At:        time.Now().UTC(),
	})
	if err != nil {
		s.logger.Warn("append process exit event failed", "session_id", exit.ID, "error", err)
		return
	}
	s.publishEvent(exit.ID, event)
}

func (s *Service) markSessionFailed(sessionID, stage string) {
	if err := s.repo.UpdateSessionStatus(context.Background(), sessionID, domain.SessionStatusFailed); err != nil {
		s.logger.Error("mark failed session", "session_id", sessionID, "stage", stage, "error", err)
	}
}

func (s *Service) publishEvent(sessionID string, event domain.SessionEvent) {
	s.eventMu.RLock()
	defer s.eventMu.RUnlock()
	for _, ch := range s.eventStreams[sessionID] {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Service) closeEventSubscribers(sessionID string) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if sessionID != "" {
		s.closeEventSubscribersLocked(sessionID)
		return
	}
	for id := range s.eventStreams {
		s.closeEventSubscribersLocked(id)
	}
}

func (s *Service) closeEventSubscribersLocked(sessionID string) {
	subs := s.eventStreams[sessionID]
	for id, ch := range subs {
		delete(subs, id)
		close(ch)
	}
	delete(s.eventStreams, sessionID)
}

func (s *Service) storeBridgeCancel(sessionID string, cancel func()) {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	s.bridgeCancels[sessionID] = cancel
}

func (s *Service) cancelReceiverBridge(sessionID string) {
	s.bridgeMu.Lock()
	cancel := s.bridgeCancels[sessionID]
	delete(s.bridgeCancels, sessionID)
	s.bridgeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) cancelReceiverBridges() {
	s.bridgeMu.Lock()
	cancels := make([]func(), 0, len(s.bridgeCancels))
	for id, cancel := range s.bridgeCancels {
		delete(s.bridgeCancels, id)
		cancels = append(cancels, cancel)
	}
	s.bridgeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *eventSubscription) C() <-chan domain.SessionEvent {
	return s.ch
}

func (s *eventSubscription) Close() {
	s.closed.Do(func() {
		if s.close != nil {
			s.close()
		}
	})
}

func parseAgentKind(value string) (agentruntime.AgentKind, error) {
	switch strings.TrimSpace(value) {
	case string(agentruntime.AgentClaude):
		return agentruntime.AgentClaude, nil
	case string(agentruntime.AgentCodex):
		return agentruntime.AgentCodex, nil
	case string(agentruntime.AgentOpenCode):
		return agentruntime.AgentOpenCode, nil
	default:
		return "", &domain.ValidationError{Field: "profile_name", Message: "uses unsupported agent " + value}
	}
}

func configKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneAnyMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}
