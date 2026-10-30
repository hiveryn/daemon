package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Companions (shared domain/companion.go).
//
// Every `companions` entry of an architect's hiveryn.yaml starts with each new
// architect session: once the architect's first run is live, its companion
// records are written (the configuration snapshot that session owns, status
// starting) and each companion is launched in the background through the
// ordinary session path — CreateRun, so local and remote (SSH/tmux, gateway)
// execution, variant snapshots and restore are the ticket worker's own. One
// companion_startup message tells the architect session every named outcome.
//
// The record is the ownership: it lives as long as the architect session,
// survives daemon restarts and is what shutdown, messaging and the conclusion
// guard consult — never the current configuration, whose edits apply to later
// architect sessions. A companion ends only through the cooperative shutdown
// (companion_shutdown.go); a launch that fails is cleaned up (remote
// termination confirmed) and reported, and a cleanup that cannot be confirmed
// stays visible as stop_failed and blocks the architect's conclusion.

// companionLaunchConcurrency bounds parallel companion launches of one
// architect session.
const companionLaunchConcurrency = 4

// companionStopGrace is how long the daemon waits after acknowledging a
// companion's completeCompanionShutdown before terminating it, so the
// acknowledgement reaches the agent first.
var companionStopGrace = 2 * time.Second

// companionRuntime holds the background companion work of this process.
type companionRuntime struct {
	// wg tracks background launches and stops (tests wait on it).
	wg sync.WaitGroup
}

// SetCompanionRepository wires companion persistence; without it architect
// sessions start no companions and the companion tools fail explicitly.
func (s *Service) SetCompanionRepository(repo domain.CompanionRepository) {
	s.companionRepo = repo
	if s.messages == nil {
		return
	}
	// Daemon-produced notices announce something that already happened.
	settled := func(context.Context, domain.AgentMessage) messageCommitVerdict {
		return messageCommitVerdict{committed: true}
	}
	s.messages.commitChecks[domain.AgentMessageCompanionStartup] = settled
	s.messages.commitChecks[domain.AgentMessageCompanionShutdownOutcome] = settled
	s.messages.commitChecks[domain.AgentMessageCompanionFailed] = settled
	// An agent's message left uncommitted was never reported to the sender as
	// accepted: delivering it now could duplicate a retry.
	unconfirmed := func(context.Context, domain.AgentMessage) messageCommitVerdict {
		return messageCommitVerdict{failed: "never confirmed to the sender as accepted (the daemon stopped while queueing it); not delivered"}
	}
	s.messages.commitChecks[domain.AgentMessageCompanionMessage] = unconfirmed
	s.messages.commitChecks[domain.AgentMessageArchitectMessage] = unconfirmed
	s.messages.commitChecks[domain.AgentMessageCompanionShutdownRequested] = s.checkShutdownRequestedCommit
}

var errCompanionsNotConfigured = errors.New("companion repository not configured")

func (s *Service) companionStore() (domain.CompanionRepository, error) {
	if s.companionRepo == nil {
		return nil, errCompanionsNotConfigured
	}
	return s.companionRepo, nil
}

func (s *Service) emitCompanionChanged(architectKey, sessionID string) {
	s.emitArchitectEvent(architectKey, domain.ArchitectEventCompanionChanged, "", sessionID)
}

// updateCompanion persists a record change and announces it.
func (s *Service) updateCompanion(ctx context.Context, c *domain.Companion) error {
	c.UpdatedAt = time.Now().UTC()
	if err := s.companionRepo.UpdateCompanion(context.WithoutCancel(ctx), *c); err != nil {
		s.logger.Error("update companion", "architect_session_id", c.ArchitectSessionID, "companion", c.Name, "status", c.Status, "error", err)
		return err
	}
	s.emitCompanionChanged(c.ArchitectKey, c.SessionID)
	return nil
}

// startCompanions records and launches the configured companions of an
// architect session whose run just became live. A session that already has
// companion records (a relaunch of the same session after a failed run)
// starts none: its companions were started, or reported, once.
func (s *Service) startCompanions(ctx context.Context, architect domain.Session) {
	if s.companionRepo == nil || architect.SessionType != domain.SessionTypeArchitect {
		return
	}
	existing, err := s.companionRepo.ListCompanions(ctx, architect.ID)
	if err != nil {
		s.logger.Error("list companions before startup", "session_id", architect.ID, "error", err)
		return
	}
	if len(existing) > 0 {
		return
	}
	cfg, err := s.currentConfig()
	if err != nil {
		s.logger.Error("read config for companion startup", "session_id", architect.ID, "error", err)
		return
	}
	conf := cfg.Architects[architect.ArchitectKey]
	if len(conf.Companions) == 0 {
		return
	}
	now := time.Now().UTC()
	records := make([]domain.Companion, 0, len(conf.Companions))
	for _, name := range configKeys(conf.Companions) {
		entry := conf.Companions[name]
		c := domain.Companion{
			ArchitectKey:       architect.ArchitectKey,
			ArchitectSessionID: architect.ID,
			Name:               name,
			Repo:               entry.Repo,
			Workdir:            conf.Repos[entry.Repo],
			Variant:            entry.Variant,
			Machine:            conf.RepoMachines[entry.Repo],
			Status:             domain.CompanionStarting,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if err := s.companionRepo.CreateCompanion(ctx, c); err != nil {
			s.logger.Error("record companion", "session_id", architect.ID, "companion", name, "error", err)
			c.Status, c.Error = domain.CompanionStartFailed, "its ownership record could not be written: "+err.Error()
		}
		records = append(records, c)
	}
	s.emitCompanionChanged(architect.ArchitectKey, "")
	s.companionRT.wg.Add(1)
	go func() {
		defer s.companionRT.wg.Done()
		s.launchCompanions(architect, records)
	}()
}

// launchCompanions launches the recorded companions in parallel and reports
// every outcome to the architect session in one message.
func (s *Service) launchCompanions(architect domain.Session, records []domain.Companion) {
	outcomes := make([]domain.Companion, len(records))
	sem := make(chan struct{}, companionLaunchConcurrency)
	var wg sync.WaitGroup
	for i, c := range records {
		if c.Status != domain.CompanionStarting {
			outcomes[i] = c
			continue
		}
		wg.Add(1)
		go func(i int, c domain.Companion) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outcomes[i] = s.launchCompanion(architect, c)
		}(i, c)
	}
	wg.Wait()
	s.reportCompanionStartup(context.Background(), architect, outcomes)
}

// launchCompanion creates and launches one companion session and records the
// outcome. A failed launch leaves nothing running: its session is removed
// once termination is confirmed, or kept and reported stop_failed.
func (s *Service) launchCompanion(architect domain.Session, c domain.Companion) domain.Companion {
	ctx := context.Background()
	fail := func(status domain.CompanionStatus, err error) domain.Companion {
		c.Status, c.Error = status, err.Error()
		_ = s.updateCompanion(ctx, &c)
		s.logger.Warn("companion did not start", "architect_session_id", architect.ID, "companion", c.Name, "status", status, "error", err)
		return c
	}
	cfg, err := s.currentConfig()
	if err != nil {
		return fail(domain.CompanionStartFailed, err)
	}
	variant, err := cfg.CompanionVariant(c.Name, c.Variant, c.Machine)
	if err != nil {
		return fail(domain.CompanionStartFailed, err)
	}
	c.Agent = variant.Agent
	ssh := ""
	if c.Machine != "" {
		ssh = cfg.Machines[c.Machine].SSH
		if ssh == "" {
			return fail(domain.CompanionStartFailed, fmt.Errorf("machine %q of repo %q has no SSH alias in machines.yaml", c.Machine, c.Repo))
		}
	} else if err := validateRepoPath(c.Workdir); err != nil {
		return fail(domain.CompanionStartFailed, fmt.Errorf("repo %q: %w", c.Repo, err))
	}
	instructions, err := companionInstructions()
	if err != nil {
		return fail(domain.CompanionStartFailed, err)
	}
	kickoff, err := renderCompanionKickoff(time.Now())
	if err != nil {
		return fail(domain.CompanionStartFailed, err)
	}
	token, port, err := newRemoteCredentials(c.Machine)
	if err != nil {
		return fail(domain.CompanionStartFailed, fmt.Errorf("remote gateway credentials: %w", err))
	}
	session, err := s.createStoredSession(ctx, domain.CreateSessionParams{
		Machine: c.Machine, SSH: ssh, RemoteToken: token, RemotePort: port,
		ArchitectKey:   c.ArchitectKey,
		SessionType:    domain.SessionTypeCompanion,
		ContextID:      c.Name,
		Prompt:         kickoff,
		Workdir:        c.Workdir,
		Instructions:   instructions,
		CreatedBy:      domain.SessionCreatedByCompanionStartup,
		OwnerSessionID: architect.ID,
	})
	if err != nil {
		return fail(domain.CompanionStartFailed, fmt.Errorf("create companion session: %w", err))
	}
	c.SessionID = session.ID
	if err := s.updateCompanion(ctx, &c); err != nil {
		// The record cannot name its session: remove the session rather
		// than leave an unowned one behind.
		if cleanupErr := s.discardCompanionSession(ctx, session.ID); cleanupErr != nil {
			return fail(domain.CompanionStopFailed, fmt.Errorf("record companion session: %w; cleanup: %v", err, cleanupErr))
		}
		c.SessionID = ""
		return fail(domain.CompanionStartFailed, fmt.Errorf("record companion session: %w", err))
	}
	if _, err := s.CreateRun(ctx, session.ID, domain.CreateSessionRunRequest{ProfileName: c.Variant}); err != nil {
		if cleanupErr := s.discardCompanionSession(ctx, session.ID); cleanupErr != nil {
			return fail(domain.CompanionStopFailed, fmt.Errorf("launch failed: %w; termination of what it started is not confirmed: %v", err, cleanupErr))
		}
		c.SessionID = ""
		return fail(domain.CompanionStartFailed, fmt.Errorf("launch failed: %w", err))
	}
	// A shutdown cannot race this: it requires status running.
	c.Status, c.Error = domain.CompanionRunning, ""
	_ = s.updateCompanion(ctx, &c)
	s.logger.Info("companion started", "architect_session_id", architect.ID, "companion", c.Name, "session_id", c.SessionID, "machine", c.Machine, "variant", c.Variant)
	return c
}

// discardCompanionSession confirms that nothing of a companion session runs
// any more (remote termination confirmed over SSH, local terminals killed)
// and removes the session. On an unconfirmed remote termination the session
// is kept and the error returned.
func (s *Service) discardCompanionSession(ctx context.Context, sessionID string) error {
	ctx, done, err := s.beginTeardown(ctx, sessionID)
	if err != nil {
		return err
	}
	defer done()
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		if errors.As(err, new(*domain.NotFoundError)) {
			return nil
		}
		return err
	}
	return s.endCompanionSession(ctx, session, "discarded", "companion session discarded", nil)
}

// endCompanionSession is the companion's single end path, run under
// beginTeardown: confirm termination first (remote over SSH), then record
// the end, kill local terminals and remove the session. An unconfirmed
// termination returns before anything local changes.
func (s *Service) endCompanionSession(ctx context.Context, session domain.Session, lifecycle, message string, raw map[string]any) error {
	if err := s.stopRemote(ctx, session); err != nil {
		return err
	}
	// The process ends before the run does: a companion keeps working after
	// acknowledging its shutdown, and its hooks must not outlive the run.
	s.cancelReceiverBridge(session.ID)
	if err := s.terminal.KillBySession(ctx, session.ID); err != nil && !errors.Is(err, errTerminalNotFound) {
		return fmt.Errorf("kill companion terminals: %w", err)
	}
	runID := ""
	if session.CurrentRun != nil {
		runID = session.CurrentRun.ID
		if session.CurrentRun.Status == domain.SessionRunStatusRunning {
			if err := s.repo.MarkRunCompleted(ctx, runID); err != nil {
				return err
			}
		}
	}
	if raw == nil {
		raw = map[string]any{}
	}
	raw["companion"] = session.ContextID
	if err := s.appendAndPublishSessionEnded(ctx, session.ID, runID, message, lifecycle, raw); err != nil {
		return err
	}
	s.closeRemoteConversation(session.ID)
	if err := s.repo.DeleteSession(ctx, session.ID); err != nil && !errors.As(err, new(*domain.NotFoundError)) {
		return err
	}
	s.cleanupDeletedSession(session.ID)
	s.emitArchitectEvent(session.ArchitectKey, domain.ArchitectEventSessionEnded, "", session.ID)
	return nil
}

// reportCompanionStartup sends the architect session one message naming
// every companion's startup outcome. The dedupe key makes it once per
// architect session, also when startup reconciliation reports it instead.
func (s *Service) reportCompanionStartup(ctx context.Context, architect domain.Session, outcomes []domain.Companion) {
	var b strings.Builder
	b.WriteString("Companion startup outcomes for this architect session:")
	items := make([]map[string]any, 0, len(outcomes))
	for _, c := range outcomes {
		location := config.MachineLabel(c.Machine)
		switch c.Status {
		case domain.CompanionRunning:
			fmt.Fprintf(&b, "\n- %s: started (repo %s, variant %s, %s). It orients itself and waits for assignments.", c.Name, c.Repo, c.Variant, location)
		default:
			fmt.Fprintf(&b, "\n- %s: %s (repo %s, variant %s, %s): %s", c.Name, companionStatusText(c.Status), c.Repo, c.Variant, location, c.Error)
		}
		items = append(items, map[string]any{"name": c.Name, "status": string(c.Status), "session_id": c.SessionID, "error": c.Error})
	}
	if _, err := s.EnqueueAgentMessage(ctx, AgentMessageParams{
		Kind:         domain.AgentMessageCompanionStartup,
		Recipient:    domain.AgentMessageRecipientSession + architect.ID,
		ArchitectKey: architect.ArchitectKey,
		DedupeKey:    string(domain.AgentMessageCompanionStartup) + ":" + architect.ID,
		Body:         b.String(),
		Payload:      map[string]any{"architect_session_id": architect.ID, "companions": items},
	}); err != nil {
		s.logger.Error("report companion startup", "architect_session_id", architect.ID, "error", err)
	}
}

func companionStatusText(status domain.CompanionStatus) string {
	switch status {
	case domain.CompanionStartFailed:
		return "failed to start; nothing of it is running"
	case domain.CompanionStopFailed:
		return "failed to start, and termination of what its launch started is NOT confirmed (it may still be running and blocks your conclusion)"
	default:
		return string(status)
	}
}

// notifyCompanionFailed tells the owning architect session that a companion
// became unavailable outside a shutdown.
func (s *Service) notifyCompanionFailed(ctx context.Context, c domain.Companion, reason string) {
	if _, err := s.EnqueueAgentMessage(ctx, AgentMessageParams{
		Kind:         domain.AgentMessageCompanionFailed,
		Recipient:    domain.AgentMessageRecipientSession + c.ArchitectSessionID,
		ArchitectKey: c.ArchitectKey,
		DedupeKey:    fmt.Sprintf("%s:%s:%s:%d", domain.AgentMessageCompanionFailed, c.ArchitectSessionID, c.Name, time.Now().UnixNano()),
		Body:         fmt.Sprintf("Companion %s is no longer available: %s. It is not running; messages to it are not delivered.", c.Name, reason),
		Payload:      map[string]any{"companion": c.Name, "companion_session_id": c.SessionID, "error": reason},
	}); err != nil {
		s.logger.Error("notify companion failure", "companion", c.Name, "architect_session_id", c.ArchitectSessionID, "error", err)
	}
}

// companionRunFailed is called when a local companion's run was marked failed
// (its process ended and could not be resumed, or its restore failed).
func (s *Service) companionRunFailed(ctx context.Context, session domain.Session, reason string) {
	if s.companionRepo == nil || session.SessionType != domain.SessionTypeCompanion {
		return
	}
	c, err := s.companionRepo.GetCompanionBySession(ctx, session.ID)
	if err != nil {
		s.logger.Error("companion record for failed run", "session_id", session.ID, "error", err)
		return
	}
	if c.Status.Settled() {
		return
	}
	c.Status, c.Error = domain.CompanionFailed, reason
	if err := s.updateCompanion(ctx, &c); err != nil {
		return
	}
	s.failSessionActions(ctx, session.ID, domain.ActionCancelled, "the companion's process ended")
	s.notifyCompanionFailed(ctx, c, reason)
}

// unsettledCompanions names the companions of an architect session that
// still block its conclusion.
func (s *Service) unsettledCompanions(ctx context.Context, architectSessionID string) ([]string, error) {
	if s.companionRepo == nil {
		return nil, nil
	}
	records, err := s.companionRepo.ListCompanions(ctx, architectSessionID)
	if err != nil {
		return nil, fmt.Errorf("list companions for architect conclusion: %w", err)
	}
	blocking := []string{}
	for _, c := range records {
		if !c.Status.Settled() {
			blocking = append(blocking, c.Name+" ("+string(c.Status)+")")
		}
	}
	return blocking, nil
}

// companionConclusionConflict is the architect conclusion guard.
func (s *Service) companionConclusionConflict(ctx context.Context, architectSessionID string) error {
	blocking, err := s.unsettledCompanions(ctx, architectSessionID)
	if err != nil {
		return err
	}
	if len(blocking) == 0 {
		return nil
	}
	return &domain.ConflictError{
		Resource: "session",
		Field:    "companions",
		Message:  fmt.Sprintf("Cannot conclude architect session: %d companion(s) not stopped: %s. Request their shutdown with requestCompanionShutdown and wait for Hiveryn's shutdown outcomes; a stop_failed companion needs its termination confirmed first", len(blocking), strings.Join(blocking, ", ")),
	}
}

// removeSettledCompanionSessions removes what is left of settled companions
// (a failed local companion keeps its session with the failed run) when the
// architect session concludes.
func (s *Service) removeSettledCompanionSessions(ctx context.Context, architectSessionID string) {
	if s.companionRepo == nil {
		return
	}
	records, err := s.companionRepo.ListCompanions(ctx, architectSessionID)
	if err != nil {
		s.logger.Error("list companions at architect conclusion", "session_id", architectSessionID, "error", err)
		return
	}
	for _, c := range records {
		if c.SessionID == "" || !c.Status.Settled() {
			continue
		}
		if err := s.discardCompanionSession(ctx, c.SessionID); err != nil {
			s.logger.Error("remove settled companion session", "session_id", c.SessionID, "companion", c.Name, "error", err)
		}
	}
}

// ListArchitectCompanions is the desktop's view of the companions of the
// architect's running session.
func (s *Service) ListArchitectCompanions(ctx context.Context, architectKey string) (domain.CompanionList, error) {
	repo, err := s.companionStore()
	if err != nil {
		return domain.CompanionList{}, err
	}
	if _, err := s.currentArchitect(architectKey); err != nil {
		return domain.CompanionList{}, err
	}
	sessions, err := s.repo.ListSessions(ctx)
	if err != nil {
		return domain.CompanionList{}, err
	}
	list := domain.CompanionList{Companions: []domain.Companion{}}
	byID := map[string]domain.Session{}
	for _, session := range sessions {
		byID[session.ID] = session
		if session.ArchitectKey == architectKey && session.SessionType == domain.SessionTypeArchitect &&
			session.CurrentRun != nil && session.CurrentRun.Status == domain.SessionRunStatusRunning {
			list.ArchitectSessionID = session.ID
		}
	}
	if list.ArchitectSessionID == "" {
		return list, nil
	}
	records, err := repo.ListCompanions(ctx, list.ArchitectSessionID)
	if err != nil {
		return domain.CompanionList{}, err
	}
	for _, c := range records {
		list.Companions = append(list.Companions, s.hydrateCompanion(ctx, c, byID))
	}
	return list, nil
}

// hydrateCompanion adds the live session values, the shutdown and the
// pending action requests.
func (s *Service) hydrateCompanion(ctx context.Context, c domain.Companion, sessions map[string]domain.Session) domain.Companion {
	c.PendingActions = s.actions.pendingFor(c.SessionID)
	if c.SessionID == "" {
		return c
	}
	if sd, err := s.companionRepo.GetShutdownBySession(ctx, c.SessionID); err == nil {
		c.Shutdown = &sd
	}
	session, ok := sessions[c.SessionID]
	if !ok {
		return c
	}
	session = s.hydrateSession(session)
	c.Connection = session.Connection
	if session.CurrentRun != nil {
		c.MainTerminalID = session.CurrentRun.MainTerminalID
		c.AgentStatus = session.CurrentRun.AgentStatus
	}
	return c
}

// companionOf returns the record of a running companion session (the caller
// of a companion tool), or the error the tool reports.
func (s *Service) companionOf(ctx context.Context, sessionID string) (domain.Session, domain.Companion, error) {
	repo, err := s.companionStore()
	if err != nil {
		return domain.Session{}, domain.Companion{}, err
	}
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, domain.Companion{}, err
	}
	if session.SessionType != domain.SessionTypeCompanion {
		return domain.Session{}, domain.Companion{}, &domain.ConflictError{Resource: "session", Field: "session_type", Message: "only companion sessions can use this tool"}
	}
	c, err := repo.GetCompanionBySession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, domain.Companion{}, err
	}
	return session, c, nil
}

// architectOf returns a running architect session (the caller of an
// architect companion tool).
func (s *Service) architectOf(ctx context.Context, sessionID string) (domain.Session, error) {
	if _, err := s.companionStore(); err != nil {
		return domain.Session{}, err
	}
	session, err := s.repo.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, err
	}
	if session.SessionType != domain.SessionTypeArchitect {
		return domain.Session{}, &domain.ConflictError{Resource: "session", Field: "session_type", Message: "only architect sessions can use this tool"}
	}
	if session.CurrentRun == nil || session.CurrentRun.Status != domain.SessionRunStatusRunning {
		return domain.Session{}, &domain.ConflictError{Resource: "session", Field: "status", Message: "architect session " + sessionID + " is not running"}
	}
	return session, nil
}

// ReconcileCompanions runs at startup, before sessions are restored. A
// companion still starting had its launch interrupted: a running session is
// kept (restored as usual), anything else is cleaned up and reported. A
// companion stopping had completed its handoff: its termination is finished
// instead of restoring it. Records whose architect session is gone are
// removed by the database with it.
func (s *Service) ReconcileCompanions(ctx context.Context) error {
	if s.companionRepo == nil {
		return nil
	}
	records, err := s.companionRepo.ListAllCompanions(ctx)
	if err != nil {
		return fmt.Errorf("list companions: %w", err)
	}
	interrupted := map[string][]domain.Companion{}
	order := []string{}
	for _, c := range records {
		switch c.Status {
		case domain.CompanionStarting:
			if _, seen := interrupted[c.ArchitectSessionID]; !seen {
				order = append(order, c.ArchitectSessionID)
			}
			interrupted[c.ArchitectSessionID] = append(interrupted[c.ArchitectSessionID], s.reconcileStartingCompanion(ctx, c))
		case domain.CompanionShutdownRequested, domain.CompanionStopping:
			// A completed handoff whose termination did not finish: finish it
			// instead of restoring the companion.
			sd, err := s.companionRepo.GetShutdownBySession(ctx, c.SessionID)
			if err != nil {
				s.logger.Error("shutdown record of a stopping companion", "companion", c.Name, "session_id", c.SessionID, "error", err)
				continue
			}
			if sd.Status == domain.ShutdownCompleting {
				c.Status = domain.CompanionStopping
				s.finishCompanionShutdown(ctx, c, sd)
			}
		}
	}
	sort.Strings(order)
	for _, architectSessionID := range order {
		architect, err := s.repo.GetSession(ctx, architectSessionID)
		if err != nil {
			continue
		}
		all, err := s.companionRepo.ListCompanions(ctx, architectSessionID)
		if err != nil {
			continue
		}
		s.reportCompanionStartup(ctx, architect, all)
	}
	return nil
}

func (s *Service) reconcileStartingCompanion(ctx context.Context, c domain.Companion) domain.Companion {
	if c.SessionID != "" {
		session, err := s.repo.GetSession(ctx, c.SessionID)
		if err == nil && session.CurrentRun != nil && session.CurrentRun.Status == domain.SessionRunStatusRunning {
			c.Status, c.Error = domain.CompanionRunning, ""
			_ = s.updateCompanion(ctx, &c)
			return c
		}
		if err == nil {
			if cleanupErr := s.discardCompanionSession(ctx, c.SessionID); cleanupErr != nil {
				c.Status, c.Error = domain.CompanionStopFailed, "the Hiveryn daemon restarted during its launch and termination of what the launch started is not confirmed: "+cleanupErr.Error()
				_ = s.updateCompanion(ctx, &c)
				return c
			}
		}
	}
	c.SessionID = ""
	c.Status, c.Error = domain.CompanionStartFailed, "the Hiveryn daemon restarted during its launch"
	_ = s.updateCompanion(ctx, &c)
	return c
}
