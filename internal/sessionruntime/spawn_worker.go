package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

// Architect-requested worker launches. An architect asks to launch a worker
// for one of its own backlog tickets with spawnTicketWorker(ticketId, variant,
// workflows). The request is a blocking intent with createWorkTicket's
// wait-then-allow policy and no approval inputs:
//
//	request         session must be an architect; variant, workflow names and
//	                the whole ticket-session contract (board, backlog status,
//	                repo scope, worker context and workflow validity) are
//	                checked before anything is shown
//	deny            nothing is launched
//	approve/expiry  everything is resolved again against the live config,
//	                board and workspace, then the worker is launched through
//	                the desktop's own path (ticket session + CreateRun); the
//	                call returns once it is running, never waiting for it
//	launch failure  outcome error with the original cause; the session that
//	                could not be launched is removed
//
// The selection is exactly the named workflows (none when empty), shown in
// the approval and recorded on the session like a desktop selection; nothing
// is attached to the ticket. Duplicate workers are prevented by the backlog
// requirement, intent dedup for retries, and the session store's rule of one
// active session per ticket for launches racing each other or the desktop.

// RequestSpawnTicketWorker raises the approval request for one worker launch
// and waits for its resolution. Approval reports success only when the worker
// actually launched; a failed launch is outcome error with its reason.
func (s *Service) RequestSpawnTicketWorker(ctx context.Context, sessionID string, req domain.SpawnTicketWorkerRequest) (domain.SpawnTicketWorkerResponse, error) {
	var zero domain.SpawnTicketWorkerResponse
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	if session.SessionType != domain.SessionTypeArchitect {
		return zero, &domain.ValidationError{Field: "session_id", Message: "only architect sessions may spawn ticket workers"}
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return zero, &domain.ValidationError{Field: "session_id", Message: "session run is not running"}
	}
	ticketID := strings.TrimSpace(req.TicketID)
	if ticketID == "" {
		return zero, &domain.ValidationError{Field: "ticket_id", Message: "is required"}
	}
	// The board is always the calling architect's own, from the stored
	// session; GetTicket against its path cannot reach another project.
	architect, err := s.currentArchitect(session.ArchitectKey)
	if err != nil {
		return zero, err
	}
	ticket, err := s.tickets.GetTicket(ctx, architect.Path, ticketID)
	if err != nil {
		return zero, err
	}
	// The worker runs where its writable repositories are, so only variants
	// for that machine are eligible.
	machine, err := s.ticketMachine(architect, ticket)
	if err != nil {
		return zero, err
	}
	variant, err := s.requestedVariant(req.Variant, "worker", machine)
	if err != nil {
		return zero, err
	}
	workflows, err := workspacefs.ResolveWorkflowNames(architect.Path, session.ArchitectKey, req.Workflows)
	if err != nil {
		return zero, err
	}
	// The payload is the request's identity for dedup: the normalized
	// selection, so NAME and NAME.md retry as the same request. The title is
	// only the summary, so a retitled ticket does not mint a second request.
	names := workflowNames(workflows)
	additional := append([]string{}, ticket.AdditionalRepos...)
	sort.Strings(additional)
	payload := map[string]any{
		"ticket_id":        ticketID,
		"repo":             ticket.Repo,
		"additional_repos": additional,
		"variant":          variant,
		"workflows":        names,
	}
	if _, err := s.ticketSessionParams(ctx, session.ArchitectKey, architect, ticketID, workflowPaths(workflows)); err != nil {
		// A retry of a request that already launched its worker fails these
		// checks (the ticket left the backlog); it gets the original outcome
		// and worker instead, like any retry inside the replay window.
		key, keyErr := intentDedupKey(sessionID, domain.IntentTypeSpawnTicketWorker, payload)
		if keyErr != nil || !s.intents.Known(key) {
			return zero, err
		}
	}

	res, err := awaitIntent(ctx, s, intentSpec[domain.SpawnedTicketWorker]{
		SessionID: sessionID,
		Type:      domain.IntentTypeSpawnTicketWorker,
		Summary:   ticket.Title,
		Payload:   payload,
		Origin:    intentOrigin(session),
		Exec: func(ctx context.Context, _ domain.IntentInputValues) (domain.SpawnedTicketWorker, error) {
			// Detached: the approving request going away must not abort a
			// launch halfway.
			return s.launchRequestedWorker(context.WithoutCancel(ctx), session.ArchitectKey, ticketID, variant, names)
		},
	})
	if err != nil {
		return zero, err
	}
	out := domain.SpawnTicketWorkerResponse{IntentID: res.IntentID, Outcome: res.Outcome, Reason: res.Reason}
	if res.Outcome.Approved() {
		worker := res.Result
		out.Worker = &worker
	}
	s.logger.Info("worker spawn request resolved",
		"architect", session.ArchitectKey, "session_id", sessionID, "ticket_id", ticketID,
		"variant", variant, "workflows", names, "intent_id", res.IntentID, "outcome", res.Outcome, "reason", res.Reason)
	return out, nil
}

// launchRequestedWorker is the approved request's operation. The variant,
// the workflow names and the ticket-session contract are resolved again, since
// any of them may have changed while the request was pending; then the worker
// is launched through the same CreateRun the desktop uses, which announces it
// on the architect stream. Returning means launched and running.
func (s *Service) launchRequestedWorker(ctx context.Context, architectKey, ticketID, variant string, names []string) (domain.SpawnedTicketWorker, error) {
	architect, err := s.currentArchitect(architectKey)
	if err != nil {
		return domain.SpawnedTicketWorker{}, err
	}
	workflows, err := workspacefs.ResolveWorkflowNames(architect.Path, architectKey, names)
	if err != nil {
		return domain.SpawnedTicketWorker{}, err
	}
	params, err := s.ticketSessionParams(ctx, architectKey, architect, ticketID, workflowPaths(workflows))
	if err != nil {
		return domain.SpawnedTicketWorker{}, err
	}
	// The scope's machine may have moved, or the variant been reassigned,
	// while the request was pending.
	if _, err := s.requestedVariant(variant, "worker", params.Machine); err != nil {
		return domain.SpawnedTicketWorker{}, err
	}
	params.CreatedBy = domain.SessionCreatedByArchitect
	session, err := s.repo.CreateSession(ctx, params)
	if err != nil {
		return domain.SpawnedTicketWorker{}, fmt.Errorf("create worker session for ticket %s: %w", ticketID, err)
	}
	run, err := s.CreateRun(ctx, session.ID, domain.CreateSessionRunRequest{ProfileName: variant})
	if err != nil {
		// Nothing runs and the ticket was not claimed: remove the session so
		// the board shows no phantom worker and a new request starts clean.
		if delErr := s.repo.DeleteSession(ctx, session.ID); delErr != nil {
			s.logger.Error("remove worker session after failed launch", "session_id", session.ID, "ticket_id", ticketID, "error", delErr)
			err = errors.Join(err, fmt.Errorf("remove session %s after failed launch: %w", session.ID, delErr))
		}
		return domain.SpawnedTicketWorker{}, fmt.Errorf("launch worker for ticket %s with variant %s: %w", ticketID, variant, err)
	}
	recorded := session.Workflows
	if recorded == nil {
		recorded = []string{}
	}
	return domain.SpawnedTicketWorker{
		SessionID:      session.ID,
		RunID:          run.Run.ID,
		MainTerminalID: run.MainTerminalID,
		TicketID:       ticketID,
		Variant:        variant,
		Workflows:      workflowNames(workflows),
		WorkflowPaths:  recorded,
	}, nil
}

// ticketMachine is the execution machine of a ticket's writable scope (empty
// means local), from the current configuration.
func (s *Service) ticketMachine(architect config.ArchitectConfig, ticket domain.Ticket) (string, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return "", err
	}
	machine, err := cfg.ScopeMachine(architect, ticket.Repo, ticket.AdditionalRepos)
	if err != nil {
		return "", &domain.ValidationError{Field: "repos", Message: err.Error()}
	}
	return machine, nil
}

func workflowPaths(workflows []workspacefs.NamedWorkflow) []string {
	paths := make([]string, 0, len(workflows))
	for _, w := range workflows {
		paths = append(paths, w.Path)
	}
	return paths
}

func workflowNames(workflows []workspacefs.NamedWorkflow) []string {
	names := make([]string, 0, len(workflows))
	for _, w := range workflows {
		names = append(names, w.Name)
	}
	return names
}
