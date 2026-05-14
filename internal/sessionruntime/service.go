package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"gopkg.in/yaml.v3"
)

const (
	ingestPathPrefix = "/internal/agentruntime"
	setupMarker      = "hiveryn-daemon"
	defaultPTYCols   = 80
	defaultPTYRows   = 24
)

type Service struct {
	logger         *slog.Logger
	cfg            config.Config
	repo           domain.SessionRepository
	tickets        domain.TicketService
	baseURL        string
	receiver       *ingest.Receiver
	ingestHTTP     http.Handler
	executablePath func() (string, error)

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

func New(ctx context.Context, cfg config.Config, repo domain.SessionRepository, tickets domain.TicketService, logger *slog.Logger, baseURL string) (*Service, error) {
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
		logger:         logger,
		cfg:            cfg,
		repo:           repo,
		tickets:        tickets,
		baseURL:        baseURL,
		receiver:       receiver,
		ingestHTTP:     http.StripPrefix(ingestPathPrefix, mux),
		executablePath: os.Executable,
		adapters:       adapters,
		terminal:       newPTYTerminalManager(logger),
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
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

	profile, ok := s.cfg.Variants[req.ProfileName]
	if !ok {
		return domain.SpawnArchitectSessionResult{}, &domain.NotFoundError{Resource: "agent_profile", ID: req.ProfileName}
	}

	agentKind, err := parseAgentKind(profile.Agent)
	if err != nil {
		return domain.SpawnArchitectSessionResult{}, err
	}

	systemContent, kickoffContent, err := loadArchitectPrompts(req.ArchitectKey, architect, s.cfg)
	if err != nil {
		s.logger.Error("failed to load architect prompts", "architect_key", req.ArchitectKey, "error", err)
		return domain.SpawnArchitectSessionResult{}, err
	}

	sessionID := uuid.NewString()
	session, err := s.repo.CreateSession(ctx, domain.CreateSessionParams{
		ID:           sessionID,
		ProfileName:  req.ProfileName,
		ArchitectKey: req.ArchitectKey,
		SessionType:  string(domain.SessionTypeArchitect),
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
	mcpServers, err := s.mcpServersForSession(domain.SessionTypeArchitect, req.ArchitectKey, session.ID)
	if err != nil {
		s.markSessionFailed(session.ID, "resolve mcp server")
		return domain.SpawnArchitectSessionResult{}, err
	}
	spec, err := adapter.PrepareLaunch(ctx, agentruntime.StartRequest{
		ID:           session.ID,
		Agent:        agentKind,
		Prompt:       kickoffContent,
		Instructions: systemContent,
		Workdir:      architect.Path,
		Args:         append([]string(nil), profile.Args...),
		Env:          cloneStringMap(profile.Env),
		MCPServers:   mcpServers,
	})
	if err != nil {
		s.markSessionFailed(session.ID, "prepare launch")
		return domain.SpawnArchitectSessionResult{}, fmt.Errorf("prepare launch: %w", err)
	}

	cancelBridge := s.startReceiverBridge(session.ID)
	s.storeBridgeCancel(session.ID, cancelBridge)

	s.logger.Info("[spawn] starting PTY",
		"session_id", session.ID,
		"requested_cols", req.Cols,
		"requested_rows", req.Rows,
		"command", spec.Command,
	)

	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID:    session.ID,
		Name:         "main",
		Command:      spec.Command,
		Args:         append([]string(nil), spec.Args...),
		Env:          cloneStringMap(spec.Env),
		Workdir:      spec.Workdir,
		Size:         terminalSize{Cols: req.Cols, Rows: req.Rows},
		CleanupPaths: append([]string(nil), spec.CleanupPaths...),
		OnExit:       s.handleTerminalExit,
	}); err != nil {
		s.cancelReceiverBridge(session.ID)
		s.markSessionFailed(session.ID, "terminal start")
		return domain.SpawnArchitectSessionResult{}, err
	}

	return domain.SpawnArchitectSessionResult{Session: session}, nil
}

func (s *Service) SpawnWorkSession(ctx context.Context, req domain.SpawnWorkSessionRequest) (domain.SpawnWorkSessionResult, error) {
	architect, ok := s.cfg.Architects[req.ArchitectKey]
	if !ok {
		return domain.SpawnWorkSessionResult{}, &domain.NotFoundError{Resource: "architect", ID: req.ArchitectKey}
	}

	ticket, err := s.tickets.GetTicket(ctx, architect.Path, req.TicketID)
	if err != nil {
		return domain.SpawnWorkSessionResult{}, err
	}

	if ticket.Status != domain.TicketStatusBacklog {
		return domain.SpawnWorkSessionResult{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog to spawn a work session"}
	}

	repoKey := ticket.Repo
	if repoKey == "" {
		return domain.SpawnWorkSessionResult{}, &domain.ValidationError{Field: "repo", Message: "ticket has no repo key in frontmatter"}
	}

	repoPath, ok := architect.Repos[repoKey]
	if !ok {
		return domain.SpawnWorkSessionResult{}, &domain.ValidationError{Field: "repo", Message: "repo key " + repoKey + " not configured in architect repos"}
	}

	if err := validateRepoPath(repoPath); err != nil {
		return domain.SpawnWorkSessionResult{}, err
	}

	if req.Mode != "" && req.Mode != "normal" {
		return domain.SpawnWorkSessionResult{}, &domain.ValidationError{Field: "mode", Message: "only 'normal' mode is supported"}
	}

	profile, ok := s.cfg.Variants[req.ProfileName]
	if !ok {
		return domain.SpawnWorkSessionResult{}, &domain.NotFoundError{Resource: "agent_profile", ID: req.ProfileName}
	}

	agentKind, err := parseAgentKind(profile.Agent)
	if err != nil {
		return domain.SpawnWorkSessionResult{}, err
	}

	kickoffContent, err := loadWorkerPrompt(req.ArchitectKey, architect, s.cfg, ticket)
	if err != nil {
		s.logger.Error("failed to load worker prompt", "architect_key", req.ArchitectKey, "ticket_id", req.TicketID, "error", err)
		return domain.SpawnWorkSessionResult{}, err
	}

	sessionID := uuid.NewString()
	session, err := s.repo.CreateSession(ctx, domain.CreateSessionParams{
		ID:           sessionID,
		ProfileName:  req.ProfileName,
		ArchitectKey: req.ArchitectKey,
		SessionType:  string(domain.SessionTypeWork),
		Prompt:       kickoffContent,
		Status:       domain.SessionStatusRunning,
		TicketID:     req.TicketID,
	})
	if err != nil {
		return domain.SpawnWorkSessionResult{}, err
	}

	adapter := s.adapters[agentKind]
	if _, err := adapter.EnsureSetup(ctx, setupRequestForAgent(agentKind, s.baseURL+ingestPathPrefix, profile.Env)); err != nil {
		s.markSessionFailed(session.ID, "ensure setup")
		return domain.SpawnWorkSessionResult{}, fmt.Errorf("ensure %s setup: %w", agentKind, err)
	}
	mcpServers, err := s.mcpServersForSession(domain.SessionTypeWork, req.ArchitectKey, session.ID)
	if err != nil {
		s.markSessionFailed(session.ID, "resolve mcp server")
		return domain.SpawnWorkSessionResult{}, err
	}

	spec, err := adapter.PrepareLaunch(ctx, agentruntime.StartRequest{
		ID:         session.ID,
		Agent:      agentKind,
		Prompt:     kickoffContent,
		Workdir:    repoPath,
		Args:       append([]string(nil), profile.Args...),
		Env:        cloneStringMap(profile.Env),
		MCPServers: mcpServers,
	})
	if err != nil {
		s.markSessionFailed(session.ID, "prepare launch")
		return domain.SpawnWorkSessionResult{}, fmt.Errorf("prepare launch: %w", err)
	}

	cancelBridge := s.startReceiverBridge(session.ID)
	s.storeBridgeCancel(session.ID, cancelBridge)

	s.logger.Info("[spawn] starting worker PTY",
		"session_id", session.ID,
		"ticket_id", req.TicketID,
		"repo_path", repoPath,
		"command", spec.Command,
	)

	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID:    session.ID,
		Name:         "main",
		Command:      spec.Command,
		Args:         append([]string(nil), spec.Args...),
		Env:          cloneStringMap(spec.Env),
		Workdir:      spec.Workdir,
		Size:         terminalSize{Cols: req.Cols, Rows: req.Rows},
		CleanupPaths: append([]string(nil), spec.CleanupPaths...),
		OnExit:       s.handleTerminalExit,
	}); err != nil {
		s.cancelReceiverBridge(session.ID)
		s.markSessionFailed(session.ID, "terminal start")
		return domain.SpawnWorkSessionResult{}, err
	}

	if _, err := s.tickets.MoveTicket(ctx, architect.Path, req.TicketID, domain.MoveTicketParams{
		To: domain.TicketStatusProgress,
	}); err != nil {
		s.logger.Error("failed to move ticket to progress", "ticket_id", req.TicketID, "error", err)
	}

	return domain.SpawnWorkSessionResult{Session: session}, nil
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

func (s *Service) ConcludeSession(ctx context.Context, id string, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	if strings.TrimSpace(params.Body) == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "body", Message: "is required"}
	}

	session, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if session.Status != domain.SessionStatusRunning {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "session is not running"}
	}

	switch session.SessionType {
	case string(domain.SessionTypeArchitect):
		return s.concludeArchitectSession(ctx, session, params)
	case string(domain.SessionTypeWork):
		return s.concludeWorkSession(ctx, session, params)
	default:
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_type", Message: "cannot conclude session of type " + session.SessionType}
	}
}

func (s *Service) ReadConclusion(ctx context.Context, architectKey, id string) (domain.ArchitectConclusion, error) {
	architect, ok := s.cfg.Architects[architectKey]
	if !ok {
		return domain.ArchitectConclusion{}, &domain.NotFoundError{Resource: "architect", ID: architectKey}
	}

	conclusionPath := filepath.Join(architect.Path, "architect-sessions", id, conclusionFileName)
	doc, err := architectfs.ReadMarkdownDocument(conclusionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.ArchitectConclusion{}, &domain.NotFoundError{Resource: "conclusion", ID: id}
		}
		return domain.ArchitectConclusion{}, fmt.Errorf("read conclusion file: %w", err)
	}

	return parseArchitectConclusion(doc)
}

func parseArchitectConclusion(doc architectfs.MarkdownDocument) (domain.ArchitectConclusion, error) {
	var result domain.ArchitectConclusion
	result.Body = doc.Body

	if doc.Metadata != nil {
		result.StartedAt = yamlNodeTime(doc.Metadata, "started_at")
		result.ConcludedAt = yamlNodeTime(doc.Metadata, "concluded_at")
		result.Agent = yamlNodeString(doc.Metadata, "agent")
	}

	return result, nil
}

func (s *Service) ReadRecentConclusion(ctx context.Context, architectKey string) (domain.ArchitectConclusion, error) {
	architect, ok := s.cfg.Architects[architectKey]
	if !ok {
		return domain.ArchitectConclusion{}, &domain.NotFoundError{Resource: "architect", ID: architectKey}
	}

	sessionsDir := filepath.Join(architect.Path, "architect-sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.ArchitectConclusion{}, &domain.NotFoundError{Resource: "conclusion", ID: "recent"}
		}
		return domain.ArchitectConclusion{}, fmt.Errorf("read architect-sessions directory: %w", err)
	}

	var bestEntry string
	var bestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		conclusionPath := filepath.Join(sessionsDir, entry.Name(), conclusionFileName)
		doc, err := architectfs.ReadMarkdownDocument(conclusionPath)
		if err != nil {
			continue
		}
		var concludedAt time.Time
		if doc.Metadata != nil {
			concludedAt = yamlNodeTime(doc.Metadata, "concluded_at")
		}
		if concludedAt.After(bestTime) {
			bestTime = concludedAt
			bestEntry = entry.Name()
		}
	}

	if bestEntry == "" {
		return domain.ArchitectConclusion{}, &domain.NotFoundError{Resource: "conclusion", ID: "recent"}
	}

	return s.ReadConclusion(ctx, architectKey, bestEntry)
}

func (s *Service) ListConclusions(ctx context.Context, architectKey string, limit int) ([]domain.ConclusionSummary, error) {
	architect, ok := s.cfg.Architects[architectKey]
	if !ok {
		return nil, &domain.NotFoundError{Resource: "architect", ID: architectKey}
	}

	if limit <= 0 {
		limit = 10
	}

	sessionsDir := filepath.Join(architect.Path, "architect-sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read architect-sessions directory: %w", err)
	}

	var summaries []domain.ConclusionSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		conclusionPath := filepath.Join(sessionsDir, entry.Name(), conclusionFileName)
		doc, err := architectfs.ReadMarkdownDocument(conclusionPath)
		if err != nil {
			continue
		}
		var concludedAt time.Time
		if doc.Metadata != nil {
			concludedAt = yamlNodeTime(doc.Metadata, "concluded_at")
		}
		summaries = append(summaries, domain.ConclusionSummary{
			ID:          entry.Name(),
			ConcludedAt: concludedAt,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].ConcludedAt.After(summaries[j].ConcludedAt)
	})

	if len(summaries) > limit {
		summaries = summaries[:limit]
	}

	return summaries, nil
}

func (s *Service) concludeArchitectSession(ctx context.Context, session domain.Session, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	architect, ok := s.cfg.Architects[session.ArchitectKey]
	if !ok {
		return domain.ConcludeSessionResult{}, &domain.NotFoundError{Resource: "architect", ID: session.ArchitectKey}
	}

	folderName := session.CreatedAt.UTC().Format("2006-01-02-1504")
	dir := filepath.Join(architect.Path, "architect-sessions", folderName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return domain.ConcludeSessionResult{}, fmt.Errorf("create architect session directory: %w", err)
	}

	now := time.Now().UTC()
	conclusionPath := filepath.Join(dir, conclusionFileName)
	doc := newArchitectConclusionDocument(session.CreatedAt, now, session.ProfileName, params.Body)
	if err := writeConclusionFile(conclusionPath, doc); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.repo.EndSession(ctx, session.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	_ = s.terminal.KillBySession(ctx, session.ID)

	s.appendAndPublishSessionEnded(ctx, session.ID, "session concluded", map[string]any{"body": params.Body})

	return domain.ConcludeSessionResult{SessionID: session.ID}, nil
}

func (s *Service) concludeWorkSession(ctx context.Context, session domain.Session, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	if params.Rejected {
		if strings.TrimSpace(params.RejectionReason) == "" {
			return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "rejection_reason", Message: "is required when rejected is true"}
		}
	} else {
		if len(params.Commits) == 0 {
			return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "commits", Message: "are required when not rejected"}
		}
	}

	if session.TicketID == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "work session has no ticket ID"}
	}

	architect, ok := s.cfg.Architects[session.ArchitectKey]
	if !ok {
		return domain.ConcludeSessionResult{}, &domain.NotFoundError{Resource: "architect", ID: session.ArchitectKey}
	}

	ticket, err := s.tickets.GetTicket(ctx, architect.Path, session.TicketID)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if ticket.Status != domain.TicketStatusProgress {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in progress to conclude"}
	}

	repoKey := ticket.Repo
	if repoKey == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "repo", Message: "ticket has no repo key"}
	}
	repoPath, ok := architect.Repos[repoKey]
	if !ok {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "repo", Message: "repo key " + repoKey + " not configured in architect repos"}
	}

	for _, sha := range params.Commits {
		sha = strings.TrimSpace(sha)
		if sha == "" {
			return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "commits", Message: "commit SHA cannot be empty"}
		}
		if err := validateCommitSHA(ctx, repoPath, sha); err != nil {
			return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "commits", Message: "commit " + sha + " not found in repo " + repoKey + ": " + err.Error()}
		}
	}

	now := time.Now().UTC()
	conclusion := domain.TicketConclusion{
		StartedAt:       session.CreatedAt,
		ConcludedAt:     now,
		Agent:           session.ProfileName,
		Profile:         session.ProfileName,
		Rejected:        params.Rejected,
		RejectionReason: params.RejectionReason,
		Commits:         params.Commits,
		Body:            params.Body,
	}

	if _, err := s.tickets.ConcludeTicket(ctx, architect.Path, session.TicketID, conclusion); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.repo.EndSession(ctx, session.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	_ = s.terminal.KillBySession(ctx, session.ID)

	s.appendAndPublishSessionEnded(ctx, session.ID, "session concluded", map[string]any{
		"body":             params.Body,
		"commits":          params.Commits,
		"rejected":         params.Rejected,
		"rejection_reason": params.RejectionReason,
	})

	return domain.ConcludeSessionResult{SessionID: session.ID, TicketID: session.TicketID}, nil
}

func (s *Service) appendAndPublishSessionEnded(ctx context.Context, sessionID, message string, raw map[string]any) {
	event, err := s.repo.AppendSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: sessionID,
		Type:      "status",
		Status:    "ended",
		Message:   message,
		Raw:       raw,
		At:        time.Now().UTC(),
	})
	if err != nil {
		s.logger.Warn("append session ended event failed", "session_id", sessionID, "error", err)
		return
	}
	s.publishEvent(sessionID, event)
}

func validateCommitSHA(ctx context.Context, repoPath, sha string) error {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-t", sha)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git cat-file -t %s failed: %w", sha, err)
	}
	objType := strings.TrimSpace(string(output))
	if objType != "commit" {
		return fmt.Errorf("expected commit type, got %s", objType)
	}
	return nil
}

const conclusionFileName = "conclusion.md"

func newArchitectConclusionDocument(startedAt, concludedAt time.Time, agent, body string) architectfs.MarkdownDocument {
	return architectfs.NewArchitectConclusion(startedAt, concludedAt, agent, body)
}

func writeConclusionFile(path string, doc architectfs.MarkdownDocument) error {
	content, err := architectfs.RenderMarkdownDocument(doc)
	if err != nil {
		return fmt.Errorf("render conclusion: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write conclusion: %w", err)
	}
	return nil
}

func (s *Service) TerminateSession(ctx context.Context, id string) error {
	if _, err := s.repo.GetSession(ctx, id); err != nil {
		return err
	}

	if err := s.terminal.KillBySession(ctx, id); err != nil && !errors.Is(err, errTerminalNotFound) {
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

func (s *Service) AttachTerminal(ctx context.Context, sessionID, name string) (domain.TerminalAttachment, error) {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.terminal.Attach(ctx, sessionID, name)
}

func (s *Service) CreateTerminal(ctx context.Context, sessionID string, params domain.CreateTerminalParams) (domain.TerminalInfo, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.TerminalInfo{}, err
	}
	if session.Status != domain.SessionStatusRunning {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "session_id", Message: "session is not running"}
	}
	if strings.TrimSpace(params.Name) == "" {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "name", Message: "is required"}
	}
	if strings.TrimSpace(params.Command) == "" {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "command", Message: "is required"}
	}

	architect, ok := s.cfg.Architects[session.ArchitectKey]
	if !ok {
		return domain.TerminalInfo{}, &domain.NotFoundError{Resource: "architect", ID: session.ArchitectKey}
	}

	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID: sessionID,
		Name:      params.Name,
		Command:   params.Command,
		Args:      params.Args,
		Workdir:   architect.Path,
		Size:      terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows},
	}); err != nil {
		return domain.TerminalInfo{}, err
	}

	return domain.TerminalInfo{
		Name:      params.Name,
		SessionID: sessionID,
		Status:    "running",
	}, nil
}

func (s *Service) ListTerminals(ctx context.Context, sessionID string) ([]domain.TerminalInfo, error) {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.terminal.ListBySession(sessionID), nil
}

func (s *Service) KillTerminal(ctx context.Context, sessionID, name string) error {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return err
	}
	if name == "main" {
		return &domain.ValidationError{Field: "name", Message: "cannot kill the main terminal"}
	}
	if err := s.terminal.Kill(ctx, sessionID, name); err != nil && !errors.Is(err, errTerminalNotFound) {
		return err
	}
	return nil
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
	defer s.cancelReceiverBridge(exit.SessionID)

	status := domain.SessionStatusCompleted
	message := "process exited successfully"
	if exit.Err != nil {
		status = domain.SessionStatusFailed
		message = exit.Err.Error()
	}

	if err := s.repo.UpdateSessionStatus(context.Background(), exit.SessionID, status); err != nil {
		s.logger.Error("update session status failed", "session_id", exit.SessionID, "error", err)
		return
	}

	event, err := s.repo.AppendSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionID: exit.SessionID,
		Type:      "status",
		Status:    "ended",
		Message:   message,
		At:        time.Now().UTC(),
	})
	if err != nil {
		s.logger.Warn("append process exit event failed", "session_id", exit.SessionID, "error", err)
		return
	}
	s.publishEvent(exit.SessionID, event)
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

func (s *Service) mcpServersForSession(sessionType domain.SessionType, architectKey, sessionID string) ([]agentruntime.MCPServerConfig, error) {
	hiveryndPath, err := s.resolveExecutablePath()
	if err != nil {
		return nil, fmt.Errorf("resolve hiverynd executable: %w", err)
	}

	return []agentruntime.MCPServerConfig{{
		Name:    "hiveryn-daemon",
		Command: hiveryndPath,
		Args: []string{
			"mcp",
			"--architect-key", architectKey,
			"--daemon-url", s.baseURL,
		},
		Env: map[string]string{
			"HIVERYN_DAEMON_URL":    s.baseURL,
			"HIVERYN_ARCHITECT_KEY": architectKey,
			"HIVERYN_SESSION_TYPE":  string(sessionType),
			"HIVERYN_SESSION_ID":    sessionID,
		},
	}}, nil
}

func (s *Service) resolveExecutablePath() (string, error) {
	if s.executablePath != nil {
		return s.executablePath()
	}
	return os.Executable()
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

func validateRepoPath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &domain.ValidationError{Field: "repo", Message: "repo path does not exist: " + path}
		}
		return fmt.Errorf("stat repo path %s: %w", path, err)
	}
	if !info.IsDir() {
		return &domain.ValidationError{Field: "repo", Message: "repo path is not a directory: " + path}
	}
	gitPath := filepath.Join(path, ".git")
	if gitInfo, err := os.Stat(gitPath); err != nil || !gitInfo.IsDir() {
		return &domain.ValidationError{Field: "repo", Message: "no .git directory found at repo path: " + path}
	}
	return nil
}

func yamlNodeString(node *yaml.Node, key string) string {
	if node == nil || node.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value
		}
	}
	return ""
}

func yamlNodeTime(node *yaml.Node, key string) time.Time {
	raw := yamlNodeString(node, key)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t2, err2 := time.Parse(time.RFC3339, raw)
		if err2 != nil {
			return time.Time{}
		}
		return t2
	}
	return t
}
