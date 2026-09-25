package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/actionfs"
	"github.com/hiveryn/daemon/internal/domain"
)

// Actions run as sessions of type action: one agent session per execution, in
// the action's own repository, with a daemon-created output directory outside
// it. The execution itself is an ActionRun record, addressed by its stable id,
// that outlives the session:
//
//	launch        record inserted running (the single-run rule), output
//	              directory created, session created and its run launched;
//	              an architect's approved request (actions_architect.go)
//	              moves its pending_approval record to running and launches
//	              the same way
//	conclude      the agent's concludeSession is a concludeSession intent;
//	              approval (or the wait window expiring) finishes the record
//	              (completed or failed) and ends the session like any other
//	              conclusion, a denial leaves both running
//	cancel        the user stops a running execution: failed, session ended
//	launch error  failed with the launch error; the session is removed
//	PTY exit      the agent is resumed like every session; a failed resume
//	              fails the execution
//	restart       running action sessions are restored like every session; a
//	              failed restore fails the execution, and ReconcileActionRuns
//	              fails any execution left running without a live session
//
// Unlike deferred intents, execution history is never pruned and a restart
// does not fail executions whose agent session resumes.

const (
	// actionConclusionHistory is how many earlier summaries an action agent
	// can read back.
	actionConclusionHistory = 5
	// actionOutputAdditionalRepoKey labels the output directory in the
	// session's additional workdir scope.
	actionOutputAdditionalRepoKey = "output"
)

type actionRuntime struct {
	runs       domain.ActionRunRepository
	root       string
	outputRoot string
	// launchMu serializes launches, so the busy check, the output directory
	// and the session are created as one step per action.
	launchMu sync.Mutex
}

// SetActions enables the Actions runtime: executions are persisted in runs,
// definitions are read from root and output directories are created under
// outputRoot/<action>/<execution id>.
func (s *Service) SetActions(runs domain.ActionRunRepository, root, outputRoot string) {
	s.actions = &actionRuntime{runs: runs, root: root, outputRoot: outputRoot}
}

func (s *Service) actionRuntime() (*actionRuntime, error) {
	if s.actions == nil {
		return nil, errors.New("actions runtime is not configured")
	}
	return s.actions, nil
}

func (s *Service) ListActions(ctx context.Context) (domain.ActionList, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionList{}, err
	}
	defs, err := actionfs.List(rt.root)
	if err != nil {
		return domain.ActionList{}, err
	}
	running, err := rt.runs.RunningActionRuns(ctx)
	if err != nil {
		return domain.ActionList{}, err
	}
	out := domain.ActionList{Root: rt.root, Actions: make([]domain.ActionDefinition, 0, len(defs))}
	for _, def := range defs {
		item := def.ActionDefinition
		if run, ok := running[item.Name]; ok {
			item.RunningExecutionID = run.ID
		}
		out.Actions = append(out.Actions, item)
	}
	return out, nil
}

func (s *Service) GetAction(ctx context.Context, name string) (domain.ActionDefinition, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionDefinition{}, err
	}
	def, err := actionfs.Get(rt.root, name)
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
	for i := range runs {
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
	if err := s.checkActionVariant(req.ProfileName); err != nil {
		return domain.LaunchActionResult{}, err
	}

	rt.launchMu.Lock()
	defer rt.launchMu.Unlock()

	def, err := actionfs.Get(rt.root, name)
	if err != nil {
		return domain.LaunchActionResult{}, err
	}
	if !def.Valid {
		return domain.LaunchActionResult{}, actionfs.InvalidError(def)
	}

	now := time.Now().UTC()
	run := domain.ActionRun{
		ID:          uuid.NewString(),
		Action:      def.Name,
		Trigger:     domain.ActionRunTriggerManual,
		Status:      domain.ActionRunRunning,
		Prompt:      prompt,
		ProfileName: req.ProfileName,
		RepoPath:    filepath.Clean(def.Path),
		CreatedAt:   now,
		StartedAt:   &now,
	}
	run.OutputDir = rt.outputDir(def.Name, run.ID)
	if err := rt.runs.CreateActionRun(ctx, run); err != nil {
		return domain.LaunchActionResult{}, err
	}
	return s.launchActionSession(ctx, def, run, req.Cols, req.Rows)
}

// checkActionVariant reports whether profileName names a launchable variant in
// the current config.
func (s *Service) checkActionVariant(profileName string) error {
	cfg, err := s.currentConfig()
	if err != nil {
		return err
	}
	profile, ok := cfg.Variants[profileName]
	if !ok {
		return &domain.NotFoundError{Resource: "agent_profile", ID: profileName}
	}
	if _, err := parseAgentKind(profile.Agent); err != nil {
		return err
	}
	return nil
}

func (rt *actionRuntime) outputDir(action, executionID string) string {
	return filepath.Join(rt.outputRoot, action, executionID)
}

// launchActionSession starts the agent session of an execution already
// recorded running: the output directory, the session and its run. It is
// shared by manual launches and approved architect requests, and must be
// called with launchMu held.
func (s *Service) launchActionSession(ctx context.Context, def actionfs.Definition, run domain.ActionRun, cols, rows uint16) (domain.LaunchActionResult, error) {
	rt := s.actions

	// From here on every failure finishes the record as failed, so a broken
	// launch never leaves the action falsely busy.
	fail := func(stage string, cause error) (domain.LaunchActionResult, error) {
		s.finishActionRunFailed(run.ID, fmt.Sprintf("launch failed (%s): %v", stage, cause))
		return domain.LaunchActionResult{}, cause
	}

	if err := createEmptyOutputDir(run.OutputDir); err != nil {
		return fail("create output directory", err)
	}
	instructions, err := builtinPrompt(actionSystemPromptName)
	if err != nil {
		return fail("instructions", err)
	}
	kickoff, err := renderActionKickoff(def, run)
	if err != nil {
		return fail("kickoff", err)
	}
	session, err := s.repo.CreateSession(ctx, domain.CreateSessionParams{
		SessionType:        domain.SessionTypeAction,
		ContextID:          run.ID,
		Prompt:             kickoff,
		Workdir:            run.RepoPath,
		AdditionalRepos:    []string{actionOutputAdditionalRepoKey},
		AdditionalWorkdirs: []string{run.OutputDir},
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
		"trigger", run.Trigger,
		"architect", run.ArchitectKey,
	)
	return domain.LaunchActionResult{Run: run, Session: session, MainTerminalID: result.MainTerminalID}, nil
}

// createEmptyOutputDir creates the execution's output directory. The leaf must
// not exist yet: the agent is promised an empty folder of its own.
func createEmptyOutputDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create output parent %s: %w", filepath.Dir(dir), err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory %s: %w", dir, err)
	}
	return nil
}

func renderActionKickoff(def actionfs.Definition, run domain.ActionRun) (string, error) {
	kickoff, err := actionfs.RenderKickoff(def, run.Prompt, run.OutputDir)
	if err != nil {
		return "", err
	}
	return renderBuiltinPrompt(actionKickoffPromptName, actionKickoffData{
		Action:         def.Name,
		ExecutionID:    run.ID,
		RepoPath:       run.RepoPath,
		OutputDir:      run.OutputDir,
		DefinitionPath: filepath.Join(def.Path, actionfs.DefinitionFileName),
		Artifacts:      def.Artifacts,
		KickoffName:    actionfs.KickoffFileName,
		Kickoff:        kickoff,
	})
}

type actionKickoffData struct {
	Action         string
	ExecutionID    string
	RepoPath       string
	OutputDir      string
	DefinitionPath string
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
// the agent as the outcome. completed requires a non-empty output directory,
// checked before the request is shown and again when it is applied.
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
	// Validate before the popup, as RequestConclusion does: a doomed
	// conclusion goes straight back to the agent and is never shown.
	if status == domain.ActionRunCompleted {
		if err := checkActionOutputDelivered(run.OutputDir); err != nil {
			return zero, err
		}
	}
	if err := s.repo.UpdateRunAgentStatus(ctx, session.CurrentRun.ID, domain.AgentStatusWaiting); err != nil {
		return zero, err
	}

	return awaitIntent(ctx, s, intentSpec[domain.ActionRun]{
		SessionID: sessionID,
		Type:      domain.IntentTypeConcludeSession,
		Summary:   summary,
		// body/outcome are what every conclusion card renders; the rest names
		// the execution the conclusion finishes.
		Payload: map[string]any{
			"body":         summary,
			"outcome":      string(req.Outcome),
			"action":       run.Action,
			"execution_id": run.ID,
			"output_dir":   run.OutputDir,
		},
		Origin: intentOrigin(session),
		Exec: func(ctx context.Context, _ domain.IntentInputValues) (domain.ActionRun, error) {
			// Ending the session kills the agent and with it the MCP client
			// whose call this is; the teardown must run to completion anyway.
			return s.applyActionConclusion(context.WithoutCancel(ctx), sessionID, req.Outcome, status, summary)
		},
	})
}

// applyActionConclusion finishes an execution with its approved conclusion
// and ends its session. The execution and its output are rechecked: the
// agent kept running while the conclusion waited for approval.
func (s *Service) applyActionConclusion(ctx context.Context, sessionID string, outcome domain.ActionConclusionOutcome, status domain.ActionRunStatus, summary string) (domain.ActionRun, error) {
	rt := s.actions
	session, run, err := s.runningActionSession(ctx, sessionID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	if status == domain.ActionRunCompleted {
		if err := checkActionOutputDelivered(run.OutputDir); err != nil {
			return domain.ActionRun{}, err
		}
	}
	if err := rt.runs.FinishActionRun(ctx, run.ID, status, summary, "", time.Now().UTC()); err != nil {
		return domain.ActionRun{}, err
	}
	finished, err := rt.runs.GetActionRun(ctx, run.ID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	if err := s.endActionSession(ctx, session, "session concluded", "concluded", map[string]any{
		"execution_id": run.ID,
		"outcome":      string(outcome),
		"summary":      summary,
		"output_dir":   run.OutputDir,
	}); err != nil {
		return domain.ActionRun{}, err
	}
	s.publishActionEvent(run, status)
	s.logger.Info("action execution concluded", "action", run.Action, "execution_id", run.ID, "status", status)
	return finished, nil
}

// checkActionOutputDelivered refuses a completed conclusion while the output
// directory is empty.
func checkActionOutputDelivered(outputDir string) error {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return &domain.ValidationError{Field: "outcome", Message: fmt.Sprintf("cannot read output directory %s: %v", outputDir, err)}
	}
	if len(entries) == 0 {
		return &domain.ValidationError{Field: "outcome", Message: "output directory " + outputDir + " is empty; deliver the artifact package before concluding completed, or conclude failed"}
	}
	return nil
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
	if run.Status != domain.ActionRunRunning {
		return domain.ActionRun{}, &domain.ConflictError{Resource: "action_run", Field: "status", Message: fmt.Sprintf("action execution %s is %s, not running", id, run.Status)}
	}
	if err := rt.runs.FinishActionRun(ctx, id, domain.ActionRunFailed, "", "cancelled by user", time.Now().UTC()); err != nil {
		return domain.ActionRun{}, err
	}
	if run.SessionID != "" {
		session, err := s.repo.GetSession(ctx, run.SessionID)
		switch {
		case err == nil:
			if err := s.endActionSession(context.WithoutCancel(ctx), session, "session cancelled", "cancelled", map[string]any{"execution_id": id}); err != nil {
				return domain.ActionRun{}, err
			}
		case errors.As(err, new(*domain.NotFoundError)):
		default:
			return domain.ActionRun{}, err
		}
	}
	s.publishActionEvent(run, domain.ActionRunFailed)
	s.logger.Info("action execution cancelled", "action", run.Action, "execution_id", id)
	return rt.runs.GetActionRun(ctx, id)
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

func (s *Service) finishActionRunFailed(id, reason string) {
	if s.actions == nil {
		return
	}
	ctx := context.Background()
	if err := s.actions.runs.FinishActionRun(ctx, id, domain.ActionRunFailed, "", reason, time.Now().UTC()); err != nil {
		var conflict *domain.ConflictError
		if errors.As(err, &conflict) {
			return
		}
		s.logger.Error("failed to mark action execution failed", "execution_id", id, "reason", reason, "error", err)
		return
	}
	s.logger.Error("action execution failed", "execution_id", id, "reason", reason)
	run, err := s.actions.runs.GetActionRun(ctx, id)
	if err != nil {
		s.logger.Error("failed to read failed action execution", "execution_id", id, "error", err)
		return
	}
	s.publishActionEvent(run, domain.ActionRunFailed)
}

// removeActionSession kills an action session's terminals and deletes it. The
// execution record keeps the history, so nothing is lost with the session.
func (s *Service) removeActionSession(sessionID string) {
	ctx := context.Background()
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
// deleted.
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
		if home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
			return "~" + strings.TrimPrefix(p, home)
		}
		return p
	}
	out := []domain.TerminalWorkdir{{ID: "session-primary", Title: "Action repository", Path: run.Workdir, DisplayPath: display(run.Workdir), Default: true}}
	for i, path := range run.AdditionalWorkdirs {
		title := "Output directory"
		if i < len(run.AdditionalRepos) && run.AdditionalRepos[i] != actionOutputAdditionalRepoKey {
			title = run.AdditionalRepos[i]
		}
		out = append(out, domain.TerminalWorkdir{ID: fmt.Sprintf("session-additional:%d", i), Title: title, Path: path, DisplayPath: display(path)})
	}
	return out, nil
}
