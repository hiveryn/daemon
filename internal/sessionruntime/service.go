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
	"github.com/hiveryn/daemon/internal/archive"
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

var defaultTabsBySessionType = map[string][]config.TabEntry{
	"ticket": {{Type: "ticket"}},
}

type Service struct {
	logger         *slog.Logger
	cfg            config.Config
	configSource   config.Source
	repo           domain.SessionRepository
	tickets        domain.TicketService
	baseURL        string
	receiver       *ingest.Receiver
	ingestHTTP     http.Handler
	executablePath func() (string, error)

	adapters map[agentruntime.AgentKind]agentruntime.Adapter
	terminal terminalManager

	intents          *intentStore
	publishArchitect func(string, domain.ArchitectEvent)

	eventMu      sync.RWMutex
	eventNextID  uint64
	eventStreams map[string]map[uint64]chan domain.SessionEvent

	bridgeMu      sync.Mutex
	bridgeCancels map[string]func()

	terminalStateMu sync.RWMutex
	terminalStates  map[string]sessionTerminalState

	archive *archive.Archiver
}

type eventSubscription struct {
	ch     <-chan domain.SessionEvent
	close  func()
	closed sync.Once
}

type sessionTerminalState struct {
	runID          string
	mainTerminalID string
	tabs           []sessionTabState
}

type sessionTabState struct {
	tab          domain.SessionTab
	removeOnExit bool
}

func New(ctx context.Context, cfg config.Config, configSource config.Source, repo domain.SessionRepository, tickets domain.TicketService, logger *slog.Logger, baseURL string) (*Service, error) {
	if configSource == nil {
		configSource = config.StaticSource(cfg)
	}

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
		configSource:   configSource,
		repo:           repo,
		tickets:        tickets,
		baseURL:        baseURL,
		receiver:       receiver,
		ingestHTTP:     http.StripPrefix(ingestPathPrefix, mux),
		executablePath: os.Executable,
		adapters:       adapters,
		terminal:       newPTYTerminalManager(logger),
		intents:        newIntentStore(),
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}, nil
}

func (s *Service) IngestHandler() http.Handler {
	return s.ingestHTTP
}

func (s *Service) SetEventArchiver(a *archive.Archiver) {
	s.archive = a
}

func (s *Service) currentConfig() (config.Config, error) {
	if s.configSource == nil {
		return s.cfg.Clone(), nil
	}
	return s.configSource.Current()
}

func (s *Service) currentArchitect(key string) (config.ArchitectConfig, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return config.ArchitectConfig{}, err
	}

	architect, ok := cfg.Architects[key]
	if !ok {
		return config.ArchitectConfig{}, &domain.NotFoundError{Resource: "architect", ID: key}
	}
	return architect, nil
}

func (s *Service) CreateSession(ctx context.Context, req domain.CreateSessionRequest) (domain.Session, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return domain.Session{}, err
	}

	architect, ok := cfg.Architects[req.ArchitectKey]
	if !ok {
		return domain.Session{}, &domain.NotFoundError{Resource: "architect", ID: req.ArchitectKey}
	}

	switch req.SessionType {
	case domain.SessionTypeArchitect:
		if err := validateArchitectCreateRequest(req); err != nil {
			return domain.Session{}, err
		}
		systemContent, kickoffContent, err := loadArchitectPrompts(req.ArchitectKey, architect, cfg)
		if err != nil {
			return domain.Session{}, err
		}
		now := time.Now().UTC()
		return s.repo.CreateSession(ctx, domain.CreateSessionParams{
			ArchitectKey: req.ArchitectKey,
			SessionType:  domain.SessionTypeArchitect,
			ContextID:    now.Format("2006-01-02-1504"),
			Prompt:       kickoffContent,
			Workdir:      architect.Path,
			Instructions: systemContent,
			CreatedBy:    domain.SessionCreatedByDesktop,
		})
	case domain.SessionTypeTicket:
		if strings.TrimSpace(req.TicketID) == "" {
			return domain.Session{}, &domain.ValidationError{Field: "ticket_id", Message: "is required"}
		}
		if strings.TrimSpace(req.Prompt) != "" {
			return domain.Session{}, &domain.ValidationError{Field: "prompt", Message: "ticket sessions do not accept a prompt"}
		}
		if strings.TrimSpace(req.Workdir) != "" {
			return domain.Session{}, &domain.ValidationError{Field: "workdir", Message: "ticket sessions do not accept a workdir"}
		}
		if strings.TrimSpace(req.Slug) != "" {
			return domain.Session{}, &domain.ValidationError{Field: "slug", Message: "ticket sessions do not accept a slug"}
		}
		ticket, err := s.tickets.GetTicket(ctx, architect.Path, req.TicketID)
		if err != nil {
			return domain.Session{}, err
		}
		if ticket.Status != domain.TicketStatusBacklog {
			return domain.Session{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog to create a ticket session"}
		}
		repoKey := ticket.Repo
		if repoKey == "" {
			return domain.Session{}, &domain.ValidationError{Field: "repo", Message: "ticket has no repo key in frontmatter"}
		}
		repoPath, ok := architect.Repos[repoKey]
		if !ok {
			return domain.Session{}, &domain.ValidationError{Field: "repo", Message: "repo key " + repoKey + " not configured in architect repos"}
		}
		if err := validateRepoPath(repoPath); err != nil {
			return domain.Session{}, err
		}
		additionalRepos, additionalWorkdirs, err := resolveAdditionalRepos(architect.Repos, repoKey, repoPath, ticket.AdditionalRepos)
		if err != nil {
			return domain.Session{}, err
		}
		kickoffContent, err := loadWorkerPrompt(req.ArchitectKey, architect, cfg, ticket)
		if err != nil {
			return domain.Session{}, err
		}
		return s.repo.CreateSession(ctx, domain.CreateSessionParams{
			ArchitectKey:       req.ArchitectKey,
			SessionType:        domain.SessionTypeTicket,
			ContextID:          req.TicketID,
			Prompt:             kickoffContent,
			Workdir:            repoPath,
			AdditionalRepos:    additionalRepos,
			AdditionalWorkdirs: additionalWorkdirs,
			CreatedBy:          domain.SessionCreatedByDesktop,
		})
	case domain.SessionTypeFreeform:
		prompt := req.Prompt
		if strings.TrimSpace(prompt) == "" {
			return domain.Session{}, &domain.ValidationError{Field: "prompt", Message: "is required"}
		}
		workdir := strings.TrimSpace(req.Workdir)
		if workdir == "" {
			return domain.Session{}, &domain.ValidationError{Field: "workdir", Message: "is required"}
		}
		slug := normalizeSlug(req.Slug)
		if strings.TrimSpace(req.Slug) == "" {
			return domain.Session{}, &domain.ValidationError{Field: "slug", Message: "is required"}
		}
		if slug == "" {
			return domain.Session{}, &domain.ValidationError{Field: "slug", Message: "must contain at least one letter or digit"}
		}
		if strings.TrimSpace(req.TicketID) != "" {
			return domain.Session{}, &domain.ValidationError{Field: "ticket_id", Message: "freeform sessions do not accept a ticket ID"}
		}
		if err := validateExistingDirectory(workdir, "workdir"); err != nil {
			return domain.Session{}, err
		}

		now := time.Now().UTC()
		contextID, err := nextFreeformContextID(architect.Path, now, slug)
		if err != nil {
			return domain.Session{}, err
		}
		if err := writeFreeformPrompt(architect.Path, contextID, prompt); err != nil {
			return domain.Session{}, err
		}
		session, err := s.repo.CreateSession(ctx, domain.CreateSessionParams{
			ArchitectKey: req.ArchitectKey,
			SessionType:  domain.SessionTypeFreeform,
			ContextID:    contextID,
			Prompt:       prompt,
			Workdir:      workdir,
			CreatedBy:    domain.SessionCreatedByDesktop,
		})
		if err != nil {
			dir := filepath.Join(architect.Path, "freeform", contextID)
			if removeErr := os.RemoveAll(dir); removeErr != nil {
				return domain.Session{}, fmt.Errorf("create freeform session: %w (also failed to clean up %s: %v)", err, dir, removeErr)
			}
			return domain.Session{}, err
		}
		return session, nil
	default:
		return domain.Session{}, &domain.ValidationError{Field: "session_type", Message: "must be 'architect', 'ticket', or 'freeform'"}
	}
}

func (s *Service) CreateRun(ctx context.Context, sessionID string, req domain.CreateSessionRunRequest) (domain.CreateSessionRunResult, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	cfg, err := s.currentConfig()
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	architect, ok := cfg.Architects[session.ArchitectKey]
	if !ok {
		return domain.CreateSessionRunResult{}, &domain.NotFoundError{Resource: "architect", ID: session.ArchitectKey}
	}

	profile, ok := cfg.Variants[req.ProfileName]
	if !ok {
		return domain.CreateSessionRunResult{}, &domain.NotFoundError{Resource: "agent_profile", ID: req.ProfileName}
	}

	agentKind, err := parseAgentKind(profile.Agent)
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	if err := validateStoredSession(session); err != nil {
		return domain.CreateSessionRunResult{}, err
	}
	if err := validateExistingDirectory(session.Workdir, "workdir"); err != nil {
		return domain.CreateSessionRunResult{}, err
	}
	for i, workdir := range session.AdditionalWorkdirs {
		if err := validateExistingDirectory(workdir, "additional_workdirs"); err != nil {
			return domain.CreateSessionRunResult{}, fmt.Errorf("validate additional repo %q workdir: %w", session.AdditionalRepos[i], err)
		}
	}

	run, err := s.repo.CreateRun(ctx, domain.CreateSessionRunParams{
		SessionID:          session.ID,
		ProfileName:        req.ProfileName,
		ProfileSnapshot:    snapshotVariant(profile),
		Workdir:            session.Workdir,
		AdditionalRepos:    session.AdditionalRepos,
		AdditionalWorkdirs: session.AdditionalWorkdirs,
		StartedAt:          time.Now().UTC(),
	})
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	s.logger.Info("[spawn] starting session run PTY",
		"session_id", session.ID,
		"run_id", run.ID,
		"session_type", session.SessionType,
		"requested_cols", req.Cols,
		"requested_rows", req.Rows,
	)

	mainTerminalID, err := s.launchSession(ctx, cfg, session, run, profile, agentKind, agentruntime.StartRequest{
		Prompt:             session.Prompt,
		Model:              profile.Model,
		Yolo:               profile.Yolo,
		Mode:               agentruntime.Mode(profile.Mode),
		Instructions:       session.Instructions,
		Workdir:            session.Workdir,
		AdditionalWorkdirs: session.AdditionalWorkdirs,
		Args:               append([]string(nil), profile.Args...),
		Env:                cloneStringMap(profile.Env),
	}, terminalSize{Cols: req.Cols, Rows: req.Rows})
	if err != nil {
		s.markRunFailed(run.ID, domain.SessionRunFailureLaunchFailed, "launch")
		return domain.CreateSessionRunResult{}, fmt.Errorf("launch session run: %w", err)
	}

	if session.SessionType == domain.SessionTypeTicket {
		if _, err := s.tickets.MoveTicket(ctx, architect.Path, session.ContextID, domain.MoveTicketParams{To: domain.TicketStatusProgress}); err != nil {
			if killErr := s.terminal.KillBySession(ctx, session.ID); killErr != nil && !errors.Is(killErr, errTerminalNotFound) {
				return domain.CreateSessionRunResult{}, fmt.Errorf("move ticket %s to progress: %w (also failed to kill terminals: %v)", session.ContextID, err, killErr)
			}
			s.cancelReceiverBridge(session.ID)
			s.removeSessionTerminalState(session.ID)
			s.markRunFailed(run.ID, domain.SessionRunFailureLaunchFailed, "move_ticket_to_progress")
			return domain.CreateSessionRunResult{}, fmt.Errorf("move ticket %s to progress: %w", session.ContextID, err)
		}
	}

	return domain.CreateSessionRunResult{Run: run, MainTerminalID: mainTerminalID}, nil
}

func setupRequestForAgent(adapter agentruntime.Adapter, endpoint string, env map[string]string) agentruntime.SetupRequest {
	return agentruntime.SetupRequest{
		Marker:     setupMarker,
		ConfigRoot: adapter.ConfigRoot(env),
		Hook:       hookCommandForAgent(adapter.Agent(), endpoint),
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

func (s *Service) launchSession(ctx context.Context, cfg config.Config, session domain.Session, run domain.SessionRun, profile config.VariantConfig, agentKind agentruntime.AgentKind, startReq agentruntime.StartRequest, size terminalSize) (string, error) {
	mainTerminalID, spec, err := s.startSessionMainTerminal(ctx, session, run, profile, agentKind, startReq, size)
	if err != nil {
		return "", err
	}

	tabs := s.startAutoTerminals(ctx, cfg, session, spec.Workdir, spec.Env, size, spec.CleanupPaths)
	s.storeSessionTerminalState(session.ID, sessionTerminalState{
		runID:          run.ID,
		mainTerminalID: mainTerminalID,
		tabs:           tabs,
	})

	return mainTerminalID, nil
}

func (s *Service) RestoreRunningSessions(ctx context.Context) error {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for restore: %w", err)
	}

	if len(sessions) == 0 {
		return nil
	}

	restoredCount := 0
	failedCount := 0
	for _, session := range sessions {
		if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
			continue
		}
		run := *session.CurrentRun
		if err := s.restoreSession(ctx, session, run); err != nil {
			s.logger.Error("failed to restore session run, marking as failed",
				"session_id", session.ID,
				"run_id", run.ID,
				"session_type", session.SessionType,
				"architect_key", session.ArchitectKey,
				"error", err,
			)
			failedCount++

			if markErr := s.repo.MarkRunFailed(ctx, run.ID, domain.SessionRunFailureRestoreFailed); markErr != nil {
				s.logger.Error("failed to mark run as failed after restore failure",
					"run_id", run.ID,
					"error", markErr,
				)
			}

			if session.SessionType == domain.SessionTypeTicket {
				if ticketErr := s.moveTicketToBacklog(ctx, session); ticketErr != nil {
					s.logger.Error("failed to move ticket back to backlog after restore failure",
						"session_id", session.ID,
						"ticket_id", session.ContextID,
						"error", ticketErr,
					)
				}
			}
			continue
		}
		restoredCount++
		s.logger.Info("session run restored",
			"session_id", session.ID,
			"run_id", run.ID,
			"session_type", session.SessionType,
			"architect_key", session.ArchitectKey,
		)
	}

	if restoredCount > 0 {
		s.logger.Info("restored running session runs", "count", restoredCount)
	}
	if failedCount > 0 {
		s.logger.Warn("failed to restore some session runs", "count", failedCount)
	}

	return nil
}

func (s *Service) restoreSession(ctx context.Context, session domain.Session, run domain.SessionRun) error {
	cfg, err := s.currentConfig()
	if err != nil {
		return err
	}

	profile, agentKind, err := s.resolveStoredRunLaunchContext(session, run)
	if err != nil {
		return err
	}

	if _, err := s.launchSession(ctx, cfg, session, run, profile, agentKind, agentruntime.StartRequest{
		Model:              profile.Model,
		Yolo:               profile.Yolo,
		Mode:               agentruntime.Mode(profile.Mode),
		Instructions:       session.Instructions,
		Workdir:            run.Workdir,
		AdditionalWorkdirs: run.AdditionalWorkdirs,
		Args:               append([]string(nil), profile.Args...),
		Env:                cloneStringMap(profile.Env),
		Resume:             true,
		ResumeID:           run.NativeID,
	}, terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows}); err != nil {
		return fmt.Errorf("launch session run: %w", err)
	}

	return nil
}

func (s *Service) moveTicketToBacklog(ctx context.Context, session domain.Session) error {
	cfg, err := s.currentConfig()
	if err != nil {
		return fmt.Errorf("get config: %w", err)
	}

	architect, ok := cfg.Architects[session.ArchitectKey]
	if !ok {
		return fmt.Errorf("architect %q not found in config", session.ArchitectKey)
	}

	if _, err := s.tickets.MoveTicket(ctx, architect.Path, session.ContextID, domain.MoveTicketParams{To: domain.TicketStatusBacklog}); err != nil {
		return fmt.Errorf("move ticket %q to backlog: %w", session.ContextID, err)
	}

	return nil
}

func (s *Service) startSessionMainTerminal(ctx context.Context, session domain.Session, run domain.SessionRun, profile config.VariantConfig, agentKind agentruntime.AgentKind, startReq agentruntime.StartRequest, size terminalSize) (string, agentruntime.LaunchSpec, error) {
	spec, err := s.prepareLaunchSpec(ctx, session, run, profile, agentKind, startReq)
	if err != nil {
		return "", agentruntime.LaunchSpec{}, err
	}

	s.cancelReceiverBridge(session.ID)
	cancelBridge := s.startReceiverBridge(session.ID)
	s.storeBridgeCancel(session.ID, cancelBridge)

	mainTerminalID := uuid.NewString()
	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID:    session.ID,
		TerminalID:   mainTerminalID,
		Name:         mainTerminalName,
		Command:      spec.Command,
		Args:         append([]string(nil), spec.Args...),
		Env:          cloneStringMap(spec.Env),
		Workdir:      spec.Workdir,
		Size:         size,
		CleanupPaths: append([]string(nil), spec.CleanupPaths...),
		OnExit:       s.handleTerminalExit,
	}); err != nil {
		s.cancelReceiverBridge(session.ID)
		return "", agentruntime.LaunchSpec{}, err
	}

	s.logger.Info("[launch] started agent PTY",
		"session_id", session.ID,
		"run_id", run.ID,
		"session_type", session.SessionType,
		"command", spec.Command,
	)

	return mainTerminalID, spec, nil
}

func (s *Service) prepareLaunchSpec(ctx context.Context, session domain.Session, _ domain.SessionRun, profile config.VariantConfig, agentKind agentruntime.AgentKind, startReq agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	startReq.ID = session.ID
	startReq.Agent = agentKind

	if err := configureOpenCodeArchitectAgent(session, agentKind, &startReq); err != nil {
		return agentruntime.LaunchSpec{}, err
	}

	adapter := s.adapters[agentKind]
	if _, err := adapter.EnsureSetup(ctx, setupRequestForAgent(adapter, s.baseURL+ingestPathPrefix, profile.Env)); err != nil {
		return agentruntime.LaunchSpec{}, fmt.Errorf("ensure %s setup: %w", agentKind, err)
	}

	mcpServers, err := s.mcpServersForSession(session.SessionType, session.ArchitectKey, session.ID, profile.MCP)
	if err != nil {
		return agentruntime.LaunchSpec{}, err
	}
	startReq.MCPServers = mcpServers

	spec, err := adapter.PrepareLaunch(ctx, startReq)
	if err != nil {
		return agentruntime.LaunchSpec{}, fmt.Errorf("prepare launch: %w", err)
	}

	return spec, nil
}

func configureOpenCodeArchitectAgent(session domain.Session, agentKind agentruntime.AgentKind, startReq *agentruntime.StartRequest) error {
	if agentKind != agentruntime.AgentOpenCode || session.SessionType != domain.SessionTypeArchitect {
		return nil
	}
	if strings.TrimSpace(session.ArchitectKey) == "" {
		return errors.New("architect OpenCode session missing architect key")
	}
	if hasArgFlag(startReq.Args, "--agent") {
		return fmt.Errorf("architect OpenCode session profile args must not include --agent; daemon manages the agent selection for architect %q", session.ArchitectKey)
	}
	if startReq.Mode == agentruntime.ModePlan {
		return fmt.Errorf("architect OpenCode session profile must not use mode \"plan\"; daemon manages the agent selection for architect %q", session.ArchitectKey)
	}

	architectKey := session.ArchitectKey
	startReq.Args = append([]string{"--agent", architectKey}, startReq.Args...)
	startReq.OpenCodeAgentConfig = map[string]agentruntime.OpenCodeAgentConfig{
		architectKey: {
			Description: architectAgentDescription(architectKey),
			Mode:        "primary",
			Prompt:      startReq.Instructions,
		},
	}
	startReq.Instructions = ""

	return nil
}

func architectAgentDescription(architectKey string) string {
	if architectKey == "" {
		return "architect"
	}
	return strings.ToUpper(architectKey[:1]) + architectKey[1:] + " architect"
}

func hasArgFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func (s *Service) resolveStoredRunLaunchContext(session domain.Session, run domain.SessionRun) (config.VariantConfig, agentruntime.AgentKind, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return config.VariantConfig{}, "", err
	}

	if _, ok := cfg.Architects[session.ArchitectKey]; !ok {
		return config.VariantConfig{}, "", fmt.Errorf("architect %q not found in architects.yaml", session.ArchitectKey)
	}
	if _, ok := cfg.Variants[run.ProfileName]; !ok {
		return config.VariantConfig{}, "", fmt.Errorf("agent profile %q not found in variants.yaml", run.ProfileName)
	}
	if run.ProfileSnapshot == nil {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s has no profile snapshot", run.ID)
	}
	// An empty NativeID is tolerated: both restore and main-terminal-exit resume
	// pass ResumeID: run.NativeID, so a blank NativeID launches the agent in
	// id-less resume mode (session picker / continue) instead of failing. The
	// spawn->killed-before-first-ingest race can still leave NativeID empty.
	if strings.TrimSpace(run.Workdir) == "" {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s has no workdir", run.ID)
	}
	if len(run.AdditionalRepos) != len(run.AdditionalWorkdirs) {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s additional repo snapshot has %d keys and %d workdirs", run.ID, len(run.AdditionalRepos), len(run.AdditionalWorkdirs))
	}
	if _, err := agentruntime.NormalizeAdditionalWorkdirs(run.Workdir, run.AdditionalWorkdirs); err != nil {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s additional workdirs: %w", run.ID, err)
	}

	snapshot := *run.ProfileSnapshot
	agentKind, err := parseAgentKind(snapshot.Agent)
	if err != nil {
		return config.VariantConfig{}, "", fmt.Errorf("unsupported agent %q in snapshot: %w", snapshot.Agent, err)
	}

	return config.VariantConfig{
		Agent: snapshot.Agent,
		Model: snapshot.Model,
		Yolo:  snapshot.Yolo,
		Mode:  snapshot.Mode,
		Args:  append([]string(nil), snapshot.Args...),
		Env:   cloneStringMap(snapshot.Env),
		MCP:   mcpServersFromSnapshot(snapshot.MCP),
	}, agentKind, nil
}

func (s *Service) resumeSessionMainTerminal(ctx context.Context, session domain.Session, run domain.SessionRun, size terminalSize) (string, error) {
	profile, agentKind, err := s.resolveStoredRunLaunchContext(session, run)
	if err != nil {
		return "", err
	}

	mainTerminalID, _, err := s.startSessionMainTerminal(ctx, session, run, profile, agentKind, agentruntime.StartRequest{
		Model:              profile.Model,
		Yolo:               profile.Yolo,
		Mode:               agentruntime.Mode(profile.Mode),
		Instructions:       session.Instructions,
		Workdir:            run.Workdir,
		AdditionalWorkdirs: run.AdditionalWorkdirs,
		Args:               append([]string(nil), profile.Args...),
		Env:                cloneStringMap(profile.Env),
		Resume:             true,
		ResumeID:           run.NativeID,
	}, size)
	if err != nil {
		return "", err
	}

	if err := s.replaceSessionMainTerminalID(session.ID, mainTerminalID); err != nil {
		return "", err
	}

	return mainTerminalID, nil
}

func (s *Service) ConcludeSession(ctx context.Context, id string, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	session, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}
	run := *session.CurrentRun

	switch session.SessionType {
	case domain.SessionTypeArchitect:
		return s.concludeArchitectSession(ctx, session, run, params)
	case domain.SessionTypeTicket:
		return s.concludeTicketSession(ctx, session, run, params)
	case domain.SessionTypeFreeform:
		return s.concludeFreeformSession(ctx, session, run, params)
	default:
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_type", Message: "cannot conclude session of type " + string(session.SessionType)}
	}
}

func (s *Service) UnspawnTicketSession(ctx context.Context, id string) (domain.ConcludeSessionResult, error) {
	session, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if session.SessionType != domain.SessionTypeTicket {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_type", Message: "only ticket sessions can be discarded"}
	}
	if strings.TrimSpace(session.ContextID) == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "ticket session has no context ID"}
	}
	if session.CurrentRun == nil {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "session has no run to discard"}
	}
	run := *session.CurrentRun

	if err := s.moveTicketToBacklog(ctx, session); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.appendAndPublishSessionEnded(ctx, session.ID, run.ID, "session discarded", "discarded", map[string]any{
		"ticket_id": session.ContextID,
	}); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.terminal.KillBySession(ctx, session.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return domain.ConcludeSessionResult{}, fmt.Errorf("kill session terminal: %w", err)
	}

	if err := s.repo.DeleteRun(ctx, run.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	s.cleanupDeletedSession(session.ID)

	return domain.ConcludeSessionResult{SessionID: session.ID, ArchitectKey: session.ArchitectKey, TicketID: session.ContextID}, nil
}

// validateConclusionCommits enforces the required-field set per outcome shared
// by ticket conclusion and moveTicketToDone: completed requires at least one
// commit, exploratory requires nothing extra, rejected requires a reason.
func validateConclusionCommits(outcome domain.TicketOutcome, rejectionReason string, commits []domain.CommitRef) error {
	switch outcome {
	case domain.TicketOutcomeRejected:
		if strings.TrimSpace(rejectionReason) == "" {
			return &domain.ValidationError{Field: "rejection_reason", Message: "is required when outcome is rejected"}
		}
		return nil
	case domain.TicketOutcomeExploratory:
		return nil
	case domain.TicketOutcomeCompleted:
		if len(commits) == 0 {
			return &domain.ValidationError{Field: "commits", Message: "are required when outcome is completed"}
		}
		return nil
	default:
		return &domain.ValidationError{Field: "outcome", Message: "must be one of: completed, exploratory, rejected"}
	}
}

// RequestConclusion is the agent-facing conclude path: it proposes the
// conclusion as an intent and blocks until the user answers or the tool's
// policy fires. The conclusion is written only by the intent's Exec.
func (s *Service) RequestConclusion(
	ctx context.Context, id string, params domain.ConcludeSessionParams,
) (domain.IntentResolution[domain.ConcludeSessionResult], error) {
	var zero domain.IntentResolution[domain.ConcludeSessionResult]

	session, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return zero, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return zero, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}

	// Render the structured conclusion input into the canonical markdown body
	// before anything is stored or published. This validates the required
	// fields (returning the error straight to the agent) and makes the pending
	// intent — and therefore the desktop approval surface — carry the rendered
	// markdown, so the approval flow and read path are unchanged in shape.
	body, err := s.renderConclusionBody(session.SessionType, params)
	if err != nil {
		return zero, err
	}
	params.Body = body

	// Validate commit/rejection invariants before raising the intent, so the
	// agent gets the error immediately and the desktop dialog is never shown.
	// Only ticket sessions carry commits; architect and freeform conclusions
	// have no such requirement.
	if session.SessionType == domain.SessionTypeTicket {
		if err := validateConclusionCommits(params.Outcome, params.RejectionReason, params.Commits); err != nil {
			return zero, err
		}
	}

	// Deliberately not moved into the generic intent path: restoring the prior
	// status after resolution would race with other intents open on the same
	// session, and a conclusion never needs the restore because the run ends.
	if err := s.repo.UpdateRunAgentStatus(ctx, session.CurrentRun.ID, domain.AgentStatusWaiting); err != nil {
		return zero, err
	}

	return awaitIntent(ctx, s, intentSpec[domain.ConcludeSessionResult]{
		SessionID: id,
		Type:      domain.IntentTypeConcludeSession,
		Summary:   conclusionIntentSummary(params),
		Payload:   conclusionIntentPayload(params),
		Origin:    intentOrigin(session),
		Exec: func(ctx context.Context) (domain.ConcludeSessionResult, error) {
			result, err := s.ConcludeSession(ctx, id, params)
			if err != nil {
				return domain.ConcludeSessionResult{}, err
			}
			if result.TicketID != "" {
				s.emitArchitectEvent(result.ArchitectKey, "ticket_concluded", result.TicketID)
			}
			return result, nil
		},
	})
}

// conclusionIntentSummary is the one-line popup title. The full rendered body
// travels in the payload.
func conclusionIntentSummary(params domain.ConcludeSessionParams) string {
	if summary := strings.TrimSpace(params.Summary); summary != "" {
		return summary
	}
	return "Conclude session"
}

// conclusionIntentPayload carries exactly the fields the old approval_required
// event carried, so the conclusion popup renders from the same data — now
// nested under the generic intent payload.
func conclusionIntentPayload(params domain.ConcludeSessionParams) map[string]any {
	payload := map[string]any{"body": params.Body}
	if len(params.Commits) > 0 {
		payload["commits"] = params.Commits
	}
	if params.Outcome != "" {
		payload["outcome"] = string(params.Outcome)
	}
	if params.RejectionReason != "" {
		payload["rejection_reason"] = params.RejectionReason
	}
	return payload
}

func (s *Service) ReadConclusion(ctx context.Context, architectKey, id string) (domain.ArchitectConclusion, error) {
	architect, err := s.currentArchitect(architectKey)
	if err != nil {
		return domain.ArchitectConclusion{}, err
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
	architect, err := s.currentArchitect(architectKey)
	if err != nil {
		return domain.ArchitectConclusion{}, err
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
	architect, err := s.currentArchitect(architectKey)
	if err != nil {
		return nil, err
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

func (s *Service) concludeArchitectSession(ctx context.Context, session domain.Session, run domain.SessionRun, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	activeTicketSessionIDs, err := s.activeTicketSessionIDs(ctx, session.ArchitectKey, session.ID)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if len(activeTicketSessionIDs) > 0 {
		return domain.ConcludeSessionResult{}, &domain.ConflictError{
			Resource: "session",
			Field:    "architect_key",
			Message:  fmt.Sprintf("Cannot conclude architect session: %d ticket session(s) still active: %s", len(activeTicketSessionIDs), strings.Join(activeTicketSessionIDs, ", ")),
		}
	}

	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if strings.TrimSpace(params.Body) != "" {
		folderName := session.ContextID
		dir := filepath.Join(architect.Path, "architect-sessions", folderName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return domain.ConcludeSessionResult{}, fmt.Errorf("create architect session directory: %w", err)
		}

		now := time.Now().UTC()
		conclusionPath := filepath.Join(dir, conclusionFileName)
		doc := newArchitectConclusionDocument(session.CreatedAt, now, run.ProfileName, params.Body)
		if err := writeConclusionFile(conclusionPath, doc); err != nil {
			return domain.ConcludeSessionResult{}, err
		}
	}

	if err := s.repo.MarkRunCompleted(ctx, run.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.appendAndPublishSessionEnded(ctx, session.ID, run.ID, "session concluded", "concluded", map[string]any{"body": params.Body}); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.terminal.KillBySession(ctx, session.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return domain.ConcludeSessionResult{}, fmt.Errorf("kill session terminal: %w", err)
	}

	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	s.cleanupDeletedSession(session.ID)

	return domain.ConcludeSessionResult{SessionID: session.ID, ArchitectKey: session.ArchitectKey}, nil
}

func (s *Service) activeTicketSessionIDs(ctx context.Context, architectKey, excludedSessionID string) ([]string, error) {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions for architect conclude: %w", err)
	}

	ids := make([]string, 0, len(sessions))
	for _, candidate := range sessions {
		if candidate.ID == excludedSessionID {
			continue
		}
		if candidate.ArchitectKey != architectKey {
			continue
		}
		if candidate.SessionType != domain.SessionTypeTicket {
			continue
		}
		if candidate.CurrentRun == nil || candidate.CurrentRun.Status != domain.SessionRunStatusRunning {
			continue
		}
		ids = append(ids, candidate.ID)
	}

	sort.Strings(ids)
	return ids, nil
}

func (s *Service) concludeTicketSession(ctx context.Context, session domain.Session, run domain.SessionRun, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	// Commit/rejection validation runs earlier in RequestConclusion (before the
	// approval dialog fires); see validateConclusionCommits.
	if err := validateConclusionCommits(params.Outcome, params.RejectionReason, params.Commits); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if strings.TrimSpace(session.ContextID) == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "ticket session has no context ID"}
	}

	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	ticket, err := s.tickets.GetTicket(ctx, architect.Path, session.ContextID)
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
	_, ok := architect.Repos[repoKey]
	if !ok {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "repo", Message: "repo key " + repoKey + " not configured in architect repos"}
	}

	scopedRepos := map[string]string{ticket.Repo: session.Workdir}
	for i, key := range session.AdditionalRepos {
		scopedRepos[key] = session.AdditionalWorkdirs[i]
	}
	resolvedCommits, err := resolveConclusionCommitRefs(ctx, scopedRepos, params.Commits)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	now := time.Now().UTC()
	startedAt := session.CreatedAt
	if run.StartedAt != nil {
		startedAt = run.StartedAt.UTC()
	}
	conclusion := domain.TicketConclusion{
		StartedAt:       startedAt,
		ConcludedAt:     now,
		Agent:           run.ProfileName,
		Profile:         run.ProfileName,
		Outcome:         params.Outcome,
		RejectionReason: params.RejectionReason,
		Commits:         resolvedCommits,
		Body:            params.Body,
	}

	if _, err := s.tickets.ConcludeTicket(ctx, architect.Path, session.ContextID, conclusion); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.repo.MarkRunCompleted(ctx, run.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.appendAndPublishSessionEnded(ctx, session.ID, run.ID, "session concluded", "concluded", map[string]any{
		"body":             params.Body,
		"commits":          resolvedCommits,
		"outcome":          string(params.Outcome),
		"rejection_reason": params.RejectionReason,
	}); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.terminal.KillBySession(ctx, session.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return domain.ConcludeSessionResult{}, fmt.Errorf("kill session terminal: %w", err)
	}

	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	s.cleanupDeletedSession(session.ID)

	return domain.ConcludeSessionResult{SessionID: session.ID, ArchitectKey: session.ArchitectKey, TicketID: session.ContextID}, nil
}

func (s *Service) MoveTicketToDone(ctx context.Context, architectKey, ticketID string, params domain.MoveTicketToDoneParams) (domain.MoveTicketToDoneResult, error) {
	if strings.TrimSpace(params.Body) == "" {
		return domain.MoveTicketToDoneResult{}, &domain.ValidationError{Field: "body", Message: "is required"}
	}
	if err := validateConclusionCommits(params.Outcome, params.RejectionReason, params.Commits); err != nil {
		return domain.MoveTicketToDoneResult{}, err
	}

	architect, err := s.currentArchitect(architectKey)
	if err != nil {
		return domain.MoveTicketToDoneResult{}, err
	}

	ticket, err := s.tickets.GetTicket(ctx, architect.Path, ticketID)
	if err != nil {
		return domain.MoveTicketToDoneResult{}, err
	}

	switch ticket.Status {
	case domain.TicketStatusBacklog:
		// No worker session can be running for a backlog ticket.
	case domain.TicketStatusProgress:
		runningSessionID, running, err := s.runningTicketSessionID(ctx, architectKey, ticketID)
		if err != nil {
			return domain.MoveTicketToDoneResult{}, err
		}
		if running {
			return domain.MoveTicketToDoneResult{}, &domain.ConflictError{
				Resource: "session",
				Field:    "ticket_id",
				Message:  "Cannot move ticket to done: worker session " + runningSessionID + " is currently running",
			}
		}
	default:
		return domain.MoveTicketToDoneResult{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog or progress to move to done"}
	}

	scopedRepos := make(map[string]string, 1+len(ticket.AdditionalRepos))
	for _, key := range append([]string{ticket.Repo}, ticket.AdditionalRepos...) {
		scopedRepos[key] = architect.Repos[key]
	}
	resolvedCommits, err := resolveConclusionCommitRefs(ctx, scopedRepos, params.Commits)
	if err != nil {
		return domain.MoveTicketToDoneResult{}, err
	}

	now := time.Now().UTC()
	conclusion := domain.TicketConclusion{
		StartedAt:       now,
		ConcludedAt:     now,
		Outcome:         params.Outcome,
		RejectionReason: params.RejectionReason,
		Commits:         resolvedCommits,
		Body:            params.Body,
	}

	if _, err := s.tickets.ConcludeTicket(ctx, architect.Path, ticketID, conclusion); err != nil {
		return domain.MoveTicketToDoneResult{}, err
	}

	return domain.MoveTicketToDoneResult{TicketID: ticketID, ArchitectKey: architectKey}, nil
}

func (s *Service) runningTicketSessionID(ctx context.Context, architectKey, ticketID string) (string, bool, error) {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list sessions for ticket running check: %w", err)
	}

	for _, candidate := range sessions {
		if candidate.ArchitectKey != architectKey {
			continue
		}
		if candidate.SessionType != domain.SessionTypeTicket {
			continue
		}
		if candidate.ContextID != ticketID {
			continue
		}
		if candidate.CurrentRun == nil || candidate.CurrentRun.Status != domain.SessionRunStatusRunning {
			continue
		}
		return candidate.ID, true, nil
	}

	return "", false, nil
}

func (s *Service) concludeFreeformSession(ctx context.Context, session domain.Session, run domain.SessionRun, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	if params.Outcome != "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "outcome", Message: "freeform sessions do not support an outcome"}
	}
	if strings.TrimSpace(params.RejectionReason) != "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "rejection_reason", Message: "freeform sessions do not accept a rejection reason"}
	}
	if strings.TrimSpace(session.ContextID) == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "freeform session has no context ID"}
	}

	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	resolvedCommits, err := resolveConclusionCommitRefs(ctx, architect.Repos, params.Commits)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if strings.TrimSpace(params.Body) != "" {
		now := time.Now().UTC()
		startedAt := session.CreatedAt
		if run.StartedAt != nil {
			startedAt = run.StartedAt.UTC()
		}
		dir := filepath.Join(architect.Path, "freeform", session.ContextID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return domain.ConcludeSessionResult{}, fmt.Errorf("create freeform session directory: %w", err)
		}
		conclusionPath := filepath.Join(dir, conclusionFileName)
		doc := newSessionConclusionDocument(startedAt, now, run.ProfileName, params.Body, resolvedCommits)
		if err := writeConclusionFile(conclusionPath, doc); err != nil {
			return domain.ConcludeSessionResult{}, err
		}
	}

	if err := s.repo.MarkRunCompleted(ctx, run.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	raw := map[string]any{"body": params.Body}
	if len(resolvedCommits) > 0 {
		raw["commits"] = resolvedCommits
	}
	if err := s.appendAndPublishSessionEnded(ctx, session.ID, run.ID, "session concluded", "concluded", raw); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.terminal.KillBySession(ctx, session.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return domain.ConcludeSessionResult{}, fmt.Errorf("kill session terminal: %w", err)
	}

	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	s.cleanupDeletedSession(session.ID)

	return domain.ConcludeSessionResult{SessionID: session.ID, ArchitectKey: session.ArchitectKey}, nil
}

func (s *Service) appendAndPublishSessionEnded(ctx context.Context, sessionID, runID, message, lifecycle string, raw map[string]any) error {
	// Every session-ending path funnels through here, and it is the last point
	// at which the session row still exists — so this is where intents left
	// open on the session get resolved. Without it a pending createWorkTicket
	// would outlive its session and fire its write into a dead session when the
	// wait window expired. The conclude intent driving this teardown is already
	// claimed, so it is skipped rather than resolved twice.
	s.failPendingIntents(ctx, sessionID, "session ended before this intent was resolved")

	raw = cloneAnyMap(raw)
	raw["lifecycle"] = lifecycle
	raw["session_id"] = sessionID
	if runID != "" {
		raw["run_id"] = runID
	}
	raw["message"] = message
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: sessionID,
		RunID:     runID,
		Type:      "status",
		Status:    "ended",
		Message:   message,
		Raw:       raw,
		At:        time.Now().UTC(),
	})
}

func (s *Service) appendAndPublishSessionEvent(ctx context.Context, params domain.AppendSessionEventParams) error {
	event, err := s.repo.AppendSessionEvent(ctx, params)
	if err != nil {
		return fmt.Errorf("append session event: %w", err)
	}
	s.publishEvent(params.SessionID, event)
	return nil
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

func resolveConclusionCommitRefs(ctx context.Context, repos map[string]string, commits []domain.CommitRef) ([]domain.CommitRef, error) {
	resolved := make([]domain.CommitRef, 0, len(commits))
	for _, commit := range commits {
		sha := strings.TrimSpace(commit.SHA)
		if sha == "" {
			return nil, &domain.ValidationError{Field: "commits", Message: "commit SHA cannot be empty"}
		}

		repoKey := strings.TrimSpace(commit.Repo)
		if repoKey == "" {
			return nil, &domain.ValidationError{Field: "commits", Message: "commit repo cannot be empty"}
		}

		repoPath, ok := repos[repoKey]
		if !ok {
			return nil, &domain.ValidationError{Field: "commits", Message: "repo key " + repoKey + " not configured in architect repos"}
		}

		if err := validateCommitSHA(ctx, repoPath, sha); err != nil {
			return nil, &domain.ValidationError{Field: "commits", Message: "commit " + sha + " not found in repo " + repoKey + ": " + err.Error()}
		}

		resolved = append(resolved, domain.CommitRef{SHA: sha, Repo: repoKey})
	}
	return resolved, nil
}

const conclusionFileName = "conclusion.md"
const promptFileName = "prompt.md"

func newArchitectConclusionDocument(startedAt, concludedAt time.Time, agent, body string) architectfs.MarkdownDocument {
	return architectfs.NewArchitectConclusion(startedAt, concludedAt, agent, body)
}

func newSessionConclusionDocument(startedAt, concludedAt time.Time, agent, body string, commits []domain.CommitRef) architectfs.MarkdownDocument {
	if len(commits) == 0 {
		return newArchitectConclusionDocument(startedAt, concludedAt, agent, body)
	}
	meta := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setYAMLString(meta, "started_at", startedAt.UTC().Format(time.RFC3339Nano))
	setYAMLString(meta, "concluded_at", concludedAt.UTC().Format(time.RFC3339Nano))
	if agent != "" {
		setYAMLString(meta, "agent", agent)
	}
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, commit := range commits {
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setYAMLString(entry, "sha", commit.SHA)
		setYAMLString(entry, "repo", commit.Repo)
		sequence.Content = append(sequence.Content, entry)
	}
	setYAMLNode(meta, "commits", sequence)
	return architectfs.MarkdownDocument{Metadata: meta, Body: body}
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

func writePromptFile(path, body string) error {
	content, err := architectfs.RenderMarkdownDocument(architectfs.MarkdownDocument{Body: body})
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write prompt: %w", err)
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

	if err := s.repo.DeleteSession(ctx, id); err != nil {
		return err
	}
	s.cleanupDeletedSession(id)
	return nil
}

func (s *Service) cleanupDeletedSession(id string) {
	s.cancelReceiverBridge(id)
	s.closeEventSubscribers(id)
	s.removeSessionTerminalState(id)
}

func (s *Service) GetSession(ctx context.Context, id string) (domain.Session, error) {
	session, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return domain.Session{}, err
	}
	return s.hydrateSession(session), nil
}

func (s *Service) ListSessions(ctx context.Context) ([]domain.Session, error) {
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i] = s.hydrateSession(sessions[i])
	}
	return sessions, nil
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

func (s *Service) AttachTerminal(ctx context.Context, sessionID, terminalID string) (domain.TerminalAttachment, error) {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.terminal.Attach(ctx, sessionID, terminalID)
}

func (s *Service) CreateTerminal(ctx context.Context, sessionID string, params domain.CreateTerminalParams) (domain.TerminalInfo, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.TerminalInfo{}, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "session_id", Message: "session is not running"}
	}
	if strings.TrimSpace(session.CurrentRun.Workdir) == "" {
		return domain.TerminalInfo{}, fmt.Errorf("session %s current run has no workdir", session.ID)
	}
	if params.Placement != domain.TerminalPlacementTab && params.Placement != domain.TerminalPlacementSplit {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "placement", Message: "must be one of: tab, split"}
	}
	if params.Placement == domain.TerminalPlacementTab && strings.TrimSpace(params.BaseTabID) != "" {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "base_tab_id", Message: "is only allowed when placement is split"}
	}
	if params.Placement == domain.TerminalPlacementSplit {
		if strings.TrimSpace(params.BaseTabID) == "" {
			return domain.TerminalInfo{}, &domain.ValidationError{Field: "base_tab_id", Message: "is required when placement is split"}
		}
		foundBaseTab := false
		for _, tab := range s.sessionTabs(sessionID) {
			if sessionTabID(tab) == params.BaseTabID && tab.Placement != domain.TerminalPlacementSplit {
				foundBaseTab = true
			}
			if tab.Type == "terminal" && tab.Placement == domain.TerminalPlacementSplit && tab.BaseTabID == params.BaseTabID {
				return domain.TerminalInfo{}, &domain.ConflictError{Resource: "terminal", Field: "base_tab_id", Message: "split terminal already exists for base tab"}
			}
		}
		if !foundBaseTab {
			return domain.TerminalInfo{}, &domain.ValidationError{Field: "base_tab_id", Message: "must reference an existing primary right-pane tab"}
		}
	}

	command := s.defaultShell()

	terminalID := uuid.NewString()
	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID:  sessionID,
		TerminalID: terminalID,
		Command:    command,
		Workdir:    session.CurrentRun.Workdir,
		Size:       terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows},
		OnExit:     s.handleAuxTerminalExit,
	}); err != nil {
		return domain.TerminalInfo{}, err
	}

	s.appendSessionTab(sessionID, sessionTabState{
		tab: domain.SessionTab{
			Type:      "terminal",
			ID:        terminalID,
			Command:   command,
			Status:    "running",
			Placement: params.Placement,
			BaseTabID: params.BaseTabID,
		},
		removeOnExit: true,
	})

	return domain.TerminalInfo{
		TerminalID: terminalID,
		SessionID:  sessionID,
		Command:    command,
		Status:     "running",
	}, nil
}

func (s *Service) ListTerminals(ctx context.Context, sessionID string) ([]domain.TerminalInfo, error) {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.terminal.ListBySession(sessionID), nil
}

func (s *Service) ListSessionTabs(ctx context.Context, sessionID string) ([]domain.SessionTab, error) {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}

	tabs := s.sessionTabs(sessionID)
	if tabs == nil {
		return []domain.SessionTab{}, nil
	}

	terminals := s.terminal.ListBySession(sessionID)
	byTerminalID := make(map[string]domain.TerminalInfo, len(terminals))
	for _, terminal := range terminals {
		byTerminalID[terminal.TerminalID] = terminal
	}

	for i := range tabs {
		if tabs[i].Type != "terminal" {
			continue
		}
		tabs[i].Status = "exited"
		if terminal, ok := byTerminalID[tabs[i].ID]; ok {
			tabs[i].Status = terminal.Status
			if terminal.Command != "" {
				tabs[i].Command = terminal.Command
			}
		}
	}

	return tabs, nil
}

func (s *Service) KillTerminal(ctx context.Context, sessionID, terminalID string) error {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return err
	}
	if s.mainTerminalID(sessionID) == terminalID {
		return &domain.ValidationError{Field: "terminal_id", Message: "cannot kill the main terminal"}
	}
	if err := s.terminal.Kill(ctx, sessionID, terminalID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return err
	}
	s.removeSessionTabOnTerminalClose(sessionID, terminalID)
	return nil
}

func (s *Service) PreviewBrowserTab(ctx context.Context, sessionID string, params domain.PreviewBrowserTabParams) (domain.BrowserTabInfo, error) {
	target := strings.TrimSpace(params.Target)
	if target == "" {
		return domain.BrowserTabInfo{}, &domain.ValidationError{Field: "target", Message: "is required"}
	}
	if !isValidBrowserTarget(target) {
		return domain.BrowserTabInfo{}, &domain.ValidationError{Field: "target", Message: "must be a file://, absolute path, http://localhost, or https:// URL"}
	}

	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.BrowserTabInfo{}, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.BrowserTabInfo{}, &domain.ValidationError{Field: "session_id", Message: "session is not running"}
	}

	tabID := strings.TrimSpace(params.TabID)
	if tabID == "" {
		tabID = uuid.NewString()
		s.appendSessionTab(sessionID, sessionTabState{
			tab: domain.SessionTab{
				Type:   "browser",
				ID:     tabID,
				Target: target,
			},
		})
	} else if !s.setSessionTabTarget(sessionID, tabID, target) {
		return domain.BrowserTabInfo{}, &domain.NotFoundError{Resource: "browser_tab", ID: tabID}
	}

	if err := s.publishTabChanged(ctx, sessionID); err != nil {
		return domain.BrowserTabInfo{}, err
	}

	return domain.BrowserTabInfo{
		TabID:     tabID,
		SessionID: sessionID,
		Target:    target,
	}, nil
}

func (s *Service) CloseBrowserTab(ctx context.Context, sessionID, tabID string) error {
	if _, err := s.repo.GetSession(ctx, sessionID); err != nil {
		return err
	}
	if !s.removeSessionTabByID(sessionID, tabID) {
		return &domain.NotFoundError{Resource: "browser_tab", ID: tabID}
	}
	return s.publishTabChanged(ctx, sessionID)
}

func isValidBrowserTarget(target string) bool {
	switch {
	case strings.HasPrefix(target, "file://"):
		return true
	case strings.HasPrefix(target, "https://"):
		return true
	case strings.HasPrefix(target, "http://localhost:"), target == "http://localhost":
		return true
	case strings.HasPrefix(target, "/"):
		return true
	default:
		return false
	}
}

func (s *Service) publishTabChanged(ctx context.Context, sessionID string) error {
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionID: sessionID,
		Type:      "status",
		Status:    "tab_changed",
		Message:   "session tabs changed",
		At:        time.Now().UTC(),
	})
}

func (s *Service) Shutdown(ctx context.Context) error {
	terminalErr := s.terminal.Shutdown(ctx)
	s.cancelReceiverBridges()
	s.closeEventSubscribers("")
	if s.archive != nil {
		if err := s.archive.Close(); err != nil {
			s.logger.Error("failed to close event archive", "error", err)
		}
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

func (s *Service) archiveEvent(event agentruntime.Event, run *domain.SessionRun) {
	if s.archive == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("archive event panicked", "session_id", event.ID, "panic", r)
		}
	}()

	session, err := s.repo.GetSession(context.Background(), event.ID)
	if err != nil {
		s.logger.Warn("failed to get session for event archival", "session_id", event.ID, "error", err)
		return
	}

	architectWorkspace := ""
	if session.ArchitectKey != "" {
		architect, err := s.currentArchitect(session.ArchitectKey)
		if err != nil {
			s.logger.Warn("failed to get architect for event archival", "session_id", event.ID, "architect_key", session.ArchitectKey, "error", err)
		} else {
			architectWorkspace = architect.Path
		}
	}

	agentProfile := ""
	if run.ProfileSnapshot != nil {
		agentProfile = run.ProfileSnapshot.Agent
	}

	raw := event.Raw
	if raw == nil {
		raw = map[string]any{}
	}

	record := archive.ArchivedEvent{
		IngestedAt:         time.Now(),
		ArchitectKey:       session.ArchitectKey,
		ArchitectWorkspace: architectWorkspace,
		SessionID:          event.ID,
		SessionType:        string(session.SessionType),
		TicketID:           session.ContextID,
		ProfileName:        run.ProfileName,
		Agent:              agentProfile,
		Workdir:            run.Workdir,
		AdditionalWorkdirs: run.AdditionalWorkdirs,
		AdditionalRepos:    run.AdditionalRepos,
		AgentID:            event.ID,
		AgentKind:          string(event.Agent),
		NativeID:           event.NativeID,
		PrimaryNativeID:    event.PrimaryNativeID,
		NativeSessionRole:  string(event.NativeSessionRole),
		Status:             string(event.Status),
		Tool:               event.Tool,
		Message:            event.Message,
		At:                 event.At,
		Metadata:           cloneStringMap(event.Metadata),
		Raw:                cloneAnyMap(raw),
	}

	s.archive.Write(record)
}

func (s *Service) handleReceiverEvent(event agentruntime.Event) {
	run, err := s.repo.GetCurrentRun(context.Background(), event.ID)
	if err != nil {
		panic(fmt.Errorf("get current run for session %s on receiver event: %w", event.ID, err))
	}
	if run == nil || run.Status != domain.SessionRunStatusRunning {
		panic(fmt.Errorf("receiver event for session %s has no running run", event.ID))
	}

	persisted, err := s.repo.AppendSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionID:         event.ID,
		RunID:             run.ID,
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
		panic(fmt.Errorf("persist session event for session %s run %s: %w", event.ID, run.ID, err))
	}

	s.archiveEvent(event, run)

	primaryNativeID := event.PrimaryNativeID
	if primaryNativeID == "" {
		primaryNativeID = event.NativeID
	}
	if primaryNativeID != "" {
		if err := s.repo.UpdateRunNativeID(context.Background(), run.ID, primaryNativeID); err != nil {
			panic(fmt.Errorf("update session run native id for session %s run %s: %w", event.ID, run.ID, err))
		}
	}

	if agentStatus := mapAgentStatusToRunStatus(event.Status); agentStatus != "" {
		previous := run.AgentStatus
		if err := s.repo.UpdateRunAgentStatus(context.Background(), run.ID, agentStatus); err != nil {
			panic(fmt.Errorf("update session run agent status for session %s run %s: %w", event.ID, run.ID, err))
		}
		// Emit a live event only when the mapped status actually changes, so the
		// desktop can re-render the per-session tab icon without polling. A
		// distinct type avoids colliding with the overloaded "status" events
		// (approval_required/approval_resolved/ended and raw agentruntime
		// statuses). The append persists it, so it lands in the connect backlog
		// and a freshly-attached client renders the right icon immediately.
		if agentStatus != previous {
			if err := s.appendAndPublishSessionEvent(context.Background(), domain.AppendSessionEventParams{
				SessionID: event.ID,
				RunID:     run.ID,
				Type:      "agent_status",
				Status:    agentStatus,
				At:        event.At,
			}); err != nil {
				panic(fmt.Errorf("publish agent_status event for session %s run %s: %w", event.ID, run.ID, err))
			}
		}
	}

	s.publishEvent(event.ID, persisted)
}

func (s *Service) handleTerminalExit(exit terminalExit) {
	session, err := s.repo.GetSession(context.Background(), exit.SessionID)
	if err != nil {
		panic(fmt.Errorf("get session %s after main terminal exit: %w", exit.SessionID, err))
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		panic(fmt.Errorf("main terminal exited for non-running session %s", exit.SessionID))
	}
	run := *session.CurrentRun

	mainTerminalID, err := s.resumeSessionMainTerminal(context.Background(), session, run, terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows})
	if err != nil {
		// A single session's resume failure must never crash the daemon (and every
		// other live session with it). Recover per-session: mark this run failed and,
		// for tickets, move it back to backlog — mirroring RestoreRunningSessions.
		s.logger.Error("failed to resume main terminal after exit, marking run as failed",
			"session_id", exit.SessionID,
			"run_id", run.ID,
			"session_type", session.SessionType,
			"error", err,
		)
		s.markRunFailed(run.ID, domain.SessionRunFailureLaunchFailed, "resume_main_terminal")
		if session.SessionType == domain.SessionTypeTicket {
			if ticketErr := s.moveTicketToBacklog(context.Background(), session); ticketErr != nil {
				s.logger.Error("failed to move ticket back to backlog after resume failure",
					"session_id", exit.SessionID,
					"ticket_id", session.ContextID,
					"error", ticketErr,
				)
			}
		}
		return
	}

	if err := s.appendAndPublishSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionID: exit.SessionID,
		RunID:     run.ID,
		Type:      "main_terminal_resumed",
		Message:   "main terminal resumed",
		Raw: map[string]any{
			"main_terminal_id":     mainTerminalID,
			"previous_terminal_id": exit.TerminalID,
			"exit_error":           errorString(exit.Err),
		},
		At: time.Now().UTC(),
	}); err != nil {
		panic(fmt.Errorf("append main terminal resumed event for session %s run %s: %w", exit.SessionID, run.ID, err))
	}
}

func (s *Service) handleAuxTerminalExit(exit terminalExit) {
	s.removeSessionTabOnTerminalClose(exit.SessionID, exit.TerminalID)
}

func (s *Service) markRunFailed(runID string, reason domain.SessionRunFailureReason, stage string) {
	if err := s.repo.MarkRunFailed(context.Background(), runID, reason); err != nil {
		s.logger.Error("mark failed session run", "run_id", runID, "failure_reason", reason, "stage", stage, "error", err)
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

func mapAgentStatusToRunStatus(native agentruntime.Status) string {
	switch native {
	case agentruntime.StatusStarting, agentruntime.StatusWorking:
		return domain.AgentStatusActive
	case agentruntime.StatusIdle:
		return domain.AgentStatusIdle
	case agentruntime.StatusAwaitingInput:
		return domain.AgentStatusWaiting
	case agentruntime.StatusEnded, agentruntime.StatusError:
		return domain.AgentStatusStopped
	default:
		return ""
	}
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

func (s *Service) mcpServersForSession(sessionType domain.SessionType, architectKey, sessionID string, variantServers map[string]config.MCPServerConfig) ([]agentruntime.MCPServerConfig, error) {
	hiveryndPath, err := s.resolveExecutablePath()
	if err != nil {
		return nil, fmt.Errorf("resolve hiverynd executable: %w", err)
	}

	servers := []agentruntime.MCPServerConfig{{
		Name:    config.ReservedMCPServerName,
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
	}}

	names := make([]string, 0, len(variantServers))
	for name := range variantServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := variantServers[name]
		servers = append(servers, agentruntime.MCPServerConfig{
			Name:              name,
			Command:           server.Command,
			Args:              append([]string(nil), server.Args...),
			CWD:               server.CWD,
			Env:               cloneStringMap(server.Env),
			URL:               server.URL,
			BearerTokenEnvVar: server.BearerTokenEnvVar,
		})
	}

	return servers, nil
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

func validateArchitectCreateRequest(req domain.CreateSessionRequest) error {
	if strings.TrimSpace(req.TicketID) != "" {
		return &domain.ValidationError{Field: "ticket_id", Message: "architect sessions do not accept a ticket ID"}
	}
	if strings.TrimSpace(req.Prompt) != "" {
		return &domain.ValidationError{Field: "prompt", Message: "architect sessions do not accept a prompt"}
	}
	if strings.TrimSpace(req.Workdir) != "" {
		return &domain.ValidationError{Field: "workdir", Message: "architect sessions do not accept a workdir"}
	}
	if strings.TrimSpace(req.Slug) != "" {
		return &domain.ValidationError{Field: "slug", Message: "architect sessions do not accept a slug"}
	}
	return nil
}

func validateStoredSession(session domain.Session) error {
	if strings.TrimSpace(session.ContextID) == "" {
		return fmt.Errorf("session %s has no context_id", session.ID)
	}
	if strings.TrimSpace(session.Prompt) == "" {
		return fmt.Errorf("session %s has no prompt", session.ID)
	}
	if strings.TrimSpace(session.Workdir) == "" {
		return fmt.Errorf("session %s has no workdir", session.ID)
	}
	if len(session.AdditionalRepos) != len(session.AdditionalWorkdirs) {
		return fmt.Errorf("session %s additional repo snapshot has %d keys and %d workdirs", session.ID, len(session.AdditionalRepos), len(session.AdditionalWorkdirs))
	}
	if _, err := agentruntime.NormalizeAdditionalWorkdirs(session.Workdir, session.AdditionalWorkdirs); err != nil {
		return fmt.Errorf("session %s additional workdirs: %w", session.ID, err)
	}
	return nil
}

func nextFreeformContextID(architectPath string, now time.Time, slug string) (string, error) {
	prefix := now.Local().Format("2006-01-02-1504") + "-" + slug
	contextID := prefix
	for suffix := 2; ; suffix++ {
		dir := filepath.Join(architectPath, "freeform", contextID)
		_, err := os.Stat(dir)
		if err == nil {
			contextID = fmt.Sprintf("%s-%d", prefix, suffix)
			continue
		}
		if os.IsNotExist(err) {
			return contextID, nil
		}
		return "", fmt.Errorf("stat freeform directory %s: %w", dir, err)
	}
}

func writeFreeformPrompt(architectPath, contextID, prompt string) error {
	dir := filepath.Join(architectPath, "freeform", contextID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create freeform session directory: %w", err)
	}
	return writePromptFile(filepath.Join(dir, promptFileName), prompt)
}

func normalizeSlug(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	var b strings.Builder
	lastDash := false
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if lastDash {
			continue
		}
		b.WriteByte('-')
		lastDash = true
	}
	return strings.Trim(b.String(), "-")
}

func validateExistingDirectory(path, field string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &domain.ValidationError{Field: field, Message: "path does not exist: " + path}
		}
		return fmt.Errorf("stat %s path %s: %w", field, path, err)
	}
	if !info.IsDir() {
		return &domain.ValidationError{Field: field, Message: "path is not a directory: " + path}
	}
	return nil
}

func setYAMLString(node *yaml.Node, key, value string) {
	setYAMLNode(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func setYAMLNode(node *yaml.Node, key string, value *yaml.Node) {
	if node == nil {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

func validateRepoPath(path string) error {
	if err := validateExistingDirectory(path, "repo"); err != nil {
		return err
	}
	gitPath := filepath.Join(path, ".git")
	if gitInfo, err := os.Stat(gitPath); err != nil || !gitInfo.IsDir() {
		return &domain.ValidationError{Field: "repo", Message: "no .git directory found at repo path: " + path}
	}
	return nil
}

func resolveAdditionalRepos(repos map[string]string, primaryKey, primaryPath string, keys []string) ([]string, []string, error) {
	normalizedKeys := append([]string(nil), keys...)
	for i := range normalizedKeys {
		normalizedKeys[i] = strings.TrimSpace(normalizedKeys[i])
	}
	sort.Strings(normalizedKeys)
	seenKeys := map[string]struct{}{primaryKey: {}}
	seenPaths := map[string]string{filepath.Clean(primaryPath): primaryKey}
	workdirs := make([]string, 0, len(normalizedKeys))
	for _, key := range normalizedKeys {
		if key == "" {
			return nil, nil, &domain.ValidationError{Field: "additional_repos", Message: "repo keys cannot be blank"}
		}
		if _, exists := seenKeys[key]; exists {
			return nil, nil, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is duplicated or overlaps primary repo", key)}
		}
		path, ok := repos[key]
		if !ok {
			return nil, nil, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is not configured in architect repos", key)}
		}
		if err := validateRepoPath(path); err != nil {
			return nil, nil, fmt.Errorf("validate additional repo %q: %w", key, err)
		}
		cleaned := filepath.Clean(path)
		if other, exists := seenPaths[cleaned]; exists {
			return nil, nil, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo keys %q and %q resolve to the same cleaned path %q", other, key, cleaned)}
		}
		seenKeys[key] = struct{}{}
		seenPaths[cleaned] = key
		workdirs = append(workdirs, cleaned)
	}
	return normalizedKeys, workdirs, nil
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

func snapshotVariant(profile config.VariantConfig) domain.AgentProfileSnapshot {
	return domain.AgentProfileSnapshot{
		Agent: profile.Agent,
		Model: profile.Model,
		Yolo:  profile.Yolo,
		Mode:  profile.Mode,
		Args:  append([]string(nil), profile.Args...),
		Env:   cloneStringMap(profile.Env),
		MCP:   snapshotMCPServers(profile.MCP),
	}
}

func snapshotMCPServers(src map[string]config.MCPServerConfig) map[string]domain.MCPServerSnapshot {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]domain.MCPServerSnapshot, len(src))
	for name, server := range src {
		dst[name] = domain.MCPServerSnapshot{
			Command:           server.Command,
			Args:              append([]string(nil), server.Args...),
			Env:               cloneStringMap(server.Env),
			CWD:               server.CWD,
			URL:               server.URL,
			BearerTokenEnvVar: server.BearerTokenEnvVar,
		}
	}
	return dst
}

func mcpServersFromSnapshot(src map[string]domain.MCPServerSnapshot) map[string]config.MCPServerConfig {
	if len(src) == 0 {
		return map[string]config.MCPServerConfig{}
	}
	dst := make(map[string]config.MCPServerConfig, len(src))
	for name, server := range src {
		dst[name] = config.MCPServerConfig{
			Command:           server.Command,
			Args:              append([]string(nil), server.Args...),
			Env:               cloneStringMap(server.Env),
			CWD:               server.CWD,
			URL:               server.URL,
			BearerTokenEnvVar: server.BearerTokenEnvVar,
		}
	}
	return dst
}

func (s *Service) defaultShell() string {
	if shell := strings.TrimSpace(s.cfg.Shell); shell != "" {
		return shell
	}
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "bash"
}

func (s *Service) startAutoTerminals(ctx context.Context, cfg config.Config, session domain.Session, workdir string, env map[string]string, size terminalSize, cleanupPaths []string) []sessionTabState {
	tabs, ok := cfg.Tabs[string(session.SessionType)]
	if !ok || len(tabs) == 0 {
		tabs = defaultTabsBySessionType[string(session.SessionType)]
	}
	if len(tabs) == 0 {
		return nil
	}
	layout := make([]sessionTabState, 0, len(tabs))
	for _, tab := range tabs {
		if tab.Type != "terminal" {
			layout = append(layout, sessionTabState{tab: domain.SessionTab{Type: tab.Type}})
			continue
		}
		terminalID := uuid.NewString()
		cmd := tab.Command
		if cmd == "" {
			cmd = s.defaultShell()
		}
		status := "running"
		err := s.terminal.Start(ctx, terminalStartSpec{
			SessionID:    session.ID,
			TerminalID:   terminalID,
			Command:      cmd,
			Env:          cloneStringMap(env),
			Workdir:      workdir,
			Size:         size,
			CleanupPaths: append([]string(nil), cleanupPaths...),
			OnExit:       s.handleAuxTerminalExit,
		})
		if err != nil {
			status = "exited"
			s.logger.Warn("[spawn] auto-create terminal failed",
				"session_id", session.ID,
				"terminal_id", terminalID,
				"command", cmd,
				"error", err,
			)
		}
		layout = append(layout, sessionTabState{tab: domain.SessionTab{Type: tab.Type, ID: terminalID, Command: cmd, Status: status, Placement: domain.TerminalPlacementTab}})
	}
	return layout
}

func (s *Service) storeSessionTerminalState(sessionID string, state sessionTerminalState) {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	if s.terminalStates == nil {
		s.terminalStates = map[string]sessionTerminalState{}
	}
	s.terminalStates[sessionID] = sessionTerminalState{
		runID:          state.runID,
		mainTerminalID: state.mainTerminalID,
		tabs:           cloneSessionTabStates(state.tabs),
	}
}

func (s *Service) replaceSessionMainTerminalID(sessionID, mainTerminalID string) error {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	state, ok := s.terminalStates[sessionID]
	if !ok {
		return fmt.Errorf("session terminal state not found for %s", sessionID)
	}
	state.mainTerminalID = mainTerminalID
	s.terminalStates[sessionID] = sessionTerminalState{
		runID:          state.runID,
		mainTerminalID: state.mainTerminalID,
		tabs:           cloneSessionTabStates(state.tabs),
	}
	return nil
}

func (s *Service) sessionTabs(sessionID string) []domain.SessionTab {
	s.terminalStateMu.RLock()
	defer s.terminalStateMu.RUnlock()
	state, ok := s.terminalStates[sessionID]
	if !ok {
		return nil
	}
	return cloneSessionTabs(state.tabs)
}

func (s *Service) mainTerminalID(sessionID string) string {
	s.terminalStateMu.RLock()
	defer s.terminalStateMu.RUnlock()
	return s.terminalStates[sessionID].mainTerminalID
}

func (s *Service) removeSessionTerminalState(sessionID string) {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	delete(s.terminalStates, sessionID)
}

func (s *Service) appendSessionTab(sessionID string, tab sessionTabState) {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	state := s.terminalStates[sessionID]
	state.tabs = append(state.tabs, cloneSessionTabState(tab))
	s.terminalStates[sessionID] = state
}

func (s *Service) removeSessionTabOnTerminalClose(sessionID, terminalID string) {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	state, ok := s.terminalStates[sessionID]
	if !ok {
		return
	}
	for i, tab := range state.tabs {
		if tab.tab.ID != terminalID || !tab.removeOnExit {
			continue
		}
		state.tabs = append(state.tabs[:i], state.tabs[i+1:]...)
		s.terminalStates[sessionID] = state
		return
	}
}

func (s *Service) setSessionTabTarget(sessionID, tabID, target string) bool {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	state, ok := s.terminalStates[sessionID]
	if !ok {
		return false
	}
	for i, tab := range state.tabs {
		if tab.tab.Type != "browser" || tab.tab.ID != tabID {
			continue
		}
		state.tabs[i].tab.Target = target
		return true
	}
	return false
}

func (s *Service) removeSessionTabByID(sessionID, tabID string) bool {
	s.terminalStateMu.Lock()
	defer s.terminalStateMu.Unlock()
	state, ok := s.terminalStates[sessionID]
	if !ok {
		return false
	}
	for i, tab := range state.tabs {
		if tab.tab.Type != "browser" || tab.tab.ID != tabID {
			continue
		}
		state.tabs = append(state.tabs[:i], state.tabs[i+1:]...)
		s.terminalStates[sessionID] = state
		return true
	}
	return false
}

func cloneSessionTabs(tabs []sessionTabState) []domain.SessionTab {
	if len(tabs) == 0 {
		return nil
	}
	cloned := make([]domain.SessionTab, len(tabs))
	for i, tab := range tabs {
		cloned[i] = tab.tab
	}
	return cloned
}

func cloneSessionTabStates(tabs []sessionTabState) []sessionTabState {
	if len(tabs) == 0 {
		return nil
	}
	cloned := make([]sessionTabState, len(tabs))
	for i, tab := range tabs {
		cloned[i] = cloneSessionTabState(tab)
	}
	return cloned
}

func cloneSessionTabState(tab sessionTabState) sessionTabState {
	return sessionTabState{tab: tab.tab, removeOnExit: tab.removeOnExit}
}

func sessionTabID(tab domain.SessionTab) string {
	if tab.ID != "" {
		return tab.ID
	}
	return tab.Type
}

func (s *Service) hydrateSession(session domain.Session) domain.Session {
	if session.CurrentRun != nil {
		session.CurrentRun.MainTerminalID = s.mainTerminalID(session.ID)
	}
	return session
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

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
