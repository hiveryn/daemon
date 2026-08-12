package sessionruntime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// intentSpec describes one approvable tool call. Exec is the side effect, and
// it runs only after the intent is approved (or auto-approved) — never before.
// Keeping the write inside Exec is what makes approval unbypassable: the agent
// has no path to the write except through an intent the daemon resolved.
type intentSpec[R any] struct {
	SessionID string
	Type      domain.IntentType
	Summary   string
	Payload   map[string]any // desktop-renderable; also the dedup input
	Origin    domain.IntentOrigin
	Exec      func(context.Context) (R, error)
}

// awaitIntent is the generic blocking wait shared by every approvable tool.
//
// The agent's MCP call parks here — held open by an untimed http.DefaultClient
// — until the user answers or the tool's policy fires. No polling.
//
// Ownership note: the intent deliberately does NOT die with the caller's ctx.
// An agent-runtime tool-call timeout cancels ctx and retries, which is exactly
// the case idempotency must survive; if ctx death killed the intent, the retry
// would find nothing cached and duplicate the write. So a detached owner
// goroutine holds the timer, and a caller whose ctx dies merely detaches.
func awaitIntent[R any](ctx context.Context, s *Service, spec intentSpec[R]) (domain.IntentResolution[R], error) {
	var zero domain.IntentResolution[R]

	policy, err := policyFor(spec.Type)
	if err != nil {
		return zero, err
	}

	// Type-erase once, here: the store holds heterogeneous intents, so it
	// cannot be generic, and this closure is what lets it stay type-agnostic.
	erased := func(ctx context.Context) (any, error) {
		v, err := spec.Exec(ctx)
		if err != nil {
			return nil, err
		}
		return v, nil
	}

	in := domain.Intent{
		ID:          uuid.NewString(),
		Type:        spec.Type,
		Summary:     spec.Summary,
		Payload:     spec.Payload,
		Origin:      spec.Origin,
		WaitSeconds: int(s.intentWaitWindow().Seconds()),
		Policy:      policy,
		CreatedAt:   time.Now().UTC(),
	}

	// auto-allow never enters the store: nothing to approve, nothing to dedup
	// against. Emit one resolved event so the action still shows up in the log.
	if policy == domain.IntentPolicyAutoAllow {
		v, execErr := spec.Exec(ctx)
		res := intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeApproved, Result: v}
		if execErr != nil {
			res = intentResult{IntentID: in.ID, Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
		}
		if pubErr := s.publishIntentResolved(ctx, in, res); pubErr != nil {
			s.logger.Error("publish intent resolved for auto-allow", "intent_id", in.ID, "error", pubErr)
		}
		return typedResolution[R](res)
	}

	key, err := intentDedupKey(spec.SessionID, spec.Type, spec.Payload)
	if err != nil {
		return zero, err
	}

	id, ch, replayed, disposition := s.intents.Begin(key, in, erased)
	switch disposition {
	case intentReplayed:
		// A retry inside the idempotency window: hand back the original
		// outcome rather than blocking on a dead intent or minting a duplicate.
		return typedResolution[R](replayed)
	case intentCreated:
		if err := s.publishIntentRequired(ctx, in); err != nil {
			// Roll back through Finish rather than a bare delete, so any
			// waiter that attached in the meantime also learns.
			s.intents.Finish(id, intentResult{
				Outcome: domain.IntentOutcomeError,
				Err:     err,
				Reason:  err.Error(),
			})
			return zero, fmt.Errorf("publish intent required: %w", err)
		}
		go s.runIntentPolicy(context.WithoutCancel(ctx), id, policy)
	case intentAttached:
		// An identical call is already in flight; just wait on it.
	}

	select {
	case <-ctx.Done():
		s.intents.Detach(id, ch)
		return zero, ctx.Err()
	case res := <-ch:
		return typedResolution[R](res)
	}
}

// typedResolution performs the single type assertion in the whole intent
// system. A mismatch is a programmer error (the spec's R disagrees with what
// Exec returned) and is surfaced loudly rather than silently zeroed.
func typedResolution[R any](res intentResult) (domain.IntentResolution[R], error) {
	out := domain.IntentResolution[R]{
		IntentID: res.IntentID,
		Outcome:  res.Outcome,
		Reason:   res.Reason,
	}
	if !res.Outcome.Approved() {
		return out, nil
	}
	v, ok := res.Result.(R)
	if !ok {
		var want R
		return domain.IntentResolution[R]{}, fmt.Errorf(
			"intent %s resolved with result type %T, want %T", res.IntentID, res.Result, want)
	}
	out.Result = v
	return out, nil
}

// runIntentPolicy owns the wait window for one intent. It runs detached from
// the requesting agent's ctx on purpose (see awaitIntent).
func (s *Service) runIntentPolicy(ctx context.Context, intentID string, policy domain.IntentPolicy) {
	window := s.intentWaitWindow()
	if window <= 0 {
		// A non-positive window means "wait indefinitely" — the intent stays
		// pending until the user answers or the session tears down. Config
		// normalization coerces this back to the default, so only a
		// hand-built config reaches here.
		return
	}

	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}

	pending, won := s.intents.Claim(intentID)
	if !won {
		// The desktop claimed it first and owns Finish plus the resolved event.
		// This Claim CAS is the race resolver for the whole system.
		return
	}

	res := s.resolveByPolicy(ctx, pending, policy)
	s.intents.Finish(intentID, res)
	if err := s.publishIntentResolved(ctx, pending.intent, res); err != nil {
		s.logger.Error("publish intent resolved after policy fire",
			"intent_id", intentID, "policy", policy, "error", err)
	}
}

// resolveByPolicy applies the tool's expiry behavior. "No desktop connected" is
// not a special case: it is indistinguishable from "the user didn't click", and
// both land here.
func (s *Service) resolveByPolicy(ctx context.Context, pending *pendingIntent, policy domain.IntentPolicy) intentResult {
	switch policy {
	case domain.IntentPolicyWaitThenAllow:
		v, err := pending.exec(ctx)
		if err != nil {
			return intentResult{Outcome: domain.IntentOutcomeError, Err: err, Reason: err.Error()}
		}
		return intentResult{Outcome: domain.IntentOutcomeAutoApproved, Result: v}
	case domain.IntentPolicyWaitThenDeny:
		return intentResult{
			Outcome: domain.IntentOutcomeAutoDenied,
			Reason:  fmt.Sprintf("no user response within %s", s.intentWaitWindow()),
		}
	default:
		return intentResult{
			Outcome: domain.IntentOutcomeError,
			Reason:  fmt.Sprintf("intent policy %q has no expiry behavior", policy),
		}
	}
}

// ApproveIntent resolves an intent as approved and performs its side effect.
// The exec runs outside the store mutex — a daemon lock is never held across
// the agent's block or across I/O.
func (s *Service) ApproveIntent(ctx context.Context, sessionID, intentID string) (domain.Intent, error) {
	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return domain.Intent{}, &domain.NotFoundError{Resource: "intent", ID: intentID}
	}

	v, execErr := pending.exec(ctx)
	res := intentResult{Outcome: domain.IntentOutcomeApproved, Result: v}
	if execErr != nil {
		res = intentResult{Outcome: domain.IntentOutcomeError, Err: execErr, Reason: execErr.Error()}
	}
	s.intents.Finish(intentID, res)

	if err := s.publishIntentResolved(ctx, pending.intent, res); err != nil {
		s.logger.Error("publish intent resolved after approve",
			"intent_id", intentID, "error", err)
	}
	if execErr != nil {
		// The user clicked approve and it failed. Surface it to them too —
		// telling the agent while silently returning 200 to the desktop would
		// hide a real failure.
		return pending.intent, fmt.Errorf("approve intent %s: %w", intentID, execErr)
	}
	return pending.intent, nil
}

// DenyIntent resolves an intent as denied. The side effect never runs.
func (s *Service) DenyIntent(ctx context.Context, sessionID, intentID, reason string) error {
	pending, ok := s.intents.ClaimForSession(sessionID, intentID)
	if !ok {
		return &domain.NotFoundError{Resource: "intent", ID: intentID}
	}

	res := intentResult{Outcome: domain.IntentOutcomeDeniedByUser, Reason: reason}
	s.intents.Finish(intentID, res)
	return s.publishIntentResolved(ctx, pending.intent, res)
}

// failPendingIntents resolves every intent still open on a session that is
// ending. Without it an intent would outlive its session and fire its side
// effect into a dead session when the policy expires — a gap that could not
// exist when there was one approval per session and it *was* the conclusion.
func (s *Service) failPendingIntents(ctx context.Context, sessionID, reason string) {
	for _, id := range s.intents.PendingForSession(sessionID) {
		pending, ok := s.intents.Claim(id)
		if !ok {
			continue
		}
		res := intentResult{Outcome: domain.IntentOutcomeError, Reason: reason}
		s.intents.Finish(id, res)
		if err := s.publishIntentResolved(ctx, pending.intent, res); err != nil {
			s.logger.Error("publish intent resolved on session teardown",
				"session_id", sessionID, "intent_id", id, "error", err)
		}
	}
}

// SetArchitectPublisher wires the architect-scoped event hub. It is a setter
// rather than a New() parameter because the hub is constructed after the
// service during startup.
func (s *Service) SetArchitectPublisher(publish func(string, domain.ArchitectEvent)) {
	s.publishArchitect = publish
}

// emitArchitectEvent invalidates the desktop's workspace view. It lives on the
// service (not the API handler) because a generic approve handler cannot know
// that a given intent touched a ticket — only the tool's Exec knows that.
// emitArchitectEvent stamps and fans out one architect event. ticketID is empty
// for reasons that are not about a ticket; sessionID is empty for reasons that
// are not about a session.
func (s *Service) emitArchitectEvent(architectKey string, reason domain.ArchitectEventReason, ticketID, sessionID string) {
	if s.publishArchitect == nil || architectKey == "" {
		return
	}
	s.publishArchitect(architectKey, domain.ArchitectEvent{
		Type:         domain.ArchitectEventType,
		ArchitectKey: architectKey,
		Reason:       reason,
		TicketID:     ticketID,
		SessionID:    sessionID,
		At:           time.Now().UTC(),
	})
}

// RequestCreateWorkTicket routes an agent's createWorkTicket through approval.
// The ticket is written only if the intent resolves approved, and the write
// happens here in the daemon — the agent has no unapproved path to it.
func (s *Service) RequestCreateWorkTicket(
	ctx context.Context, sessionID string, params domain.CreateTicketParams,
) (domain.IntentResolution[domain.Ticket], error) {
	var zero domain.IntentResolution[domain.Ticket]

	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return zero, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}

	// The architect key comes from the stored session, never from the request:
	// an agent cannot create tickets in an architect it wasn't spawned for.
	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return zero, err
	}

	// Validate before the popup, mirroring RequestConclusion: bad input goes
	// straight back to the agent and the user is never shown a doomed dialog.
	params.Title = strings.TrimSpace(params.Title)
	if params.Title == "" {
		return zero, &domain.ValidationError{Field: "title", Message: "is required"}
	}
	params.Repo = strings.TrimSpace(params.Repo)
	if params.Repo == "" {
		return zero, &domain.ValidationError{Field: "repo", Message: "is required"}
	}
	if _, ok := architect.Repos[params.Repo]; !ok {
		return zero, &domain.ValidationError{
			Field:   "repo",
			Message: fmt.Sprintf("repo key %q is not configured for architect %s", params.Repo, session.ArchitectKey),
		}
	}
	seen := map[string]struct{}{params.Repo: {}}
	for i, raw := range params.AdditionalRepos {
		key := strings.TrimSpace(raw)
		if key == "" {
			return zero, &domain.ValidationError{Field: "additional_repos", Message: "repo keys cannot be blank"}
		}
		if _, exists := seen[key]; exists {
			return zero, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is duplicated or overlaps primary repo", key)}
		}
		if _, ok := architect.Repos[key]; !ok {
			return zero, &domain.ValidationError{Field: "additional_repos", Message: fmt.Sprintf("repo key %q is not configured for architect %s", key, session.ArchitectKey)}
		}
		seen[key] = struct{}{}
		params.AdditionalRepos[i] = key
	}
	sort.Strings(params.AdditionalRepos)

	payload := map[string]any{
		"title":            params.Title,
		"repo":             params.Repo,
		"additional_repos": params.AdditionalRepos,
		"body":             params.Body,
		"references":       params.References,
	}

	return awaitIntent(ctx, s, intentSpec[domain.Ticket]{
		SessionID: sessionID,
		Type:      domain.IntentTypeCreateWorkTicket,
		Summary:   params.Title,
		Payload:   payload,
		Origin:    intentOrigin(session),
		Exec: func(ctx context.Context) (domain.Ticket, error) {
			p := params
			// Stamped here, NOT before the dedup hash. A per-call timestamp in
			// the hashed payload would make every retry hash differently and
			// silently disable dedup — the one bug that would quietly reinstate
			// duplicate tickets.
			p.Now = time.Now().UTC()

			ticket, err := s.tickets.CreateTicket(ctx, architect.Path, p)
			if err != nil {
				return domain.Ticket{}, err
			}
			s.emitArchitectEvent(session.ArchitectKey, domain.ArchitectEventTicketCreated, ticket.ID, "")
			return ticket, nil
		},
	})
}

// RequestSpawnTicketSession lets a running architect session request the same
// create-session then create-run flow used by the desktop. Both the ticket and
// profile are checked before presenting the intent and again inside Exec, so
// approval never acts on a stale board or config view.
func (s *Service) RequestSpawnTicketSession(
	ctx context.Context, architectSessionID, ticketID, profileName string,
) (domain.IntentResolution[domain.SpawnTicketSessionResult], error) {
	var zero domain.IntentResolution[domain.SpawnTicketSessionResult]

	session, err := s.repo.GetSession(ctx, architectSessionID)
	if err != nil {
		return zero, err
	}
	if session.SessionType != domain.SessionTypeArchitect {
		return zero, &domain.ValidationError{Field: "session_id", Message: "spawnTicketSession is only available to architect sessions"}
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return zero, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}

	ticketID = strings.TrimSpace(ticketID)
	if ticketID == "" {
		return zero, &domain.ValidationError{Field: "ticket_id", Message: "is required"}
	}
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return zero, &domain.ValidationError{Field: "profile", Message: "is required; no profile is inferred or defaulted"}
	}

	_, ticket, err := s.validateSpawnTicketRequest(ctx, session.ArchitectKey, ticketID, profileName)
	if err != nil {
		return zero, err
	}
	repos := append([]string{ticket.Repo}, ticket.AdditionalRepos...)
	payload := map[string]any{
		"ticket_id":        ticket.ID,
		"ticket_title":     ticket.Title,
		"repository_scope": repos,
		"profile":          profileName,
	}

	return awaitIntent(ctx, s, intentSpec[domain.SpawnTicketSessionResult]{
		SessionID: architectSessionID,
		Type:      domain.IntentTypeSpawnTicketSession,
		Summary:   fmt.Sprintf("Spawn ticket %s with profile %s", ticket.ID, profileName),
		Payload:   payload,
		Origin:    intentOrigin(session),
		Exec: func(execCtx context.Context) (domain.SpawnTicketSessionResult, error) {
			// Reload both YAML sources and ticket state at the moment approval wins.
			if _, _, err := s.validateSpawnTicketRequest(execCtx, session.ArchitectKey, ticketID, profileName); err != nil {
				return domain.SpawnTicketSessionResult{}, err
			}
			created, err := s.createSession(execCtx, domain.CreateSessionRequest{
				SessionType:  domain.SessionTypeTicket,
				ArchitectKey: session.ArchitectKey,
				TicketID:     ticketID,
			}, domain.SessionCreatedByArchitectMCP)
			if err != nil {
				return domain.SpawnTicketSessionResult{}, err
			}
			// CreateRun publishes ticket_moved and session_started itself, so this
			// path and the desktop's POST /api/sessions/{id}/runs announce the new
			// session identically. Emitting here as well would double-deliver.
			if _, err := s.CreateRun(execCtx, created.ID, domain.CreateSessionRunRequest{ProfileName: profileName}); err != nil {
				return domain.SpawnTicketSessionResult{}, fmt.Errorf("spawn ticket session %s: %w", created.ID, err)
			}
			return domain.SpawnTicketSessionResult{SessionID: created.ID}, nil
		},
	})
}

func (s *Service) validateSpawnTicketRequest(ctx context.Context, architectKey, ticketID, profileName string) (config.ArchitectConfig, domain.Ticket, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return config.ArchitectConfig{}, domain.Ticket{}, err
	}
	architect, ok := cfg.Architects[architectKey]
	if !ok {
		return config.ArchitectConfig{}, domain.Ticket{}, &domain.NotFoundError{Resource: "architect", ID: architectKey}
	}
	profile, ok := cfg.Variants[profileName]
	if !ok {
		return config.ArchitectConfig{}, domain.Ticket{}, &domain.ValidationError{Field: "profile", Message: fmt.Sprintf("configured profile %q does not exist", profileName)}
	}
	if _, err := parseAgentKind(profile.Agent); err != nil {
		return config.ArchitectConfig{}, domain.Ticket{}, &domain.ValidationError{Field: "profile", Message: fmt.Sprintf("configured profile %q is unusable: %v", profileName, err)}
	}
	ticket, err := s.tickets.GetTicket(ctx, architect.Path, ticketID)
	if err != nil {
		return config.ArchitectConfig{}, domain.Ticket{}, err
	}
	if ticket.Status != domain.TicketStatusBacklog {
		return config.ArchitectConfig{}, domain.Ticket{}, &domain.ConflictError{Resource: "ticket", Field: "status", Message: "ticket " + ticketID + " must be in backlog to spawn"}
	}
	return architect, ticket, nil
}
