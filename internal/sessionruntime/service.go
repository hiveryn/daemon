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
	configSource   config.Source
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

	terminalStateMu sync.RWMutex
	terminalStates  map[string]sessionTerminalState
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
		eventStreams:   map[string]map[uint64]chan domain.SessionEvent{},
		bridgeCancels:  map[string]func(){},
		terminalStates: map[string]sessionTerminalState{},
	}, nil
}

func (s *Service) IngestHandler() http.Handler {
	return s.ingestHTTP
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

func (s *Service) CreateIntent(ctx context.Context, req domain.CreateSessionIntentRequest) (domain.SessionIntent, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return domain.SessionIntent{}, err
	}

	architect, ok := cfg.Architects[req.ArchitectKey]
	if !ok {
		return domain.SessionIntent{}, &domain.NotFoundError{Resource: "architect", ID: req.ArchitectKey}
	}

	switch req.SessionType {
	case domain.SessionTypeArchitect:
		if strings.TrimSpace(req.TicketID) != "" {
			return domain.SessionIntent{}, &domain.ValidationError{Field: "ticket_id", Message: "architect intents do not accept a ticket ID"}
		}
		systemContent, kickoffContent, err := loadArchitectPrompts(req.ArchitectKey, architect, cfg)
		if err != nil {
			return domain.SessionIntent{}, err
		}
		return s.repo.CreateIntent(ctx, domain.CreateSessionIntentParams{
			ArchitectKey: req.ArchitectKey,
			SessionType:  domain.SessionTypeArchitect,
			Prompt:       kickoffContent,
			Instructions: systemContent,
			CreatedBy:    domain.SessionCreatedByDesktop,
		})
	case domain.SessionTypeWork:
		if strings.TrimSpace(req.TicketID) == "" {
			return domain.SessionIntent{}, &domain.ValidationError{Field: "ticket_id", Message: "is required"}
		}
		ticket, err := s.tickets.GetTicket(ctx, architect.Path, req.TicketID)
		if err != nil {
			return domain.SessionIntent{}, err
		}
		if ticket.Status != domain.TicketStatusBacklog {
			return domain.SessionIntent{}, &domain.ValidationError{Field: "ticket_id", Message: "ticket must be in backlog to create a work intent"}
		}
		repoKey := ticket.Repo
		if repoKey == "" {
			return domain.SessionIntent{}, &domain.ValidationError{Field: "repo", Message: "ticket has no repo key in frontmatter"}
		}
		repoPath, ok := architect.Repos[repoKey]
		if !ok {
			return domain.SessionIntent{}, &domain.ValidationError{Field: "repo", Message: "repo key " + repoKey + " not configured in architect repos"}
		}
		if err := validateRepoPath(repoPath); err != nil {
			return domain.SessionIntent{}, err
		}
		kickoffContent, err := loadWorkerPrompt(req.ArchitectKey, architect, cfg, ticket)
		if err != nil {
			return domain.SessionIntent{}, err
		}
		return s.repo.CreateIntent(ctx, domain.CreateSessionIntentParams{
			ArchitectKey: req.ArchitectKey,
			SessionType:  domain.SessionTypeWork,
			TicketID:     req.TicketID,
			Prompt:       kickoffContent,
			CreatedBy:    domain.SessionCreatedByDesktop,
		})
	default:
		return domain.SessionIntent{}, &domain.ValidationError{Field: "session_type", Message: "must be 'architect' or 'work'"}
	}
}

func (s *Service) CreateRun(ctx context.Context, intentID string, req domain.CreateSessionRunRequest) (domain.CreateSessionRunResult, error) {
	intent, err := s.repo.GetIntent(ctx, intentID)
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	cfg, err := s.currentConfig()
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	architect, ok := cfg.Architects[intent.ArchitectKey]
	if !ok {
		return domain.CreateSessionRunResult{}, &domain.NotFoundError{Resource: "architect", ID: intent.ArchitectKey}
	}

	profile, ok := cfg.Variants[req.ProfileName]
	if !ok {
		return domain.CreateSessionRunResult{}, &domain.NotFoundError{Resource: "agent_profile", ID: req.ProfileName}
	}

	agentKind, err := parseAgentKind(profile.Agent)
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	workdir, err := s.resolveIntentWorkdir(ctx, intent, architect)
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	run, err := s.repo.CreateRun(ctx, domain.CreateSessionRunParams{
		SessionIntentID: intent.ID,
		ProfileName:     req.ProfileName,
		ProfileSnapshot: snapshotVariant(profile),
		Workdir:         workdir,
		StartedAt:       time.Now().UTC(),
	})
	if err != nil {
		return domain.CreateSessionRunResult{}, err
	}

	s.logger.Info("[spawn] starting session run PTY",
		"session_intent_id", intent.ID,
		"run_id", run.ID,
		"session_type", intent.SessionType,
		"requested_cols", req.Cols,
		"requested_rows", req.Rows,
	)

	mainTerminalID, err := s.launchSession(ctx, intent, run, profile, agentKind, agentruntime.StartRequest{
		Prompt:       intent.Prompt,
		Instructions: intent.Instructions,
		Workdir:      workdir,
		Args:         append([]string(nil), profile.Args...),
		Env:          cloneStringMap(profile.Env),
	}, terminalSize{Cols: req.Cols, Rows: req.Rows})
	if err != nil {
		s.markRunFailed(run.ID, domain.SessionRunFailureLaunchFailed, "launch")
		return domain.CreateSessionRunResult{}, fmt.Errorf("launch session run: %w", err)
	}

	if intent.SessionType == domain.SessionTypeWork {
		if _, err := s.tickets.MoveTicket(ctx, architect.Path, intent.TicketID, domain.MoveTicketParams{To: domain.TicketStatusProgress}); err != nil {
			if killErr := s.terminal.KillBySession(ctx, intent.ID); killErr != nil && !errors.Is(killErr, errTerminalNotFound) {
				return domain.CreateSessionRunResult{}, fmt.Errorf("move ticket %s to progress: %w (also failed to kill terminals: %v)", intent.TicketID, err, killErr)
			}
			s.cancelReceiverBridge(intent.ID)
			s.removeSessionTerminalState(intent.ID)
			s.markRunFailed(run.ID, domain.SessionRunFailureLaunchFailed, "move_ticket_to_progress")
			return domain.CreateSessionRunResult{}, fmt.Errorf("move ticket %s to progress: %w", intent.TicketID, err)
		}
	}

	return domain.CreateSessionRunResult{Run: run, MainTerminalID: mainTerminalID}, nil
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

func (s *Service) launchSession(ctx context.Context, intent domain.SessionIntent, run domain.SessionRun, profile config.VariantConfig, agentKind agentruntime.AgentKind, startReq agentruntime.StartRequest, size terminalSize) (string, error) {
	mainTerminalID, spec, err := s.startSessionMainTerminal(ctx, intent, run, profile, agentKind, startReq, size)
	if err != nil {
		return "", err
	}

	s.storeSessionTerminalState(intent.ID, sessionTerminalState{
		runID:          run.ID,
		mainTerminalID: mainTerminalID,
		tabs:           s.startAutoTerminals(ctx, intent, spec.Workdir, spec.Env, size, spec.CleanupPaths),
	})

	return mainTerminalID, nil
}

func (s *Service) RestoreRunningSessions(ctx context.Context) error {
	intents, err := s.repo.ListIntents(ctx)
	if err != nil {
		return fmt.Errorf("list session intents for restore: %w", err)
	}

	if len(intents) == 0 {
		return nil
	}

	restoredCount := 0
	for _, intent := range intents {
		if intent.CurrentRun == nil || intent.CurrentRun.Status != domain.SessionRunStatusRunning {
			continue
		}
		run := *intent.CurrentRun
		if err := s.restoreSession(ctx, intent, run); err != nil {
			if markErr := s.repo.MarkRunFailed(ctx, run.ID, domain.SessionRunFailureRestoreFailed); markErr != nil {
				return fmt.Errorf("restore session intent %s failed: %w (also failed to mark run failed: %v)", intent.ID, err, markErr)
			}
			return fmt.Errorf("restore session intent %s run %s: %w", intent.ID, run.ID, err)
		}
		restoredCount++
		s.logger.Info("session run restored",
			"session_intent_id", intent.ID,
			"run_id", run.ID,
			"session_type", intent.SessionType,
			"architect_key", intent.ArchitectKey,
		)
	}

	if restoredCount > 0 {
		s.logger.Info("restored running session runs", "count", restoredCount)
	}

	return nil
}

func (s *Service) restoreSession(ctx context.Context, intent domain.SessionIntent, run domain.SessionRun) error {
	profile, agentKind, err := s.resolveStoredRunLaunchContext(intent, run)
	if err != nil {
		return err
	}

	if _, err := s.launchSession(ctx, intent, run, profile, agentKind, agentruntime.StartRequest{
		Instructions: intent.Instructions,
		Workdir:      run.Workdir,
		Args:         append([]string(nil), profile.Args...),
		Env:          cloneStringMap(profile.Env),
		Resume:       true,
		ResumeID:     run.NativeID,
	}, terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows}); err != nil {
		return fmt.Errorf("launch session run: %w", err)
	}

	return nil
}

func (s *Service) startSessionMainTerminal(ctx context.Context, intent domain.SessionIntent, run domain.SessionRun, profile config.VariantConfig, agentKind agentruntime.AgentKind, startReq agentruntime.StartRequest, size terminalSize) (string, agentruntime.LaunchSpec, error) {
	spec, err := s.prepareLaunchSpec(ctx, intent, run, profile, agentKind, startReq)
	if err != nil {
		return "", agentruntime.LaunchSpec{}, err
	}

	s.cancelReceiverBridge(intent.ID)
	cancelBridge := s.startReceiverBridge(intent.ID)
	s.storeBridgeCancel(intent.ID, cancelBridge)

	mainTerminalID := uuid.NewString()
	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID:    intent.ID,
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
		s.cancelReceiverBridge(intent.ID)
		return "", agentruntime.LaunchSpec{}, err
	}

	s.logger.Info("[launch] started agent PTY",
		"session_intent_id", intent.ID,
		"run_id", run.ID,
		"session_type", intent.SessionType,
		"command", spec.Command,
	)

	return mainTerminalID, spec, nil
}

func (s *Service) prepareLaunchSpec(ctx context.Context, intent domain.SessionIntent, _ domain.SessionRun, profile config.VariantConfig, agentKind agentruntime.AgentKind, startReq agentruntime.StartRequest) (agentruntime.LaunchSpec, error) {
	startReq.ID = intent.ID
	startReq.Agent = agentKind

	if err := configureOpenCodeArchitectAgent(intent, agentKind, &startReq); err != nil {
		return agentruntime.LaunchSpec{}, err
	}

	adapter := s.adapters[agentKind]
	if _, err := adapter.EnsureSetup(ctx, setupRequestForAgent(agentKind, s.baseURL+ingestPathPrefix, profile.Env)); err != nil {
		return agentruntime.LaunchSpec{}, fmt.Errorf("ensure %s setup: %w", agentKind, err)
	}

	mcpServers, err := s.mcpServersForSession(intent.SessionType, intent.ArchitectKey, intent.ID)
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

func configureOpenCodeArchitectAgent(intent domain.SessionIntent, agentKind agentruntime.AgentKind, startReq *agentruntime.StartRequest) error {
	if agentKind != agentruntime.AgentOpenCode || intent.SessionType != domain.SessionTypeArchitect {
		return nil
	}
	if strings.TrimSpace(intent.ArchitectKey) == "" {
		return errors.New("architect OpenCode session missing architect key")
	}
	if hasArgFlag(startReq.Args, "--agent") {
		return fmt.Errorf("architect OpenCode session profile args must not include --agent; daemon manages the agent selection for architect %q", intent.ArchitectKey)
	}

	architectKey := intent.ArchitectKey
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

func (s *Service) resolveStoredRunLaunchContext(intent domain.SessionIntent, run domain.SessionRun) (config.VariantConfig, agentruntime.AgentKind, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return config.VariantConfig{}, "", err
	}

	if _, ok := cfg.Architects[intent.ArchitectKey]; !ok {
		return config.VariantConfig{}, "", fmt.Errorf("architect %q not found in architects.yaml", intent.ArchitectKey)
	}
	if _, ok := cfg.Variants[run.ProfileName]; !ok {
		return config.VariantConfig{}, "", fmt.Errorf("agent profile %q not found in variants.yaml", run.ProfileName)
	}
	if run.ProfileSnapshot == nil {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s has no profile snapshot", run.ID)
	}
	if strings.TrimSpace(run.NativeID) == "" {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s has no native ID", run.ID)
	}
	if strings.TrimSpace(run.Workdir) == "" {
		return config.VariantConfig{}, "", fmt.Errorf("session run %s has no workdir", run.ID)
	}

	snapshot := *run.ProfileSnapshot
	agentKind, err := parseAgentKind(snapshot.Agent)
	if err != nil {
		return config.VariantConfig{}, "", fmt.Errorf("unsupported agent %q in snapshot: %w", snapshot.Agent, err)
	}

	return config.VariantConfig{
		Agent: snapshot.Agent,
		Args:  append([]string(nil), snapshot.Args...),
		Env:   cloneStringMap(snapshot.Env),
	}, agentKind, nil
}

func (s *Service) resumeSessionMainTerminal(ctx context.Context, intent domain.SessionIntent, run domain.SessionRun, size terminalSize) (string, error) {
	profile, agentKind, err := s.resolveStoredRunLaunchContext(intent, run)
	if err != nil {
		return "", err
	}

	mainTerminalID, _, err := s.startSessionMainTerminal(ctx, intent, run, profile, agentKind, agentruntime.StartRequest{
		Instructions: intent.Instructions,
		Workdir:      run.Workdir,
		Args:         append([]string(nil), profile.Args...),
		Env:          cloneStringMap(profile.Env),
		Resume:       true,
		ResumeID:     run.NativeID,
	}, size)
	if err != nil {
		return "", err
	}

	if err := s.replaceSessionMainTerminalID(intent.ID, mainTerminalID); err != nil {
		return "", err
	}

	return mainTerminalID, nil
}

func (s *Service) ConcludeSession(ctx context.Context, id string, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	if strings.TrimSpace(params.Body) == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "body", Message: "is required"}
	}

	intent, err := s.repo.GetIntent(ctx, id)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if intent.CurrentRun == nil || intent.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}
	run := *intent.CurrentRun

	switch intent.SessionType {
	case domain.SessionTypeArchitect:
		return s.concludeArchitectSession(ctx, intent, run, params)
	case domain.SessionTypeWork:
		return s.concludeWorkSession(ctx, intent, run, params)
	default:
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_type", Message: "cannot conclude session of type " + string(intent.SessionType)}
	}
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

func (s *Service) concludeArchitectSession(ctx context.Context, intent domain.SessionIntent, run domain.SessionRun, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	activeWorkerSessionIDs, err := s.activeWorkerSessionIDs(ctx, intent.ArchitectKey, intent.ID)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	if len(activeWorkerSessionIDs) > 0 {
		return domain.ConcludeSessionResult{}, &domain.ConflictError{
			Resource: "session",
			Field:    "architect_key",
			Message:  fmt.Sprintf("Cannot conclude architect session: %d worker session(s) still active: %s", len(activeWorkerSessionIDs), strings.Join(activeWorkerSessionIDs, ", ")),
		}
	}

	architect, err := s.currentArchitect(intent.ArchitectKey)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	folderName := intent.CreatedAt.UTC().Format("2006-01-02-1504")
	dir := filepath.Join(architect.Path, "architect-sessions", folderName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return domain.ConcludeSessionResult{}, fmt.Errorf("create architect session directory: %w", err)
	}

	now := time.Now().UTC()
	conclusionPath := filepath.Join(dir, conclusionFileName)
	doc := newArchitectConclusionDocument(intent.CreatedAt, now, run.ProfileName, params.Body)
	if err := writeConclusionFile(conclusionPath, doc); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.repo.MarkRunCompleted(ctx, run.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.appendAndPublishSessionEnded(ctx, intent.ID, run.ID, "session concluded", map[string]any{"body": params.Body}); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.terminal.KillBySession(ctx, intent.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return domain.ConcludeSessionResult{}, fmt.Errorf("kill session terminal: %w", err)
	}

	if err := s.repo.DeleteIntent(ctx, intent.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	s.cleanupDeletedSession(intent.ID)

	return domain.ConcludeSessionResult{SessionID: intent.ID, ArchitectKey: intent.ArchitectKey}, nil
}

func (s *Service) activeWorkerSessionIDs(ctx context.Context, architectKey, excludedSessionID string) ([]string, error) {
	intents, err := s.repo.ListIntents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list session intents for architect conclude: %w", err)
	}

	ids := make([]string, 0, len(intents))
	for _, candidate := range intents {
		if candidate.ID == excludedSessionID {
			continue
		}
		if candidate.ArchitectKey != architectKey {
			continue
		}
		if candidate.SessionType != domain.SessionTypeWork {
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

func (s *Service) concludeWorkSession(ctx context.Context, intent domain.SessionIntent, run domain.SessionRun, params domain.ConcludeSessionParams) (domain.ConcludeSessionResult, error) {
	if params.Rejected {
		if strings.TrimSpace(params.RejectionReason) == "" {
			return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "rejection_reason", Message: "is required when rejected is true"}
		}
	} else {
		if len(params.Commits) == 0 {
			return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "commits", Message: "are required when not rejected"}
		}
	}

	if intent.TicketID == "" {
		return domain.ConcludeSessionResult{}, &domain.ValidationError{Field: "session_id", Message: "work session has no ticket ID"}
	}

	architect, err := s.currentArchitect(intent.ArchitectKey)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	ticket, err := s.tickets.GetTicket(ctx, architect.Path, intent.TicketID)
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

	resolvedCommits, err := resolveConclusionCommitRefs(ctx, architect.Repos, params.Commits)
	if err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	now := time.Now().UTC()
	startedAt := intent.CreatedAt
	if run.StartedAt != nil {
		startedAt = run.StartedAt.UTC()
	}
	conclusion := domain.TicketConclusion{
		StartedAt:       startedAt,
		ConcludedAt:     now,
		Agent:           run.ProfileName,
		Profile:         run.ProfileName,
		Rejected:        params.Rejected,
		RejectionReason: params.RejectionReason,
		Commits:         resolvedCommits,
		Body:            params.Body,
	}

	if _, err := s.tickets.ConcludeTicket(ctx, architect.Path, intent.TicketID, conclusion); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.repo.MarkRunCompleted(ctx, run.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.appendAndPublishSessionEnded(ctx, intent.ID, run.ID, "session concluded", map[string]any{
		"body":             params.Body,
		"commits":          resolvedCommits,
		"rejected":         params.Rejected,
		"rejection_reason": params.RejectionReason,
	}); err != nil {
		return domain.ConcludeSessionResult{}, err
	}

	if err := s.terminal.KillBySession(ctx, intent.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return domain.ConcludeSessionResult{}, fmt.Errorf("kill session terminal: %w", err)
	}

	if err := s.repo.DeleteIntent(ctx, intent.ID); err != nil {
		return domain.ConcludeSessionResult{}, err
	}
	s.cleanupDeletedSession(intent.ID)

	return domain.ConcludeSessionResult{SessionID: intent.ID, ArchitectKey: intent.ArchitectKey, TicketID: intent.TicketID}, nil
}

func (s *Service) appendAndPublishSessionEnded(ctx context.Context, intentID, runID, message string, raw map[string]any) error {
	raw = cloneAnyMap(raw)
	raw["lifecycle"] = "concluded"
	raw["session_intent_id"] = intentID
	if runID != "" {
		raw["run_id"] = runID
	}
	raw["message"] = message
	return s.appendAndPublishSessionEvent(ctx, domain.AppendSessionEventParams{
		SessionIntentID: intentID,
		RunID:           runID,
		Type:            "status",
		Status:          "ended",
		Message:         message,
		Raw:             raw,
		At:              time.Now().UTC(),
	})
}

func (s *Service) appendAndPublishSessionEvent(ctx context.Context, params domain.AppendSessionEventParams) error {
	event, err := s.repo.AppendSessionEvent(ctx, params)
	if err != nil {
		return fmt.Errorf("append session event: %w", err)
	}
	s.publishEvent(params.SessionIntentID, event)
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
	if _, err := s.repo.GetIntent(ctx, id); err != nil {
		return err
	}

	if err := s.terminal.KillBySession(ctx, id); err != nil && !errors.Is(err, errTerminalNotFound) {
		return err
	}

	if err := s.repo.DeleteIntent(ctx, id); err != nil {
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

func (s *Service) GetIntent(ctx context.Context, id string) (domain.SessionIntent, error) {
	intent, err := s.repo.GetIntent(ctx, id)
	if err != nil {
		return domain.SessionIntent{}, err
	}
	return s.hydrateIntent(intent), nil
}

func (s *Service) ListIntents(ctx context.Context) ([]domain.SessionIntent, error) {
	intents, err := s.repo.ListIntents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range intents {
		intents[i] = s.hydrateIntent(intents[i])
	}
	return intents, nil
}

func (s *Service) ListSessionEvents(ctx context.Context, id string) ([]domain.SessionEvent, error) {
	return s.repo.ListSessionEvents(ctx, id)
}

func (s *Service) SubscribeSessionEvents(ctx context.Context, id string) (domain.SessionEventSubscription, error) {
	if _, err := s.repo.GetIntent(ctx, id); err != nil {
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
	if _, err := s.repo.GetIntent(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.terminal.Attach(ctx, sessionID, terminalID)
}

func (s *Service) CreateTerminal(ctx context.Context, sessionID string, _ domain.CreateTerminalParams) (domain.TerminalInfo, error) {
	intent, err := s.repo.GetIntent(ctx, sessionID)
	if err != nil {
		return domain.TerminalInfo{}, err
	}
	if intent.CurrentRun == nil || intent.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.TerminalInfo{}, &domain.ValidationError{Field: "session_id", Message: "session is not running"}
	}
	if strings.TrimSpace(intent.CurrentRun.Workdir) == "" {
		return domain.TerminalInfo{}, fmt.Errorf("session intent %s current run has no workdir", intent.ID)
	}

	command := s.defaultShell()

	terminalID := uuid.NewString()
	if err := s.terminal.Start(ctx, terminalStartSpec{
		SessionID:  sessionID,
		TerminalID: terminalID,
		Command:    command,
		Workdir:    intent.CurrentRun.Workdir,
		Size:       terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows},
		OnExit:     s.handleAuxTerminalExit,
	}); err != nil {
		return domain.TerminalInfo{}, err
	}

	s.appendSessionTab(sessionID, sessionTabState{
		tab: domain.SessionTab{
			Type:       "terminal",
			TerminalID: terminalID,
			Command:    command,
			Status:     "running",
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
	if _, err := s.repo.GetIntent(ctx, sessionID); err != nil {
		return nil, err
	}
	return s.terminal.ListBySession(sessionID), nil
}

func (s *Service) ListSessionTabs(ctx context.Context, sessionID string) ([]domain.SessionTab, error) {
	if _, err := s.repo.GetIntent(ctx, sessionID); err != nil {
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
		if terminal, ok := byTerminalID[tabs[i].TerminalID]; ok {
			tabs[i].Status = terminal.Status
			if terminal.Command != "" {
				tabs[i].Command = terminal.Command
			}
		}
	}

	return tabs, nil
}

func (s *Service) KillTerminal(ctx context.Context, sessionID, terminalID string) error {
	if _, err := s.repo.GetIntent(ctx, sessionID); err != nil {
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

func (s *Service) Shutdown(ctx context.Context) error {
	terminalErr := s.terminal.Shutdown(ctx)
	s.cancelReceiverBridges()
	s.closeEventSubscribers("")
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
	run, err := s.repo.GetCurrentRun(context.Background(), event.ID)
	if err != nil {
		panic(fmt.Errorf("get current run for session intent %s on receiver event: %w", event.ID, err))
	}
	if run == nil || run.Status != domain.SessionRunStatusRunning {
		panic(fmt.Errorf("receiver event for session intent %s has no running run", event.ID))
	}

	persisted, err := s.repo.AppendSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionIntentID:   event.ID,
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
		panic(fmt.Errorf("persist session event for intent %s run %s: %w", event.ID, run.ID, err))
	}

	primaryNativeID := event.PrimaryNativeID
	if primaryNativeID == "" {
		primaryNativeID = event.NativeID
	}
	if primaryNativeID != "" {
		if err := s.repo.UpdateRunNativeID(context.Background(), run.ID, primaryNativeID); err != nil {
			panic(fmt.Errorf("update session run native id for intent %s run %s: %w", event.ID, run.ID, err))
		}
	}

	s.publishEvent(event.ID, persisted)
}

func (s *Service) handleTerminalExit(exit terminalExit) {
	intent, err := s.repo.GetIntent(context.Background(), exit.SessionID)
	if err != nil {
		panic(fmt.Errorf("get session intent %s after main terminal exit: %w", exit.SessionID, err))
	}
	if intent.CurrentRun == nil || intent.CurrentRun.Status != domain.SessionRunStatusRunning {
		panic(fmt.Errorf("main terminal exited for non-running session intent %s", exit.SessionID))
	}
	run := *intent.CurrentRun

	mainTerminalID, err := s.resumeSessionMainTerminal(context.Background(), intent, run, terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows})
	if err != nil {
		panic(fmt.Errorf("resume session intent %s run %s after main terminal exit: %w", exit.SessionID, run.ID, err))
	}

	if err := s.appendAndPublishSessionEvent(context.Background(), domain.AppendSessionEventParams{
		SessionIntentID: exit.SessionID,
		RunID:           run.ID,
		Type:            "main_terminal_resumed",
		Message:         "main terminal resumed",
		Raw: map[string]any{
			"main_terminal_id":     mainTerminalID,
			"previous_terminal_id": exit.TerminalID,
			"exit_error":           errorString(exit.Err),
		},
		At: time.Now().UTC(),
	}); err != nil {
		panic(fmt.Errorf("append main terminal resumed event for session intent %s run %s: %w", exit.SessionID, run.ID, err))
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

func snapshotVariant(profile config.VariantConfig) domain.AgentProfileSnapshot {
	return domain.AgentProfileSnapshot{
		Agent: profile.Agent,
		Args:  append([]string(nil), profile.Args...),
		Env:   cloneStringMap(profile.Env),
	}
}

func (s *Service) resolveIntentWorkdir(ctx context.Context, intent domain.SessionIntent, architect config.ArchitectConfig) (string, error) {
	switch intent.SessionType {
	case domain.SessionTypeArchitect:
		return architect.Path, nil
	case domain.SessionTypeWork:
		if intent.TicketID == "" {
			return "", errors.New("work session has no ticket ID")
		}
		ticket, err := s.tickets.GetTicket(ctx, architect.Path, intent.TicketID)
		if err != nil {
			return "", fmt.Errorf("get ticket %q: %w", intent.TicketID, err)
		}
		if ticket.Repo == "" {
			return "", errors.New("ticket has no repo key")
		}
		repoPath, ok := architect.Repos[ticket.Repo]
		if !ok {
			return "", fmt.Errorf("repo key %q not configured in architect repos", ticket.Repo)
		}
		if err := validateRepoPath(repoPath); err != nil {
			return "", err
		}
		return repoPath, nil
	default:
		return "", fmt.Errorf("unsupported session type %q", intent.SessionType)
	}
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

func (s *Service) startAutoTerminals(ctx context.Context, intent domain.SessionIntent, workdir string, env map[string]string, size terminalSize, cleanupPaths []string) []sessionTabState {
	tabs, ok := s.cfg.Tabs[string(intent.SessionType)]
	if !ok || len(tabs) == 0 {
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
			SessionID:    intent.ID,
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
				"session_intent_id", intent.ID,
				"terminal_id", terminalID,
				"command", cmd,
				"error", err,
			)
		}
		layout = append(layout, sessionTabState{tab: domain.SessionTab{Type: tab.Type, TerminalID: terminalID, Command: cmd, Status: status}})
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
		if tab.tab.TerminalID != terminalID || !tab.removeOnExit {
			continue
		}
		state.tabs = append(state.tabs[:i], state.tabs[i+1:]...)
		s.terminalStates[sessionID] = state
		return
	}
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

func (s *Service) hydrateIntent(intent domain.SessionIntent) domain.SessionIntent {
	if intent.CurrentRun != nil {
		intent.CurrentRun.MainTerminalID = s.mainTerminalID(intent.ID)
	}
	return intent
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
