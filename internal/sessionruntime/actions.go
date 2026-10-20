package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/actionfs"
	"github.com/hiveryn/daemon/internal/actionpkg"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Actions run as sessions of type action: one agent session per execution, in
// the action's registered checkout on its machine (actions.yaml; local or an
// SSH machine), with an external working output directory when artifacts are declared.
// The execution itself is an ActionRun record, addressed by its stable id,
// that outlives the session:
//
//	launch        record inserted running (the single-run rule), output
//	              directory created on the execution machine, session created
//	              and its run launched (a remote one over SSH/tmux like a
//	              remote worker); an architect's approved request
//	              (actions_architect.go) moves its pending_approval record to
//	              running and launches the same way
//	prepare       deliverArtifacts freezes a private, replaceable snapshot
//	conclude      timed approval publishes the proposed snapshot, if any,
//	              together with the final status. Completed requires a package
//	              only when promised; failed may publish partial results.
//	              Denial retains the preparation and leaves the agent running.
//	cancel        the user stops a running execution: failed, session ended
//	launch error  failed with the launch error; the session is removed
//	PTY exit      the agent is resumed like every session; a failed resume
//	              fails the execution (a remote one reattaches instead)
//
// Whatever takes an agent-requested execution out of running — conclusion,
// cancel, launch/resume failure or the restart reconciliation — tells its
// requester once through the durable agent-message outbox (messages_action.go).
//	restart       running action sessions are restored like every session; a
//	              failed local restore fails the execution, and
//	              ReconcileActionRuns fails any execution left running without
//	              a live session
//
// Unlike deferred intents, execution history is never pruned and a restart
// does not fail executions whose agent session resumes. Artifact packages
// expire (actions_artifacts.go); the records stay.

const (
	// actionConclusionHistory is how many earlier summaries an action agent
	// can read back.
	actionConclusionHistory = 5
	// actionOutputAdditionalRepoKey labels the output directory in the
	// session's additional workdir scope.
	actionOutputAdditionalRepoKey = "output"
	// actionListInspectTimeout bounds inspecting one remote checkout for a
	// listing, which must answer within the desktop's request bound even
	// when a machine is unreachable; the definition then says so.
	actionListInspectTimeout = 4 * time.Second
	// actionInspectTimeout bounds inspecting a remote checkout before a
	// launch or a request.
	actionInspectTimeout = 30 * time.Second
	// remoteActionsStateDir is the managed directory in a remote account's
	// home: runs/<action>/<id> working directories and artifacts/.
	remoteActionsStateDir = ".local/state/hiveryn-actions"
)

type actionRuntime struct {
	runs     domain.ActionRunRepository
	registry string
	// runsRoot holds local working output directories; artifactsRoot the
	// local managed artifact directories (actions_artifacts.go).
	runsRoot      string
	artifactsRoot string
	// launches serializes the launches of each action, so the busy check, the
	// output directory and the session are created as one step per action; a
	// slow remote launch never holds up another action's. The store's
	// single-running rule stays the backstop.
	launchesMu sync.Mutex
	launches   map[string]*sync.Mutex

	artifacts artifactState
}

// ActionPaths are the local files and directories the Actions runtime uses.
type ActionPaths struct {
	// Registry is actions.yaml.
	Registry string
	// Runs holds local working output directories, <Runs>/<action>/<id>.
	Runs string
	// Artifacts holds local managed packages and deliveries.
	Artifacts string
}

// SetActions enables the Actions runtime: executions are persisted in runs
// and the registry, working directories and managed artifacts are at paths.
func (s *Service) SetActions(runs domain.ActionRunRepository, paths ActionPaths) {
	s.actions = &actionRuntime{runs: runs, registry: paths.Registry, runsRoot: paths.Runs, artifactsRoot: paths.Artifacts}
}

// lockLaunch takes the launch lock of one action and returns its release.
func (rt *actionRuntime) lockLaunch(action string) func() {
	rt.launchesMu.Lock()
	if rt.launches == nil {
		rt.launches = map[string]*sync.Mutex{}
	}
	mu, ok := rt.launches[action]
	if !ok {
		mu = &sync.Mutex{}
		rt.launches[action] = mu
	}
	rt.launchesMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Service) actionRuntime() (*actionRuntime, error) {
	if s.actions == nil {
		return nil, errors.New("actions runtime is not configured")
	}
	return s.actions, nil
}

// actionLocation is where a registered Action lives and runs.
type actionLocation struct {
	machine string
	ssh     string
	path    string
}

// registeredAction resolves name in the current actions.yaml. An unknown name
// is a NotFoundError; a machine missing from machines.yaml cannot occur, the
// config loader refuses it.
func (s *Service) registeredAction(name string) (actionLocation, config.Config, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return actionLocation{}, config.Config{}, err
	}
	reg, ok := cfg.Actions[name]
	if !ok {
		return actionLocation{}, cfg, &domain.NotFoundError{Resource: "action", ID: name}
	}
	return actionLocation{machine: reg.Machine, ssh: cfg.Machines[reg.Machine].SSH, path: reg.Path}, cfg, nil
}

// inspectAction reads the registered checkout's definition where it lives,
// with its eligible variants.
func inspectAction(ctx context.Context, cfg config.Config, name string, loc actionLocation, timeout time.Duration) actionfs.Definition {
	var def actionfs.Definition
	if loc.machine == "" {
		def = actionfs.InspectLocal(loc.path, name)
	} else {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		def = actionfs.InspectRemote(ctx, loc.ssh, loc.machine, loc.path, name)
		cancel()
	}
	def.Variants = cfg.VariantsForMachine(loc.machine)
	return def
}

// actionDefinition resolves and inspects one registered Action.
func (s *Service) actionDefinition(ctx context.Context, name string, timeout time.Duration) (actionfs.Definition, actionLocation, error) {
	loc, cfg, err := s.registeredAction(name)
	if err != nil {
		return actionfs.Definition{}, actionLocation{}, err
	}
	return inspectAction(ctx, cfg, name, loc, timeout), loc, nil
}

func (s *Service) ListActions(ctx context.Context) (domain.ActionList, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionList{}, err
	}
	cfg, err := s.currentConfig()
	if err != nil {
		return domain.ActionList{}, err
	}
	running, err := rt.runs.RunningActionRuns(ctx)
	if err != nil {
		return domain.ActionList{}, err
	}
	names := make([]string, 0, len(cfg.Actions))
	for name := range cfg.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	// Remote checkouts are inspected concurrently, each bounded, so one
	// unreachable machine costs one timeout, not one per Action.
	defs := make([]domain.ActionDefinition, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		reg := cfg.Actions[name]
		loc := actionLocation{machine: reg.Machine, ssh: cfg.Machines[reg.Machine].SSH, path: reg.Path}
		wg.Add(1)
		go func() {
			defer wg.Done()
			item := inspectAction(ctx, cfg, name, loc, actionListInspectTimeout).ActionDefinition
			if run, ok := running[name]; ok {
				item.RunningExecutionID = run.ID
			}
			defs[i] = item
		}()
	}
	wg.Wait()
	return domain.ActionList{Registry: rt.registry, Actions: defs}, nil
}

func (s *Service) GetAction(ctx context.Context, name string) (domain.ActionDefinition, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionDefinition{}, err
	}
	def, _, err := s.actionDefinition(ctx, name, actionListInspectTimeout)
	if err != nil {
		return domain.ActionDefinition{}, err
	}
	running, err := rt.runs.RunningActionRuns(ctx)
	if err != nil {
		return domain.ActionDefinition{}, err
	}
	item := def.ActionDefinition
	if run, ok := running[item.Name]; ok {
		item.RunningExecutionID = run.ID
	}
	return item, nil
}

func (s *Service) ListActionRuns(ctx context.Context, action string, limit int) ([]domain.ActionRun, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return nil, err
	}
	runs, err := rt.runs.ListActionRuns(ctx, action, limit)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range runs {
		if runs[i], err = s.withArtifacts(ctx, runs[i], now); err != nil {
			return nil, err
		}
		runs[i] = s.withAttention(runs[i])
	}
	return runs, nil
}

func (s *Service) GetActionRun(ctx context.Context, id string) (domain.ActionRun, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionRun{}, err
	}
	run, err := rt.runs.GetActionRun(ctx, id)
	if err != nil {
		return domain.ActionRun{}, err
	}
	if run, err = s.withArtifacts(ctx, run, time.Now().UTC()); err != nil {
		return domain.ActionRun{}, err
	}
	return s.withAttention(run), nil
}

// LaunchAction manually starts one execution. The user's launch is the
// approval, so the execution is recorded running immediately; it stays running
// after the agent starts, until the agent concludes or the execution fails.
func (s *Service) LaunchAction(ctx context.Context, name string, req domain.LaunchActionRequest) (domain.LaunchActionResult, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.LaunchActionResult{}, err
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return domain.LaunchActionResult{}, &domain.ValidationError{Field: "prompt", Message: "is required"}
	}
	if strings.TrimSpace(req.ProfileName) == "" {
		return domain.LaunchActionResult{}, &domain.ValidationError{Field: "profile_name", Message: "is required"}
	}
	loc, _, err := s.registeredAction(name)
	if err != nil {
		return domain.LaunchActionResult{}, err
	}
	if err := s.checkActionVariant(req.ProfileName, loc.machine); err != nil {
		return domain.LaunchActionResult{}, err
	}

	defer rt.lockLaunch(name)()

	def, loc, err := s.actionDefinition(ctx, name, actionInspectTimeout)
	if err != nil {
		return domain.LaunchActionResult{}, err
	}
	if !def.Valid {
		return domain.LaunchActionResult{}, actionfs.InvalidError(def)
	}
	id := uuid.NewString()
	outputDir, err := s.actionOutputDir(ctx, loc, def.Name, id)
	if err != nil {
		return domain.LaunchActionResult{}, err
	}

	if def.ArtifactMode == "none" {
		outputDir = ""
	}
	now := time.Now().UTC()
	run := domain.ActionRun{
		ID:               id,
		Action:           def.Name,
		Trigger:          domain.ActionRunTriggerManual,
		Status:           domain.ActionRunRunning,
		Prompt:           prompt,
		ProfileName:      req.ProfileName,
		RepoPath:         loc.path,
		OutputDir:        outputDir,
		ArtifactContract: def.Artifacts,
		ArtifactMode:     def.ArtifactMode,
		Machine:          loc.machine,
		CreatedAt:        now,
		StartedAt:        &now,
	}
	if err := rt.runs.CreateActionRun(ctx, run); err != nil {
		return domain.LaunchActionResult{}, err
	}
	return s.launchActionSession(ctx, def, loc, run, req.Cols, req.Rows)
}

// checkActionVariant reports whether profileName names a launchable variant
// for machine, where the Action runs, in the current config.
func (s *Service) checkActionVariant(profileName, machine string) error {
	cfg, err := s.currentConfig()
	if err != nil {
		return err
	}
	profile, err := cfg.VariantForMachine(profileName, machine, "Action")
	if err != nil {
		return err
	}
	if _, err := parseAgentKind(profile.Agent); err != nil {
		return err
	}
	return nil
}

// actionOutputDir is the execution's working output directory on its
// machine: under the local runs root, or in the remote account's managed
// state directory (its home is asked over SSH).
func (s *Service) actionOutputDir(ctx context.Context, loc actionLocation, action, executionID string) (string, error) {
	if loc.machine == "" {
		return filepath.Join(s.actions.runsRoot, action, executionID), nil
	}
	home, err := s.remoteHome(ctx, loc.ssh)
	if err != nil {
		return "", fmt.Errorf("resolve the output directory on %s: %w", config.MachineLabel(loc.machine), err)
	}
	return path.Join(home, remoteActionsStateDir, "runs", action, executionID), nil
}

// launchActionSession starts the agent session of an execution already
// recorded running: the output directory, the session and its run. It is
// shared by manual launches and approved architect requests, and must be
// called with the action's launch lock held.
func (s *Service) launchActionSession(ctx context.Context, def actionfs.Definition, loc actionLocation, run domain.ActionRun, cols, rows uint16) (domain.LaunchActionResult, error) {
	rt := s.actions

	// From here on every failure finishes the record as failed, so a broken
	// launch never leaves the action falsely busy.
	fail := func(stage string, cause error) (domain.LaunchActionResult, error) {
		s.finishActionRunFailed(run.ID, fmt.Sprintf("launch failed (%s): %v", stage, cause))
		return domain.LaunchActionResult{}, cause
	}

	var additionalRepos, additionalWorkdirs []string
	if def.ArtifactMode != "none" {
		if err := actionpkg.MkdirEmpty(ctx, actionpkg.Location{Alias: loc.ssh, Path: run.OutputDir}); err != nil {
			return fail("create output directory", err)
		}
		additionalRepos, additionalWorkdirs = []string{actionOutputAdditionalRepoKey}, []string{run.OutputDir}
	}
	instructions, err := renderBuiltinPrompt(actionSystemPromptName, actionKickoffData{Artifacts: def.Artifacts, ArtifactMode: def.ArtifactMode})
	if err != nil {
		return fail("instructions", err)
	}
	kickoff, err := renderActionKickoff(def, run)
	if err != nil {
		return fail("kickoff", err)
	}
	remoteToken, remotePort, err := newRemoteCredentials(loc.machine)
	if err != nil {
		return fail("remote credentials", err)
	}
	session, err := s.repo.CreateSession(ctx, domain.CreateSessionParams{
		Machine: loc.machine, SSH: loc.ssh, RemoteToken: remoteToken, RemotePort: remotePort,
		SessionType:        domain.SessionTypeAction,
		ContextID:          run.ID,
		Prompt:             kickoff,
		Workdir:            run.RepoPath,
		AdditionalRepos:    additionalRepos,
		AdditionalWorkdirs: additionalWorkdirs,
		Instructions:       instructions,
		CreatedBy:          domain.SessionCreatedByDesktop,
	})
	if err != nil {
		return fail("create session", err)
	}
	if err := rt.runs.SetActionRunSession(ctx, run.ID, session.ID); err != nil {
		s.removeActionSession(session.ID)
		return fail("record session", err)
	}
	run.SessionID = session.ID

	result, err := s.CreateRun(ctx, session.ID, domain.CreateSessionRunRequest{ProfileName: run.ProfileName, Cols: cols, Rows: rows})
	if err != nil {
		s.removeActionSession(session.ID)
		return fail("launch agent", err)
	}
	session, err = s.GetSession(ctx, session.ID)
	if err != nil {
		return domain.LaunchActionResult{}, fmt.Errorf("read launched action session %s: %w", session.ID, err)
	}
	s.publishActionEvent(run, domain.ActionRunRunning)
	s.logger.Info("action execution launched",
		"action", run.Action,
		"execution_id", run.ID,
		"session_id", session.ID,
		"profile", run.ProfileName,
		"output_dir", run.OutputDir,
		"machine", run.Machine,
		"trigger", run.Trigger,
		"architect", run.ArchitectKey,
	)
	return domain.LaunchActionResult{Run: run, Session: session, MainTerminalID: result.MainTerminalID}, nil
}

func renderActionKickoff(def actionfs.Definition, run domain.ActionRun) (string, error) {
	kickoff, err := actionfs.RenderKickoff(def, run.Prompt, run.OutputDir)
	if err != nil {
		return "", err
	}
	join := filepath.Join
	if run.Machine != "" {
		join = path.Join
	}
	return renderBuiltinPrompt(actionKickoffPromptName, actionKickoffData{
		Action:         def.Name,
		ExecutionID:    run.ID,
		Machine:        runsWhere(run.Machine),
		RepoPath:       run.RepoPath,
		OutputDir:      run.OutputDir,
		DefinitionPath: join(def.Path, actionfs.DefinitionFileName),
		Artifacts:      def.Artifacts,
		ArtifactMode:   def.ArtifactMode,
		KickoffName:    actionfs.KickoffFileName,
		Kickoff:        kickoff,
	})
}

// runsWhere says where an execution runs, for its kickoff.
func runsWhere(machine string) string {
	if machine == "" {
		return "locally"
	}
	return "on machine " + machine
}

type actionKickoffData struct {
	Action         string
	ExecutionID    string
	Machine        string
	RepoPath       string
	OutputDir      string
	DefinitionPath string
	ArtifactMode   string
	Artifacts      string
	KickoffName    string
	Kickoff        string
}

// ConcludeAction is the action agent's concludeSession. Like every agent
// conclusion it is proposed as a concludeSession intent under that tool's
// policy (wait-then-allow): the user approves or denies it, and nobody
// answering within the wait window approves it. While it is pending the
// execution and its session stay running, so architect result/wait callers see
// no final result early. Only the intent's Exec finishes the execution and ends
// the session; a denial leaves both running for follow-up and is returned to
// the agent as the outcome.
//
// Completed requires a prepared package only when the frozen launch contract
// promises artifacts. Failed may publish a prepared partial package. The exact
// proposed snapshot is part of approval identity and rechecked on application.
func (s *Service) ConcludeAction(ctx context.Context, sessionID string, req domain.ConcludeActionRequest) (domain.IntentResolution[domain.ActionRun], error) {
	var zero domain.IntentResolution[domain.ActionRun]
	if _, err := s.actionRuntime(); err != nil {
		return zero, err
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		return zero, &domain.ValidationError{Field: "summary", Message: "is required"}
	}
	if len(summary) > domain.MaxActionSummaryLength {
		return zero, &domain.ValidationError{Field: "summary", Message: fmt.Sprintf("is %d characters; keep it under %d — the artifacts carry the detail", len(summary), domain.MaxActionSummaryLength)}
	}
	var status domain.ActionRunStatus
	switch req.Outcome {
	case domain.ActionConclusionCompleted:
		status = domain.ActionRunCompleted
	case domain.ActionConclusionFailed:
		status = domain.ActionRunFailed
	default:
		return zero, &domain.ValidationError{Field: "outcome", Message: "must be one of: completed, failed"}
	}

	session, run, err := s.runningActionSession(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	pkg, err := s.preparedPackage(ctx, run, status)
	if err != nil {
		return zero, err
	}
	packagePath := ""
	if pkg != nil {
		packagePath = pkg.Path
	}
	if err := s.repo.UpdateRunAgentStatus(ctx, session.CurrentRun.ID, domain.AgentStatusWaiting); err != nil {
		return zero, err
	}

	// body/outcome are what every conclusion card renders; the rest names
	// the execution the conclusion finishes and the folder it delivers.
	payload := map[string]any{
		"body":         summary,
		"outcome":      string(req.Outcome),
		"action":       run.Action,
		"execution_id": run.ID,
		"output_dir":   run.OutputDir,
	}
	if packagePath != "" {
		payload["package_dir"] = packagePath
	}
	if run.Machine != "" {
		payload["machine"] = run.Machine
	}
	return awaitIntent(ctx, s, intentSpec[domain.ActionRun]{
		SessionID: sessionID,
		Type:      domain.IntentTypeConcludeSession,
		Summary:   summary,
		Payload:   payload,
		Origin:    intentOrigin(session),
		Exec: func(ctx context.Context, _ domain.IntentInputValues) (domain.ActionRun, error) {
			// Ending the session kills the agent and with it the MCP client
			// whose call this is; the teardown must run to completion anyway.
			return s.applyActionConclusion(ctx, sessionID, req.Outcome, status, summary, packagePath)
		},
	})
}

// checkDeliveryDir validates a preparation folder on the execution
// machine and returns it cleaned. Content problems are ValidationErrors the
// agent can fix; failing to reach the machine is an ordinary error.
func (s *Service) checkDeliveryDir(ctx context.Context, session domain.Session, dir string) (string, error) {
	if dir == "" {
		return "", &domain.ValidationError{Field: "path", Message: "is required: pass the absolute path of the verified artifact folder to deliverArtifacts"}
	}
	if session.Machine != "" {
		if !path.IsAbs(dir) || strings.ContainsAny(dir, "\x00\r\n") {
			return "", &domain.ValidationError{Field: "path", Message: fmt.Sprintf("%q must be an absolute path on %s", dir, config.MachineLabel(session.Machine))}
		}
		dir = path.Clean(dir)
	} else {
		if !filepath.IsAbs(dir) {
			return "", &domain.ValidationError{Field: "path", Message: fmt.Sprintf("%q must be an absolute path", dir)}
		}
		dir = filepath.Clean(dir)
	}
	stats, err := actionpkg.Preflight(ctx, actionpkg.Location{Alias: session.SSH, Path: dir}, s.actionPackageLimits())
	if err != nil {
		return "", packageError("path", err)
	}
	if stats.Files == 0 {
		return "", emptyPackageError(dir)
	}
	return dir, nil
}

// emptyPackageError refuses preparation of a folder with no regular files.
func emptyPackageError(dir string) error {
	return &domain.ValidationError{Field: "path", Message: dir + " is empty; prepare a nonempty verified package, or conclude failed without delivery"}
}

// packageError turns a package content problem into a ValidationError on
// field, so the agent corrects the folder and concludes again; anything else
// keeps its cause.
func packageError(field string, err error) error {
	var content *actionpkg.Error
	if errors.As(err, &content) {
		return &domain.ValidationError{Field: field, Message: content.Message}
	}
	return err
}

// applyActionConclusion publishes the approved snapshot and ends the execution
// atomically. Confirm remote termination first; an unconfirmed stop leaves the
// execution running and its preparation private for retry.
func (s *Service) applyActionConclusion(ctx context.Context, sessionID string, outcome domain.ActionConclusionOutcome, status domain.ActionRunStatus, summary, packagePath string) (domain.ActionRun, error) {
	rt := s.actions
	session, run, err := s.runningActionSession(ctx, sessionID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	if err := rt.artifacts.beginMutation(run.ID); err != nil {
		return domain.ActionRun{}, err
	}
	defer rt.artifacts.endMutation(run.ID)
	session, run, err = s.runningActionSession(ctx, sessionID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	now := time.Now().UTC()
	pkg, err := s.preparedPackage(ctx, run, status)
	if err != nil {
		return domain.ActionRun{}, err
	}
	currentPath := ""
	if pkg != nil {
		currentPath = pkg.Path
	}
	if currentPath != packagePath {
		return domain.ActionRun{}, &domain.ConflictError{Resource: "action_package", Message: "prepared package changed while approval was pending; review the new package and conclude again"}
	}
	if pkg != nil {
		_, retention := s.actionPackageSettings()
		expires := now.Add(retention)
		pkg.State, pkg.ExpiresAt = domain.ActionArtifactsReady, &expires
	}
	// The caller's notice is begun before the agent is stopped and the
	// conclusion published, and settled right after the publish — the point
	// from which getActionResult serves the final status and the package
	// record — so a crash in between leaves a pending_commit row reconciled
	// against the execution instead of a lost or premature announcement. A
	// denied intent never reaches this code.
	proposed := run
	proposed.Status, proposed.Summary = status, summary
	notice, err := s.beginActionEndedMessage(ctx, proposed, pkg)
	if err != nil {
		return domain.ActionRun{}, fmt.Errorf("record action ended notice: %w", err)
	}
	if session.Machine != "" {
		if err := s.stopRemote(ctx, session); err != nil {
			return domain.ActionRun{}, fmt.Errorf("conclusion not applied: %w", err)
		}
	}
	err = rt.runs.PublishActionConclusion(ctx, run.ID, status, summary, pkg, now)
	if err != nil {
		return domain.ActionRun{}, err
	}
	s.settleActionEndedMessage(ctx, notice, run.ID)
	raw := map[string]any{
		"execution_id": run.ID,
		"outcome":      string(outcome),
		"summary":      summary,
		"output_dir":   run.OutputDir,
	}
	if pkg != nil {
		raw["package_dir"] = pkg.Path
	}
	if err := s.endActionSession(ctx, session, "session concluded", "concluded", raw); err != nil {
		return domain.ActionRun{}, err
	}
	finished, err := s.GetActionRun(ctx, run.ID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	s.publishActionEvent(run, status)
	s.logger.Info("action execution concluded", "action", run.Action, "execution_id", run.ID, "status", status, "package", packagePath, "machine", run.Machine)
	return finished, nil
}

// RecentActionConclusions returns the latest concluded summaries of the action
// the calling session executes.
func (s *Service) RecentActionConclusions(ctx context.Context, sessionID string) ([]domain.ActionConclusion, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return nil, err
	}
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.SessionType != domain.SessionTypeAction {
		return nil, &domain.ValidationError{Field: "session_id", Message: "is not an action session"}
	}
	run, err := rt.runs.GetActionRun(ctx, session.ContextID)
	if err != nil {
		return nil, err
	}
	return rt.runs.RecentActionConclusions(ctx, run.Action, actionConclusionHistory)
}

// CancelActionRun stops a running execution at the user's request.
func (s *Service) CancelActionRun(ctx context.Context, id string) (domain.ActionRun, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionRun{}, err
	}
	run, err := rt.runs.GetActionRun(ctx, id)
	if err != nil {
		return domain.ActionRun{}, err
	}
	if err := rt.artifacts.beginMutation(run.ID); err != nil {
		return domain.ActionRun{}, err
	}
	defer rt.artifacts.endMutation(run.ID)
	run, err = rt.runs.GetActionRun(ctx, id)
	if err != nil {
		return domain.ActionRun{}, err
	}
	if run.Status != domain.ActionRunRunning {
		return domain.ActionRun{}, &domain.ConflictError{Resource: "action_run", Field: "status", Message: fmt.Sprintf("action execution %s is %s, not running", id, run.Status)}
	}
	var session *domain.Session
	if run.SessionID != "" {
		live, err := s.repo.GetSession(ctx, run.SessionID)
		switch {
		case err == nil:
			session = &live
		case errors.As(err, new(*domain.NotFoundError)):
		default:
			return domain.ActionRun{}, err
		}
	}
	// A remote agent must be confirmed stopped before the execution reads as
	// ended; otherwise it stays running and the user can stop it again.
	teardownCtx := context.WithoutCancel(ctx)
	if session != nil && session.Machine != "" {
		stopCtx, cancel := context.WithTimeout(teardownCtx, sessionTeardownTimeout)
		err := s.stopRemote(stopCtx, *session)
		cancel()
		if err != nil {
			return domain.ActionRun{}, fmt.Errorf("execution not stopped: %w", err)
		}
	}
	const cancelled = "cancelled by user"
	notice, err := s.beginActionEndedMessage(ctx, failedRun(run, cancelled), nil)
	if err != nil {
		return domain.ActionRun{}, fmt.Errorf("record action ended notice: %w", err)
	}
	if err := rt.runs.FinishActionRun(ctx, id, domain.ActionRunFailed, "", cancelled, time.Now().UTC()); err != nil {
		return domain.ActionRun{}, err
	}
	s.settleActionEndedMessage(ctx, notice, id)
	if session != nil {
		if err := s.endActionSession(teardownCtx, *session, "session cancelled", "cancelled", map[string]any{"execution_id": id}); err != nil {
			return domain.ActionRun{}, err
		}
	}
	s.publishActionEvent(run, domain.ActionRunFailed)
	s.logger.Info("action execution cancelled", "action", run.Action, "execution_id", id)
	return s.GetActionRun(ctx, id)
}

// failedRun is run as a daemon-side failure is about to record it.
func failedRun(run domain.ActionRun, reason string) domain.ActionRun {
	run.Status, run.Summary, run.Error = domain.ActionRunFailed, "", reason
	return run
}

// ReconcileActionRuns runs at startup, after RestoreRunningSessions: an
// execution still recorded running whose agent session did not come back —
// missing, not running, or never created — is failed as interrupted. Running
// executions whose session was restored stay running.
func (s *Service) ReconcileActionRuns(ctx context.Context) error {
	if s.actions == nil {
		return nil
	}
	// A pending request's approval lived in memory with its intent; it cannot
	// be approved after a restart, so it must not read as pending.
	failed, err := s.actions.runs.FailPendingActionRuns(ctx, actionRequestFailedOnRestart, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("fail pending action requests: %w", err)
	}
	for _, run := range failed {
		s.logger.Warn("action request failed: daemon restarted before it was approved",
			"action", run.Action, "execution_id", run.ID, "architect", run.ArchitectKey)
	}
	running, err := s.actions.runs.RunningActionRuns(ctx)
	if err != nil {
		return fmt.Errorf("list running action executions: %w", err)
	}
	for _, run := range running {
		reason := ""
		if run.SessionID == "" {
			reason = "interrupted before its agent session was created"
		} else {
			session, err := s.repo.GetSession(ctx, run.SessionID)
			switch {
			case errors.As(err, new(*domain.NotFoundError)):
				reason = "interrupted: its agent session no longer exists"
			case err != nil:
				return fmt.Errorf("read session %s of action execution %s: %w", run.SessionID, run.ID, err)
			case session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning:
				reason = "interrupted: its agent session could not be resumed"
			}
		}
		if reason == "" {
			continue
		}
		s.logger.Error("action execution left running without a live session; marking failed",
			"action", run.Action, "execution_id", run.ID, "session_id", run.SessionID, "reason", reason)
		s.finishActionRunFailed(run.ID, reason)
		if run.SessionID != "" {
			s.removeActionSession(run.SessionID)
		}
	}
	return nil
}

// failActionSession fails the execution of an action session whose agent
// could not be (re)launched and removes the session. It is the action
// counterpart of moving a ticket back to backlog.
func (s *Service) failActionSession(session domain.Session, reason string) {
	s.finishActionRunFailed(session.ContextID, reason)
	s.removeActionSession(session.ID)
}

// finishActionRunFailed ends a running execution as a daemon-side failure
// (launch, restore or resume failure, restart interruption) and tells its
// requester. An execution no longer running was ended by another path, which
// told the requester; it is left alone.
func (s *Service) finishActionRunFailed(id, reason string) {
	if s.actions == nil {
		return
	}
	ctx := context.Background()
	run, err := s.actions.runs.GetActionRun(ctx, id)
	if err != nil {
		s.logger.Error("failed to read action execution to fail", "execution_id", id, "reason", reason, "error", err)
		return
	}
	if run.Status != domain.ActionRunRunning {
		return
	}
	notice, err := s.beginActionEndedMessage(ctx, failedRun(run, reason), nil)
	if err != nil {
		// The failure must still be recorded; reconciliation cannot create
		// the notice from nothing, so say so loudly.
		s.logger.Error("action ended notice not recorded; the requester will not be told", "execution_id", id, "reason", reason, "error", err)
	}
	if err := s.actions.runs.FinishActionRun(ctx, id, domain.ActionRunFailed, "", reason, time.Now().UTC()); err != nil {
		var conflict *domain.ConflictError
		if errors.As(err, &conflict) {
			return
		}
		s.logger.Error("failed to mark action execution failed", "execution_id", id, "reason", reason, "error", err)
		return
	}
	s.logger.Error("action execution failed", "execution_id", id, "reason", reason)
	s.settleActionEndedMessage(ctx, notice, id)
	run, err = s.actions.runs.GetActionRun(ctx, id)
	if err != nil {
		s.logger.Error("failed to read failed action execution", "execution_id", id, "error", err)
		return
	}
	s.publishActionEvent(run, domain.ActionRunFailed)
}

// removeActionSession kills an action session's terminals and deletes it. The
// execution record keeps the history, so nothing is lost with the session. A
// remote session is deleted only once its remote resources are confirmed
// stopped; otherwise it is kept, with its ownership records, and the failure
// logged.
func (s *Service) removeActionSession(sessionID string) {
	ctx := context.Background()
	if session, err := s.repo.GetSession(ctx, sessionID); err == nil && session.Machine != "" {
		stopCtx, cancel := context.WithTimeout(ctx, remoteCleanupTimeout)
		err := s.stopRemote(stopCtx, session)
		cancel()
		if err != nil {
			s.logger.Error("remote action session kept: remote stop not confirmed", "session_id", sessionID, "error", err)
			return
		}
	}
	if err := s.terminal.KillBySession(ctx, sessionID); err != nil && !errors.Is(err, errTerminalNotFound) {
		s.logger.Error("failed to kill action session terminals", "session_id", sessionID, "error", err)
	}
	if err := s.repo.DeleteSession(ctx, sessionID); err != nil && !errors.As(err, new(*domain.NotFoundError)) {
		s.logger.Error("failed to delete action session", "session_id", sessionID, "error", err)
	}
	s.cleanupDeletedSession(sessionID)
}

// endActionSession ends a live action session like a conclusion: the run is
// completed, the ended event published, the terminals killed and the session
// deleted. A remote session's agent must already be confirmed stopped.
func (s *Service) endActionSession(ctx context.Context, session domain.Session, message, lifecycle string, raw map[string]any) error {
	runID := ""
	if session.CurrentRun != nil {
		runID = session.CurrentRun.ID
		if session.CurrentRun.Status == domain.SessionRunStatusRunning {
			if err := s.repo.MarkRunCompleted(ctx, runID); err != nil {
				return err
			}
		}
	}
	if err := s.appendAndPublishSessionEnded(ctx, session.ID, runID, message, lifecycle, raw); err != nil {
		return err
	}
	if err := s.terminal.KillBySession(ctx, session.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return fmt.Errorf("kill action session terminal: %w", err)
	}
	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return err
	}
	s.cleanupDeletedSession(session.ID)
	return nil
}

func (s *Service) runningActionSession(ctx context.Context, sessionID string) (domain.Session, domain.ActionRun, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, domain.ActionRun{}, err
	}
	if session.SessionType != domain.SessionTypeAction {
		return domain.Session{}, domain.ActionRun{}, &domain.ValidationError{Field: "session_id", Message: "is not an action session"}
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.Session{}, domain.ActionRun{}, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}
	run, err := s.actions.runs.GetActionRun(ctx, session.ContextID)
	if err != nil {
		return domain.Session{}, domain.ActionRun{}, err
	}
	if run.Status != domain.ActionRunRunning {
		return domain.Session{}, domain.ActionRun{}, &domain.ConflictError{Resource: "action_run", Field: "status", Message: fmt.Sprintf("action execution %s is %s, not running", run.ID, run.Status)}
	}
	return session, run, nil
}

// actionTerminalWorkdirs offers the action repository (default) and the
// execution's output directory for new terminals.
func actionTerminalWorkdirs(session domain.Session) ([]domain.TerminalWorkdir, error) {
	run := session.CurrentRun
	if run == nil {
		return nil, &domain.ValidationError{Field: "session_id", Message: "session has no run"}
	}
	home, _ := os.UserHomeDir()
	display := func(p string) string {
		if session.Machine != "" {
			return session.Machine + ":" + p
		}
		if home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
			return "~" + strings.TrimPrefix(p, home)
		}
		return p
	}
	out := []domain.TerminalWorkdir{{ID: "session-primary", Title: "Action repository", Path: run.Workdir, DisplayPath: display(run.Workdir), Default: true, Machine: session.Machine}}
	for i, path := range run.AdditionalWorkdirs {
		title := "Output directory"
		if i < len(run.AdditionalRepos) && run.AdditionalRepos[i] != actionOutputAdditionalRepoKey {
			title = run.AdditionalRepos[i]
		}
		out = append(out, domain.TerminalWorkdir{ID: fmt.Sprintf("session-additional:%d", i), Title: title, Path: path, DisplayPath: display(path), Machine: session.Machine})
	}
	return out, nil
}
