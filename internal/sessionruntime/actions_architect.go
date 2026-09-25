package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/actionfs"
	"github.com/hiveryn/daemon/internal/domain"
)

// Architect-requested Actions. An architect discovers the Actions its
// hiveryn.yaml lists under availableActions and requests one with
// executeAction(name, prompt). The request is a deferred intent (policy
// manual) with one required input, the agent variant; its intent id IS the
// execution id, so the architect holds one id from request to result:
//
//	executeAction   execution recorded pending_approval (trigger architect),
//	                intent shown; the call returns at once
//	deny            execution denied, with the user's reason
//	approve         variant and availability rechecked, pending_approval →
//	                running under the single-run rule, agent session launched;
//	                from here the Action lifecycle (actions.go) owns the record
//	start failure   execution failed with why it could not start — including
//	                another execution of the action winning meanwhile
//	session end     the requesting architect session ended before approval:
//	                failed, it never ran
//	restart         pending requests are failed (ReconcileActionRuns); a
//	                running execution follows the Action restart rules
//
// The generic deferred record completes when the launch returns and is pruned
// after its retention; neither says anything about the execution, whose
// action_runs record is the only source for results. Results are scoped to
// the architect, not the requesting session, so a later session of the same
// architect can still read them and another architect cannot.

const (
	actionVariantInput           = "variant"
	actionRequestFailedOnRestart = "daemon restarted before this request was approved; it never ran"
)

// architectSession returns the calling session, which must be an architect
// session. The architect key always comes from the stored session.
func (s *Service) architectSession(ctx context.Context, sessionID string) (domain.Session, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, err
	}
	if session.SessionType != domain.SessionTypeArchitect {
		return domain.Session{}, &domain.ValidationError{Field: "session_id", Message: "Actions are available to architect sessions only"}
	}
	return session, nil
}

// AvailableActions lists the architect's configured Actions in config order,
// each enriched from the library. A configured name without a definition is
// listed invalid with a problem, never dropped.
func (s *Service) AvailableActions(ctx context.Context, sessionID string) (domain.AvailableActionList, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.AvailableActionList{}, err
	}
	session, err := s.architectSession(ctx, sessionID)
	if err != nil {
		return domain.AvailableActionList{}, err
	}
	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return domain.AvailableActionList{}, err
	}
	running, err := rt.runs.RunningActionRuns(ctx)
	if err != nil {
		return domain.AvailableActionList{}, err
	}
	out := domain.AvailableActionList{Actions: make([]domain.ActionDefinition, 0, len(architect.AvailableActions))}
	for _, name := range architect.AvailableActions {
		def, err := rt.availableDefinition(name)
		if err != nil {
			return domain.AvailableActionList{}, err
		}
		item := def.ActionDefinition
		// Suggestions prefill the user's manual launch form; they are not
		// guidance for an architect composing a request.
		item.Suggestions = nil
		if run, ok := running[item.Name]; ok {
			item.RunningExecutionID = run.ID
		}
		out.Actions = append(out.Actions, item)
	}
	return out, nil
}

// availableDefinition inspects a configured action; a missing directory is an
// invalid definition that says so.
func (rt *actionRuntime) availableDefinition(name string) (actionfs.Definition, error) {
	def, err := actionfs.Get(rt.root, name)
	if err == nil {
		return def, nil
	}
	if !errors.As(err, new(*domain.NotFoundError)) {
		return actionfs.Definition{}, err
	}
	path := filepath.Join(rt.root, name)
	return actionfs.Definition{ActionDefinition: domain.ActionDefinition{
		Name:     name,
		Path:     path,
		Valid:    false,
		Problems: []domain.ActionProblem{{Path: path, Message: "listed in availableActions but not found in the Actions library (" + rt.root + ")"}},
	}}, nil
}

// architectAllows reports whether the architect's current config lists name
// under availableActions.
func (s *Service) architectAllows(architectKey, name string) (bool, error) {
	architect, err := s.currentArchitect(architectKey)
	if err != nil {
		return false, err
	}
	return slices.Contains(architect.AvailableActions, name), nil
}

func notAvailableError(architectKey, name string) error {
	return &domain.ValidationError{Field: "name", Message: fmt.Sprintf("action %q is not available to architect %s: it must be listed under availableActions in hiveryn.yaml (see getAvailableActions)", name, architectKey)}
}

// RequestExecuteAction raises the approval request for one execution and
// returns at once with its pending_approval result. Nothing runs until the
// user approves and picks a variant.
func (s *Service) RequestExecuteAction(ctx context.Context, sessionID string, req domain.ExecuteActionRequest) (domain.ActionResult, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionResult{}, err
	}
	session, err := s.architectSession(ctx, sessionID)
	if err != nil {
		return domain.ActionResult{}, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.ActionResult{}, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.ActionResult{}, &domain.ValidationError{Field: "name", Message: "is required"}
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return domain.ActionResult{}, &domain.ValidationError{Field: "prompt", Message: "is required"}
	}
	allowed, err := s.architectAllows(session.ArchitectKey, name)
	if err != nil {
		return domain.ActionResult{}, err
	}
	if !allowed {
		return domain.ActionResult{}, notAvailableError(session.ArchitectKey, name)
	}
	def, err := rt.availableDefinition(name)
	if err != nil {
		return domain.ActionResult{}, err
	}
	if !def.Valid {
		return domain.ActionResult{}, actionfs.InvalidError(def)
	}
	running, err := rt.runs.RunningActionRuns(ctx)
	if err != nil {
		return domain.ActionResult{}, err
	}
	if run, ok := running[name]; ok {
		return domain.ActionResult{}, &domain.ConflictError{Resource: "action", Field: "name", Message: fmt.Sprintf("%s is already running (execution %s); only one execution of an action may run at a time — request it again once that execution ends", name, run.ID)}
	}
	variants, err := s.actionVariantInput()
	if err != nil {
		return domain.ActionResult{}, err
	}

	// The execution id is the intent id, which submitDeferredIntent mints; the
	// creator's Created hook records it for the Exec below. A retry attaches
	// to the original intent and never runs this spec's hooks or Exec.
	var executionID string
	origin := intentOrigin(session)
	record, err := submitDeferredIntent(ctx, s, intentSpec[domain.ActionRun]{
		SessionID: sessionID,
		Type:      domain.IntentTypeExecuteAction,
		Summary:   "Run " + name,
		Payload: map[string]any{
			"action": name,
			"prompt": prompt,
		},
		Inputs: []domain.IntentInputField{variants},
		Origin: origin,
		Hooks: deferredHooks{
			Created: func(ctx context.Context, in domain.Intent) error {
				executionID = in.ID
				run := domain.ActionRun{
					ID:                 in.ID,
					Action:             name,
					Trigger:            domain.ActionRunTriggerArchitect,
					Status:             domain.ActionRunPendingApproval,
					Prompt:             prompt,
					RepoPath:           filepath.Clean(def.Path),
					OutputDir:          rt.outputDir(name, in.ID),
					CreatedAt:          in.CreatedAt,
					ArchitectKey:       session.ArchitectKey,
					RequesterSessionID: session.ID,
				}
				if err := rt.runs.CreateActionRun(ctx, run); err != nil {
					return fmt.Errorf("record action request: %w", err)
				}
				s.publishActionEvent(run, domain.ActionRunPendingApproval)
				return nil
			},
			Denied: func(ctx context.Context, in domain.Intent, reason string) {
				s.endActionRequest(in.ID, domain.ActionRunDenied, reason, "")
			},
			Abandoned: func(ctx context.Context, in domain.Intent, reason string) {
				s.endActionRequest(in.ID, domain.ActionRunFailed, "", reason)
			},
		},
		Exec: func(ctx context.Context, inputs domain.IntentInputValues) (domain.ActionRun, error) {
			variant, _ := inputs[actionVariantInput].(string)
			return s.startRequestedAction(ctx, executionID, session.ArchitectKey, variant)
		},
	})
	if err != nil {
		return domain.ActionResult{}, err
	}
	s.logger.Info("action requested", "action", name, "execution_id", record.ID, "architect", session.ArchitectKey, "session_id", sessionID, "status", record.Status)
	return s.GetActionResult(ctx, sessionID, record.ID)
}

// actionVariantInput is the required variant choice, offering every
// configured variant. There is no default: the user picks one each time.
func (s *Service) actionVariantInput() (domain.IntentInputField, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return domain.IntentInputField{}, err
	}
	names := make([]string, 0, len(cfg.Variants))
	for name := range cfg.Variants {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return domain.IntentInputField{}, &domain.ValidationError{Field: "variant", Message: "no agent variants are configured (variants.yaml); an Action needs one to run"}
	}
	if len(names) > domain.MaxIntentInputOptions {
		return domain.IntentInputField{}, &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("%d agent variants are configured; an approval choice offers at most %d", len(names), domain.MaxIntentInputOptions)}
	}
	options := make([]domain.IntentInputOption, 0, len(names))
	for _, name := range names {
		options = append(options, domain.IntentInputOption{Value: name, Description: cfg.Variants[name].Agent})
	}
	return domain.IntentInputField{
		Name:        actionVariantInput,
		Label:       "Agent variant",
		Description: "The agent that runs this Action.",
		Type:        domain.IntentInputChoice,
		Required:    true,
		Options:     options,
	}, nil
}

// startRequestedAction is the approved request's operation. Everything that
// could have changed while the request was pending is checked again — the
// architect's availableActions, the variant, the definition and above all the
// single-run rule — and any failure fails the request with why it could not
// start. Once it is running the Action lifecycle owns the record: this
// returning means launched, never completed.
func (s *Service) startRequestedAction(ctx context.Context, executionID, architectKey, variant string) (domain.ActionRun, error) {
	rt := s.actions
	if executionID == "" {
		return domain.ActionRun{}, errors.New("action request has no execution id")
	}
	fail := func(cause error) (domain.ActionRun, error) {
		s.endActionRequest(executionID, domain.ActionRunFailed, "", "could not start: "+cause.Error())
		return domain.ActionRun{}, cause
	}

	run, err := rt.runs.GetActionRun(ctx, executionID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	allowed, err := s.architectAllows(architectKey, run.Action)
	if err != nil {
		return fail(err)
	}
	if !allowed {
		return fail(notAvailableError(architectKey, run.Action))
	}
	if strings.TrimSpace(variant) == "" {
		return fail(&domain.ValidationError{Field: actionVariantInput, Message: "is required"})
	}
	if err := s.checkActionVariant(variant); err != nil {
		return fail(err)
	}

	rt.launchMu.Lock()
	defer rt.launchMu.Unlock()

	def, err := rt.availableDefinition(run.Action)
	if err != nil {
		return fail(err)
	}
	if !def.Valid {
		return fail(actionfs.InvalidError(def))
	}
	now := time.Now().UTC()
	if err := rt.runs.StartActionRun(ctx, executionID, variant, now); err != nil {
		return fail(err)
	}
	run.Status = domain.ActionRunRunning
	run.ProfileName = variant
	run.StartedAt = &now
	run.RepoPath = filepath.Clean(def.Path)
	if _, err := s.launchActionSession(ctx, def, run, 0, 0); err != nil {
		// launchActionSession already failed the running execution.
		return domain.ActionRun{}, err
	}
	return rt.runs.GetActionRun(ctx, executionID)
}

// endActionRequest ends a pending request as denied or failed. A request that
// is no longer pending was already resolved elsewhere and is left alone.
func (s *Service) endActionRequest(executionID string, status domain.ActionRunStatus, reason, errText string) {
	if s.actions == nil || executionID == "" {
		return
	}
	ctx := context.Background()
	if err := s.actions.runs.EndPendingActionRun(ctx, executionID, status, reason, errText, time.Now().UTC()); err != nil {
		if errors.As(err, new(*domain.ConflictError)) || errors.As(err, new(*domain.NotFoundError)) {
			return
		}
		s.logger.Error("failed to end action request", "execution_id", executionID, "status", status, "error", err)
		return
	}
	run, err := s.actions.runs.GetActionRun(ctx, executionID)
	if err != nil {
		s.logger.Error("failed to read ended action request", "execution_id", executionID, "error", err)
		return
	}
	s.logger.Info("action request ended", "action", run.Action, "execution_id", executionID, "status", status, "reason", reason, "error", errText)
	s.publishActionEvent(run, status)
}

// GetActionResult returns one execution requested by the calling session's
// architect. Any other execution — another architect's, or a manual launch —
// reads as not found.
func (s *Service) GetActionResult(ctx context.Context, sessionID, executionID string) (domain.ActionResult, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionResult{}, err
	}
	session, err := s.architectSession(ctx, sessionID)
	if err != nil {
		return domain.ActionResult{}, err
	}
	run, err := rt.runs.GetActionRun(ctx, strings.TrimSpace(executionID))
	if err != nil {
		if errors.As(err, new(*domain.NotFoundError)) {
			return domain.ActionResult{}, &domain.NotFoundError{Resource: "action_execution", ID: executionID}
		}
		return domain.ActionResult{}, err
	}
	if run.ArchitectKey == "" || run.ArchitectKey != session.ArchitectKey {
		return domain.ActionResult{}, &domain.NotFoundError{Resource: "action_execution", ID: executionID}
	}
	return s.actionResultOf(ctx, run, time.Now().UTC()), nil
}

func (s *Service) actionResultOf(ctx context.Context, run domain.ActionRun, now time.Time) domain.ActionResult {
	out := domain.ActionResult{
		ExecutionID: run.ID,
		Action:      run.Action,
		Status:      run.Status,
		Prompt:      run.Prompt,
		ProfileName: run.ProfileName,
		RequestedAt: run.CreatedAt,
		StartedAt:   run.StartedAt,
		EndedAt:     run.EndedAt,
		Reason:      run.Reason,
		Error:       run.Error,
		Summary:     run.Summary,
	}
	if run.StartedAt != nil {
		end := now
		if run.EndedAt != nil {
			end = *run.EndedAt
		}
		elapsed := max(int64(end.Sub(*run.StartedAt).Seconds()), 0)
		out.ElapsedSeconds = &elapsed
		out.OutputDir = run.OutputDir
	}
	// Activity is the agent's last reported status while it runs, attention
	// whether it is known to wait for the user; anything else is reported
	// unavailable rather than guessed.
	out.Attention = domain.ActionAgentAttention{State: domain.ActionAttentionUnavailable}
	if run.Status == domain.ActionRunRunning && run.SessionID != "" {
		out.Attention = s.actionAttention(run.SessionID)
		session, err := s.repo.GetSession(ctx, run.SessionID)
		switch {
		case err == nil:
			if session.CurrentRun != nil && session.CurrentRun.AgentStatus != "" {
				out.Activity = domain.ActionAgentActivity{Available: true, Status: session.CurrentRun.AgentStatus}
			}
		case errors.As(err, new(*domain.NotFoundError)):
		default:
			s.logger.Error("read action session for activity", "execution_id", run.ID, "session_id", run.SessionID, "error", err)
		}
	}
	return out
}

// WaitForActionResult waits up to timeout for the execution to change status
// or its agent's attention to change (attentionKey), and returns the result
// then. A final execution returns at once. Both are measured against the
// result when the wait began, so a wait on an unchanged condition — an agent
// already reported waiting for input — blocks until its timeout instead of
// returning at once. The subscription is taken before the first read, so a
// transition between the read and the wait is never missed. The wait
// observes the execution only: its end — timeout or the caller going away —
// never touches the Action.
func (s *Service) WaitForActionResult(ctx context.Context, sessionID, executionID string, timeout time.Duration) (domain.ActionWaitResult, error) {
	if timeout <= 0 || timeout > domain.MaxActionWaitSeconds*time.Second {
		return domain.ActionWaitResult{}, &domain.ValidationError{Field: "timeout_seconds", Message: fmt.Sprintf("must be between 1 and %d", domain.MaxActionWaitSeconds)}
	}
	sub := s.SubscribeActionEvents()
	defer func() { sub.Close() }()

	initial, err := s.GetActionResult(ctx, sessionID, executionID)
	if err != nil {
		return domain.ActionWaitResult{}, err
	}
	if initial.Status.Terminal() {
		return domain.ActionWaitResult{Result: initial}, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return domain.ActionWaitResult{}, ctx.Err()
		case <-timer.C:
			current, err := s.GetActionResult(ctx, sessionID, executionID)
			if err != nil {
				return domain.ActionWaitResult{}, err
			}
			changed := waitChanged(initial, current)
			return domain.ActionWaitResult{Result: current, Changed: changed, TimedOut: !changed}, nil
		case event, ok := <-sub.C():
			if !ok {
				// Closed for falling behind: resubscribe, then reread, so
				// whatever it missed is caught by the read.
				sub = s.SubscribeActionEvents()
			} else if event.ExecutionID != initial.ExecutionID {
				continue
			}
			current, err := s.GetActionResult(ctx, sessionID, executionID)
			if err != nil {
				return domain.ActionWaitResult{}, err
			}
			if waitChanged(initial, current) {
				return domain.ActionWaitResult{Result: current, Changed: true}, nil
			}
		}
	}
}

func waitChanged(initial, current domain.ActionResult) bool {
	return current.Status != initial.Status || attentionKey(current.Attention) != attentionKey(initial.Attention)
}
