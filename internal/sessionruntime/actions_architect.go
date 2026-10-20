package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/actionfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Agent-requested Actions. An architect or one of its ticket workers
// discovers the Actions the project's hiveryn.yaml lists under
// availableActions and requests one with executeAction(name, prompt,
// variant). The variant is the requester's explicit choice — never a default —
// and is checked before anything is shown. The request is a blocking intent
// with createWorkTicket's wait-then-allow policy and no approval inputs; its
// intent id IS the execution id, so the requester holds one id from request
// to result:
//
//	executeAction   execution recorded pending_approval with its variant
//	                (trigger architect or worker), intent shown; the call
//	                waits for the resolution
//	deny            execution denied, with the user's reason; it never runs
//	approve/expiry  availability, variant and definition rechecked,
//	                pending_approval → running under the single-run rule,
//	                agent session launched; the call returns the running
//	                execution and from here the Action lifecycle (actions.go)
//	                owns the record — the call never waits for delivery
//	start failure   execution failed with why it could not start — including
//	                another execution of the action winning meanwhile
//	session end     the requesting session ended before approval: failed, it
//	                never ran
//	restart         pending requests are failed (ReconcileActionRuns); a
//	                running execution follows the Action restart rules
//
// Results are scoped to the project (architect key), not the requesting
// session, so a later session of the same project — architect or worker — can
// still read them and another project cannot. Action agents cannot request
// Actions.

const actionRequestFailedOnRestart = "daemon restarted before this request was approved; it never ran"

// requesterSession returns the calling session, which must be an architect or
// ticket session. The architect key — the project whose availableActions and
// results apply — always comes from the stored session.
func (s *Service) requesterSession(ctx context.Context, sessionID string) (domain.Session, error) {
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, err
	}
	switch session.SessionType {
	case domain.SessionTypeArchitect, domain.SessionTypeTicket:
		return session, nil
	default:
		return domain.Session{}, &domain.ValidationError{Field: "session_id", Message: "Actions are available to architect and ticket sessions only"}
	}
}

// AvailableActions lists the project's configured Actions in config order,
// each enriched from its registered checkout, with where it runs and the
// variants eligible there. A configured name that is not registered is listed
// invalid with a problem, never dropped. Remote checkouts are inspected
// concurrently, each bounded.
func (s *Service) AvailableActions(ctx context.Context, sessionID string) (domain.AvailableActionList, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.AvailableActionList{}, err
	}
	session, err := s.requesterSession(ctx, sessionID)
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
	out := domain.AvailableActionList{Actions: make([]domain.ActionDefinition, len(architect.AvailableActions))}
	errs := make([]error, len(architect.AvailableActions))
	var wg sync.WaitGroup
	for i, name := range architect.AvailableActions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			def, _, err := s.availableDefinition(ctx, name, actionListInspectTimeout)
			if err != nil {
				errs[i] = err
				return
			}
			item := def.ActionDefinition
			if run, ok := running[item.Name]; ok {
				item.RunningExecutionID = run.ID
			}
			out.Actions[i] = item
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return domain.AvailableActionList{}, err
	}
	return out, nil
}

// availableDefinition inspects a configured action where it is registered; an
// unregistered name is an invalid definition that says so.
func (s *Service) availableDefinition(ctx context.Context, name string, timeout time.Duration) (actionfs.Definition, actionLocation, error) {
	def, loc, err := s.actionDefinition(ctx, name, timeout)
	if err == nil {
		return def, loc, nil
	}
	if !errors.As(err, new(*domain.NotFoundError)) {
		return actionfs.Definition{}, actionLocation{}, err
	}
	registry := s.actions.registry
	return actionfs.Definition{ActionDefinition: domain.ActionDefinition{
		Name:     name,
		Variants: []string{},
		Valid:    false,
		Problems: []domain.ActionProblem{{Path: registry, Message: "listed in availableActions but not registered in " + registry}},
	}}, actionLocation{}, nil
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
	return &domain.ValidationError{Field: "name", Message: fmt.Sprintf("action %q is not available to project %s: it must be listed under availableActions in hiveryn.yaml (see getAvailableActions)", name, architectKey)}
}

// RequestExecuteAction raises the approval request for one execution and
// waits for its resolution, like createWorkTicket. Approval — the user's, or
// the policy's when the window expires — launches the Action and returns it
// running; the Action then runs independently. The execution record is
// returned whatever the outcome, so a denial or a failed start stays visible
// under the execution id.
func (s *Service) RequestExecuteAction(ctx context.Context, sessionID string, req domain.ExecuteActionRequest) (domain.ExecuteActionResponse, error) {
	var zero domain.ExecuteActionResponse
	rt, err := s.actionRuntime()
	if err != nil {
		return zero, err
	}
	session, err := s.requesterSession(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return zero, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return zero, &domain.ValidationError{Field: "name", Message: "is required"}
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return zero, &domain.ValidationError{Field: "prompt", Message: "is required"}
	}
	allowed, err := s.architectAllows(session.ArchitectKey, name)
	if err != nil {
		return zero, err
	}
	if !allowed {
		return zero, notAvailableError(session.ArchitectKey, name)
	}
	def, loc, err := s.availableDefinition(ctx, name, actionInspectTimeout)
	if err != nil {
		return zero, err
	}
	if !def.Valid {
		return zero, actionfs.InvalidError(def)
	}
	// The variant must run where the Action does, wherever the requester is.
	variant, err := s.requestedActionVariant(req.Variant, loc.machine)
	if err != nil {
		return zero, err
	}
	running, err := rt.runs.RunningActionRuns(ctx)
	if err != nil {
		return zero, err
	}
	if run, ok := running[name]; ok {
		return zero, &domain.ConflictError{Resource: "action", Field: "name", Message: fmt.Sprintf("%s is already running (execution %s); only one execution of an action may run at a time — request it again once that execution ends", name, run.ID)}
	}

	// The execution id is the intent id, which awaitIntent mints; the
	// creator's Created hook records it for the Exec below. A retry attaches
	// to the original intent and never runs this spec's hooks or Exec. The
	// variant is in the payload, so it is part of the request's identity: a
	// request for another variant never replays this one.
	var executionID string
	res, err := awaitIntent(ctx, s, intentSpec[domain.ActionRun]{
		SessionID: sessionID,
		Type:      domain.IntentTypeExecuteAction,
		Summary:   "Run " + name,
		Payload: map[string]any{
			"action":  name,
			"prompt":  prompt,
			"variant": variant,
			"machine": loc.machine,
		},
		Origin: intentOrigin(session),
		Hooks: intentHooks{
			Created: func(ctx context.Context, in domain.Intent) error {
				executionID = in.ID
				run := domain.ActionRun{
					ID:                 in.ID,
					Action:             name,
					Trigger:            actionRequestTrigger(session),
					Status:             domain.ActionRunPendingApproval,
					Prompt:             prompt,
					ProfileName:        variant,
					RepoPath:           loc.path,
					Machine:            loc.machine,
					CreatedAt:          in.CreatedAt,
					ArchitectKey:       session.ArchitectKey,
					RequesterSessionID: session.ID,
				}
				if session.SessionType == domain.SessionTypeTicket {
					run.RequesterTicketID = session.ContextID
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
		Exec: func(ctx context.Context, _ domain.IntentInputValues) (domain.ActionRun, error) {
			// Detached: the approving request going away must not abort a
			// launch halfway and leave the execution running without a session.
			return s.startRequestedAction(context.WithoutCancel(ctx), executionID, session.ArchitectKey, variant)
		},
	})
	if err != nil {
		return zero, err
	}
	if res.IntentID == "" {
		return zero, errors.New("executeAction resolved without an execution id")
	}
	result, err := s.GetActionResult(ctx, sessionID, res.IntentID)
	if err != nil {
		return zero, fmt.Errorf("read execution %s after its request resolved %s: %w", res.IntentID, res.Outcome, err)
	}
	response := domain.ExecuteActionResponse{Outcome: res.Outcome, Reason: res.Reason, Result: result}
	if res.Outcome == domain.IntentOutcomeApproved || res.Outcome == domain.IntentOutcomeAutoApproved {
		notification := s.actionNotification(ctx, session)
		response.Notification = &notification
	}
	s.logger.Info("action request resolved", "action", name, "variant", variant, "execution_id", res.IntentID, "architect", session.ArchitectKey, "session_id", sessionID, "outcome", res.Outcome, "status", result.Status)
	return response, nil
}

// actionNotification says whether the action_ended notice can reach the
// requester: an architect is told in whichever of its sessions runs when the
// execution ends; a worker is told in this session. Delivery is unavailable
// for a session on another machine, an agent without a message route or a
// daemon without the outbox — then the response says so instead of promising
// a notice, and the requester reads getActionResult itself.
func (s *Service) actionNotification(ctx context.Context, session domain.Session) domain.ActionNotification {
	if _, err := s.messageRepo(); err != nil {
		return domain.ActionNotification{Reason: "message delivery is not configured in this daemon"}
	}
	if session.SessionType == domain.SessionTypeTicket && session.Machine != "" {
		return domain.ActionNotification{Reason: "this session runs on " + config.MachineLabel(session.Machine) + ", and message delivery to remote sessions is not available"}
	}
	current, err := s.repo.GetSession(ctx, session.ID)
	if err != nil {
		return domain.ActionNotification{Reason: "the requesting session cannot be read: " + err.Error()}
	}
	if current.CurrentRun == nil || current.CurrentRun.ProfileSnapshot == nil {
		return domain.ActionNotification{Reason: "the requesting session has no agent profile snapshot"}
	}
	kind, err := parseAgentKind(current.CurrentRun.ProfileSnapshot.Agent)
	if err != nil {
		return domain.ActionNotification{Reason: "the requesting agent kind: " + err.Error()}
	}
	if _, _, ok := s.messageDeliverer(kind); !ok {
		return domain.ActionNotification{Reason: "agent " + string(kind) + " cannot receive messages"}
	}
	return domain.ActionNotification{Available: true}
}

// actionRequestTrigger attributes a request to the kind of session that made
// it; requesterSession admits only these two.
func actionRequestTrigger(session domain.Session) domain.ActionRunTrigger {
	if session.SessionType == domain.SessionTypeTicket {
		return domain.ActionRunTriggerWorker
	}
	return domain.ActionRunTriggerArchitect
}

// requestedActionVariant checks the requester's variant for an Action. The
// Action runs on its registered machine whoever requests it, so only the
// variants assigned to that machine are eligible.
func (s *Service) requestedActionVariant(raw, machine string) (string, error) {
	return s.requestedVariant(raw, "Action", machine)
}

// requestedVariant checks an agent's variant choice against the current
// configuration; subject names what the variant runs ("Action", "worker") and
// machine where it runs (empty means local). Only variants assigned to that
// location are eligible. There is no default: a missing, unknown or
// other-machine variant is an error that names the target location, lists
// the eligible variants and tells the agent to ask the user which one to use.
func (s *Service) requestedVariant(raw, subject, machine string) (string, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return "", err
	}
	if len(cfg.Variants) == 0 {
		return "", &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("no agent variants are configured (variants.yaml), so no %s can run; tell the user, who must configure one first", subject)}
	}
	target := config.MachineLabel(machine)
	names := cfg.VariantsForMachine(machine)
	if len(names) == 0 {
		hint := "a variant without machine"
		if machine != "" {
			hint = "a variant with machine: " + machine
		}
		return "", &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("no agent variant is configured for %s, where this %s runs, so it cannot run; tell the user, who must add %s to variants.yaml first", target, subject, hint)}
	}
	choices := make([]string, 0, len(names))
	for _, name := range names {
		choices = append(choices, fmt.Sprintf("%s (%s)", name, cfg.Variants[name].Agent))
	}
	available := fmt.Sprintf("Variants configured for %s: %s", target, strings.Join(choices, ", "))
	variant := strings.TrimSpace(raw)
	if variant == "" {
		return "", &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("is required and has no default. Ask the user which agent variant should run this %s, then request it again with that variant. %s", subject, available)}
	}
	profile, ok := cfg.Variants[variant]
	if !ok {
		return "", &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("%q is not a configured agent variant. Ask the user which one should run this %s. %s", variant, subject, available)}
	}
	if profile.Machine != machine {
		return "", &domain.ValidationError{Field: "variant", Message: fmt.Sprintf("%q is configured for %s, but this %s runs on %s. Ask the user which one should run it. %s", variant, config.MachineLabel(profile.Machine), subject, target, available)}
	}
	if _, err := parseAgentKind(profile.Agent); err != nil {
		return "", err
	}
	return variant, nil
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

	defer rt.lockLaunch(run.Action)()

	// The registration is read again: the Action may have moved while the
	// request waited, and the variant must suit where it runs now.
	def, loc, err := s.availableDefinition(ctx, run.Action, actionInspectTimeout)
	if err != nil {
		return fail(err)
	}
	if !def.Valid {
		return fail(actionfs.InvalidError(def))
	}
	if _, err := s.requestedActionVariant(variant, loc.machine); err != nil {
		return fail(err)
	}
	outputDir, err := s.actionOutputDir(ctx, loc, run.Action, executionID)
	if err != nil {
		return fail(err)
	}
	now := time.Now().UTC()
	if def.ArtifactMode == "none" {
		outputDir = ""
	}
	start := domain.ActionRunStart{ArtifactContract: def.Artifacts, ArtifactMode: def.ArtifactMode, ProfileName: variant, Machine: loc.machine, RepoPath: loc.path, OutputDir: outputDir, At: now}
	if err := rt.runs.StartActionRun(ctx, executionID, start); err != nil {
		return fail(err)
	}
	run.ArtifactContract = def.Artifacts
	run.ArtifactMode = def.ArtifactMode
	run.Status = domain.ActionRunRunning
	run.ProfileName = variant
	run.StartedAt = &now
	run.RepoPath, run.Machine, run.OutputDir = loc.path, loc.machine, outputDir
	if _, err := s.launchActionSession(ctx, def, loc, run, 0, 0); err != nil {
		// launchActionSession already failed the running execution.
		return domain.ActionRun{}, err
	}
	return s.GetActionRun(ctx, executionID)
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

// GetActionResult returns one execution requested within the calling
// session's project, by its architect or any of its workers. Any other
// execution — another project's, or a manual launch — reads as not found. A
// completed execution's package is made available on the caller's machine
// (callerArtifacts), waiting briefly for a copy in progress.
func (s *Service) GetActionResult(ctx context.Context, sessionID, executionID string) (domain.ActionResult, error) {
	return s.actionResult(ctx, sessionID, executionID, actionResultDeliveryWait)
}

// actionResult is GetActionResult waiting up to deliveryWait for the caller's
// copy of a completed package.
func (s *Service) actionResult(ctx context.Context, sessionID, executionID string, deliveryWait time.Duration) (domain.ActionResult, error) {
	rt, err := s.actionRuntime()
	if err != nil {
		return domain.ActionResult{}, err
	}
	session, err := s.requesterSession(ctx, sessionID)
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
	out := s.actionResultOf(ctx, run, time.Now().UTC())
	out.Artifacts, out.OutputDir = s.callerArtifacts(ctx, session, run, deliveryWait)
	return out, nil
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
		Machine:     run.Machine,
		Artifacts:   domain.ActionArtifacts{State: domain.ActionArtifactsNone},
	}
	if run.StartedAt != nil {
		end := now
		if run.EndedAt != nil {
			end = *run.EndedAt
		}
		elapsed := max(int64(end.Sub(*run.StartedAt).Seconds()), 0)
		out.ElapsedSeconds = &elapsed
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
