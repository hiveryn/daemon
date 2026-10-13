package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hiveryn/daemon/internal/actionpkg"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Artifact packages. A completed execution's delivered folder is frozen into
// a Hiveryn-managed package on the execution machine; callers elsewhere get a
// verified copy on their own machine. Every managed directory has an
// ownership record (action_artifacts) written before the directory is
// created:
//
//	snapshot   on approved completion: the delivered folder is copied into
//	           packages/<id> (actionpkg: regular files and directories only,
//	           limits enforced per entry, partial copy renamed only once
//	           verified); the execution is recorded completed together with
//	           its ready package, so a completed execution always has one
//	deliver    getActionResult/waitForActionResult from a caller on another
//	           machine copy the package into deliveries/<id> there, through
//	           this daemon (a remote-to-remote copy passes through the
//	           laptop); a caller on the execution machine reads the package
//	           itself. A copy runs detached and bounded; the tools wait for
//	           it briefly and report copying meanwhile. A ready copy is reused,
//	           a failed one retried by the next read. Delivery never changes
//	           the execution's status
//	expire     packages and copies expire a retention period after
//	           completion (reads do not extend it) and are reported expired;
//	           the execution record and summary stay
//	cleanup    at daemon startup only, in the background: interrupted copies
//	           and expired directories are removed with short, bounded
//	           attempts, failing fast per machine; a failure is logged and
//	           kept on the record for the next startup. Records without a
//	           managed root (packages that predate snapshots, at their
//	           original folder) are never removed, and neither is an agent's
//	           delivered folder
//
// Managed roots: HIVERYN_HOME/action-artifacts locally, and
// ~/.local/state/hiveryn-actions/artifacts in a remote account.

const (
	// actionDeliveryTimeout bounds one copy to a caller's machine.
	actionDeliveryTimeout = 10 * time.Minute
	// actionResultDeliveryWait is how long getActionResult waits for a copy
	// before reporting it copying; waitForActionResult waits up to its own
	// timeout instead.
	actionResultDeliveryWait = 20 * time.Second
	// actionCleanupTimeout bounds one removal during startup cleanup.
	actionCleanupTimeout = 15 * time.Second
	// interruptedCopyError describes a copy found unfinished at startup.
	interruptedCopyError = "interrupted before it completed (the daemon stopped); its partial copy was removed"
)

// artifactState is the in-memory coordination of artifact operations: remote
// home directories, the operations in flight per execution (a snapshot or a
// copy, both of which use the package), and the records being cleaned up. A
// package is never removed while any operation on its execution runs, a copy
// never while it is being made, and nothing starts on a record being removed.
type artifactState struct {
	mu        sync.Mutex
	homes     map[string]string
	active    map[string]int
	cleaning  map[string]bool // by recordKey
	transfers map[string]*deliveryTransfer
	// failures keeps a copy failure that has no record to carry it (the
	// destination could not even be resolved), by transfer key.
	failures map[string]string
}

type deliveryTransfer struct {
	done chan struct{}
}

func transferKey(executionID, machine string) string { return executionID + "\x00" + machine }

func recordKey(executionID string, kind domain.ActionArtifactKind, machine string) string {
	return executionID + "\x00" + string(kind) + "\x00" + machine
}

// begin admits a snapshot of an execution's package unless the package is
// being cleaned up.
func (a *artifactState) begin(executionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cleaningPackage(executionID) {
		return fmt.Errorf("expired artifacts of execution %s are being removed; try again shortly", executionID)
	}
	if a.active == nil {
		a.active = map[string]int{}
	}
	a.active[executionID]++
	return nil
}

func (a *artifactState) end(executionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[executionID]--; a.active[executionID] <= 0 {
		delete(a.active, executionID)
	}
}

// cleaningPackage reports whether the execution's package, on whichever
// machine, is being removed. Callers hold mu.
func (a *artifactState) cleaningPackage(executionID string) bool {
	prefix := executionID + "\x00" + string(domain.ActionArtifactPackage) + "\x00"
	for key := range a.cleaning {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// claimCleanup reserves one record for removal when nothing uses it: a
// package while no operation on its execution runs, a copy while it is not
// being made.
func (a *artifactState) claimCleanup(rec domain.ActionArtifactRecord) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := recordKey(rec.ExecutionID, rec.Kind, rec.Machine)
	if a.cleaning[key] {
		return false
	}
	if rec.Kind == domain.ActionArtifactPackage && a.active[rec.ExecutionID] > 0 {
		return false
	}
	if _, copying := a.transfers[transferKey(rec.ExecutionID, rec.Machine)]; rec.Kind == domain.ActionArtifactDelivery && copying {
		return false
	}
	if a.cleaning == nil {
		a.cleaning = map[string]bool{}
	}
	a.cleaning[key] = true
	return true
}

func (a *artifactState) releaseCleanup(rec domain.ActionArtifactRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cleaning, recordKey(rec.ExecutionID, rec.Kind, rec.Machine))
}

// remoteHome returns, and caches, an SSH account's home directory.
func (s *Service) remoteHome(ctx context.Context, alias string) (string, error) {
	a := &s.actions.artifacts
	a.mu.Lock()
	home, ok := a.homes[alias]
	a.mu.Unlock()
	if ok {
		return home, nil
	}
	home, err := actionpkg.RemoteHome(ctx, alias)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.homes == nil {
		a.homes = map[string]string{}
	}
	a.homes[alias] = home
	a.mu.Unlock()
	return home, nil
}

// artifactsRoot is the managed artifact directory on machine.
func (s *Service) artifactsRoot(ctx context.Context, machine, alias string) (string, error) {
	if machine == "" {
		return s.actions.artifactsRoot, nil
	}
	home, err := s.remoteHome(ctx, alias)
	if err != nil {
		return "", fmt.Errorf("resolve the artifact directory on %s: %w", config.MachineLabel(machine), err)
	}
	return path.Join(home, remoteActionsStateDir, "artifacts"), nil
}

func joinOn(machine string, elem ...string) string {
	if machine == "" {
		return filepath.Join(elem...)
	}
	return path.Join(elem...)
}

func (s *Service) actionPackageLimits() actionpkg.Limits {
	limits, _ := s.actionPackageSettings()
	return limits
}

func (s *Service) actionPackageSettings() (actionpkg.Limits, time.Duration) {
	cfg, err := s.currentConfig()
	if err != nil {
		cfg = s.cfg
	}
	maxBytes, maxFiles, retention := cfg.ActionPackageLimits()
	return actionpkg.Limits{MaxBytes: maxBytes, MaxFiles: maxFiles}, retention
}

func artifactLocation(rec domain.ActionArtifactRecord) actionpkg.Location {
	if rec.Machine == "" {
		return actionpkg.Location{Path: rec.Path}
	}
	return actionpkg.Location{Alias: rec.SSH, Path: rec.Path}
}

// snapshotPackage freezes the delivered folder of a concluding execution into
// its managed package on the execution machine and returns the ready record,
// which the caller persists together with the completed status. Its
// ownership record is written (copying) before the copy starts; a failed
// snapshot is recorded failed, its partial copy removed, and returned as a
// ValidationError on delivery_dir when the folder itself is at fault.
func (s *Service) snapshotPackage(ctx context.Context, session domain.Session, run domain.ActionRun, deliveryDir string, now time.Time) (domain.ActionArtifactRecord, error) {
	rt := s.actions
	if err := rt.artifacts.begin(run.ID); err != nil {
		return domain.ActionArtifactRecord{}, err
	}
	defer rt.artifacts.end(run.ID)

	limits, retention := s.actionPackageSettings()
	root, err := s.artifactsRoot(ctx, run.Machine, session.SSH)
	if err != nil {
		return domain.ActionArtifactRecord{}, err
	}
	rec := domain.ActionArtifactRecord{
		ExecutionID: run.ID,
		Kind:        domain.ActionArtifactPackage,
		Machine:     run.Machine,
		SSH:         session.SSH,
		Path:        joinOn(run.Machine, root, "packages", run.ID),
		ManagedRoot: root,
		State:       domain.ActionArtifactsCopying,
		CreatedAt:   now,
	}
	if err := rt.runs.PutActionArtifact(ctx, rec); err != nil {
		return domain.ActionArtifactRecord{}, err
	}
	src := actionpkg.Location{Alias: session.SSH, Path: deliveryDir}
	if run.Machine == "" {
		src.Alias = ""
	}
	stats, err := actionpkg.Copy(ctx, src, artifactLocation(rec), limits)
	if err == nil && stats.Files == 0 {
		err = emptyPackageError(deliveryDir)
	}
	if err != nil {
		s.recordFailedCopy(rec, err)
		if !errors.As(err, new(*domain.ValidationError)) && !errors.As(err, new(*actionpkg.Error)) {
			s.logger.Error("action package snapshot failed", "execution_id", run.ID, "delivery_dir", deliveryDir, "package", rec.Path, "machine", run.Machine, "error", err)
		}
		return domain.ActionArtifactRecord{}, packageError("delivery_dir", err)
	}
	ready := time.Now().UTC()
	expires := now.Add(retention)
	rec.State, rec.Files, rec.Bytes, rec.ReadyAt, rec.ExpiresAt = domain.ActionArtifactsReady, stats.Files, stats.Bytes, &ready, &expires
	return rec, nil
}

// recordFailedCopy records a failed copy and removes whatever it left. A
// removal that is not confirmed stays on the record for startup cleanup.
func (s *Service) recordFailedCopy(rec domain.ActionArtifactRecord, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), actionCleanupTimeout)
	defer cancel()
	rec.State, rec.Error, rec.CleanupError = domain.ActionArtifactsFailed, cause.Error(), ""
	if err := actionpkg.Remove(ctx, artifactLocation(rec), rec.ManagedRoot); err != nil {
		rec.CleanupError = err.Error()
		s.logger.Error("partial artifact copy not removed; retried at next startup", "execution_id", rec.ExecutionID, "path", rec.Path, "machine", rec.Machine, "error", err)
	}
	if err := s.actions.runs.PutActionArtifact(ctx, rec); err != nil {
		s.logger.Error("record failed artifact copy", "execution_id", rec.ExecutionID, "path", rec.Path, "error", err)
	}
}

// expired reports whether rec's retention ended at now.
func expired(rec domain.ActionArtifactRecord, now time.Time) bool {
	return rec.State == domain.ActionArtifactsExpired || rec.ExpiresAt != nil && !now.Before(*rec.ExpiresAt)
}

// effectiveCopy is a record's wire view at now: past its expiry it reads
// expired whether or not cleanup has removed it yet.
func effectiveCopy(rec domain.ActionArtifactRecord, now time.Time) domain.ActionArtifactCopy {
	out := rec.Copy()
	if expired(rec, now) {
		out.State = domain.ActionArtifactsExpired
	}
	return out
}

// withArtifacts attaches the execution's package and deliveries.
func (s *Service) withArtifacts(ctx context.Context, run domain.ActionRun, now time.Time) (domain.ActionRun, error) {
	recs, err := s.actions.runs.ActionArtifacts(ctx, run.ID)
	if err != nil {
		return domain.ActionRun{}, err
	}
	for _, rec := range recs {
		view := effectiveCopy(rec, now)
		if rec.Kind == domain.ActionArtifactPackage {
			run.Package = &view
		} else {
			run.Deliveries = append(run.Deliveries, view)
		}
	}
	return run, nil
}

// callerArtifacts makes a completed execution's package available on the
// caller's machine and reports it, with the path the caller can use when it
// is ready. It waits up to wait for a copy in progress.
func (s *Service) callerArtifacts(ctx context.Context, caller domain.Session, run domain.ActionRun, wait time.Duration) (domain.ActionArtifacts, string) {
	out := domain.ActionArtifacts{State: domain.ActionArtifactsNone}
	if run.Status != domain.ActionRunCompleted {
		return out, ""
	}
	rt := s.actions
	pkg, delivery, err := s.artifactRecords(ctx, run.ID, caller.Machine)
	if err != nil {
		out.State, out.Error = domain.ActionArtifactsFailed, err.Error()
		return out, ""
	}
	if pkg == nil {
		return out, ""
	}
	out.Source = &domain.ActionArtifactLocation{Machine: pkg.Machine, Path: pkg.Path}
	out.Files, out.Bytes, out.ExpiresAt = pkg.Files, pkg.Bytes, pkg.ExpiresAt
	now := time.Now().UTC()
	switch {
	case expired(*pkg, now):
		out.State = domain.ActionArtifactsExpired
		return out, ""
	case pkg.State != domain.ActionArtifactsReady:
		out.State, out.Error = domain.ActionArtifactsFailed, fmt.Sprintf("the package is %s: %s", pkg.State, pkg.Error)
		return out, ""
	case pkg.Machine == caller.Machine:
		out.State = domain.ActionArtifactsReady
		return out, pkg.Path
	case delivery != nil && delivery.State == domain.ActionArtifactsReady:
		out.State = domain.ActionArtifactsReady
		return out, delivery.Path
	}

	done := s.startDelivery(run.ID, *pkg, caller.Machine, caller.SSH)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	_, delivery, err = s.artifactRecords(ctx, run.ID, caller.Machine)
	if err != nil {
		out.State, out.Error = domain.ActionArtifactsFailed, err.Error()
		return out, ""
	}
	key := transferKey(run.ID, caller.Machine)
	rt.artifacts.mu.Lock()
	_, inFlight := rt.artifacts.transfers[key]
	failure := rt.artifacts.failures[key]
	rt.artifacts.mu.Unlock()
	switch {
	case inFlight:
		out.State = domain.ActionArtifactsCopying
	case delivery != nil && delivery.State == domain.ActionArtifactsReady:
		out.State = domain.ActionArtifactsReady
		return out, delivery.Path
	case delivery != nil && delivery.State == domain.ActionArtifactsFailed:
		out.State, out.Error = domain.ActionArtifactsFailed, delivery.Error
	case failure != "":
		out.State, out.Error = domain.ActionArtifactsFailed, failure
	default:
		out.State = domain.ActionArtifactsCopying
	}
	return out, ""
}

// artifactRecords returns an execution's package and its delivery to machine.
func (s *Service) artifactRecords(ctx context.Context, executionID, machine string) (pkg, delivery *domain.ActionArtifactRecord, err error) {
	recs, err := s.actions.runs.ActionArtifacts(ctx, executionID)
	if err != nil {
		return nil, nil, err
	}
	for i := range recs {
		switch {
		case recs[i].Kind == domain.ActionArtifactPackage:
			pkg = &recs[i]
		case recs[i].Machine == machine:
			delivery = &recs[i]
		}
	}
	return pkg, delivery, nil
}

// startDelivery starts copying pkg to the caller's machine unless a copy is
// already in flight, and returns a channel closed when that copy ends. The
// copy is detached from every requester and bounded by
// actionDeliveryTimeout; its outcome is its record.
func (s *Service) startDelivery(executionID string, pkg domain.ActionArtifactRecord, machine, alias string) <-chan struct{} {
	a := &s.actions.artifacts
	key := transferKey(executionID, machine)
	a.mu.Lock()
	if t, ok := a.transfers[key]; ok {
		a.mu.Unlock()
		return t.done
	}
	t := &deliveryTransfer{done: make(chan struct{})}
	if a.cleaningPackage(executionID) || a.cleaning[recordKey(executionID, domain.ActionArtifactDelivery, machine)] {
		if a.failures == nil {
			a.failures = map[string]string{}
		}
		a.failures[key] = "expired artifacts of this execution are being removed; try again shortly"
		a.mu.Unlock()
		close(t.done)
		return t.done
	}
	if a.transfers == nil {
		a.transfers = map[string]*deliveryTransfer{}
	}
	if a.active == nil {
		a.active = map[string]int{}
	}
	a.transfers[key] = t
	a.active[executionID]++
	delete(a.failures, key)
	a.mu.Unlock()

	go func() {
		failure := ""
		defer func() {
			a.mu.Lock()
			delete(a.transfers, key)
			if failure != "" {
				if a.failures == nil {
					a.failures = map[string]string{}
				}
				a.failures[key] = failure
			}
			a.mu.Unlock()
			a.end(executionID)
			close(t.done)
			s.publishDeliveryChange(executionID)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), actionDeliveryTimeout)
		defer cancel()
		if err := s.deliver(ctx, executionID, pkg, machine, alias); err != nil {
			failure = err.Error()
		}
	}()
	return t.done
}

// deliver copies pkg into the managed delivery directory on machine.
func (s *Service) deliver(ctx context.Context, executionID string, pkg domain.ActionArtifactRecord, machine, alias string) error {
	rt := s.actions
	limits, _ := s.actionPackageSettings()
	root, err := s.artifactsRoot(ctx, machine, alias)
	if err != nil {
		s.logger.Error("action artifact delivery failed", "execution_id", executionID, "machine", machine, "error", err)
		return err
	}
	rec := domain.ActionArtifactRecord{
		ExecutionID: executionID,
		Kind:        domain.ActionArtifactDelivery,
		Machine:     machine,
		SSH:         alias,
		Path:        joinOn(machine, root, "deliveries", executionID),
		ManagedRoot: root,
		State:       domain.ActionArtifactsCopying,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   pkg.ExpiresAt,
	}
	if machine == "" {
		rec.SSH = ""
	}
	if err := rt.runs.PutActionArtifact(ctx, rec); err != nil {
		return err
	}
	started := time.Now()
	stats, err := actionpkg.Copy(ctx, artifactLocation(pkg), artifactLocation(rec), limits)
	if err != nil {
		cause := fmt.Errorf("copying the package from %s to %s failed: %w", config.MachineLabel(pkg.Machine), config.MachineLabel(machine), err)
		s.recordFailedCopy(rec, cause)
		s.logger.Error("action artifact delivery failed", "execution_id", executionID, "from", pkg.Machine, "to", machine, "error", err)
		return cause
	}
	ready := time.Now().UTC()
	rec.State, rec.Files, rec.Bytes, rec.ReadyAt = domain.ActionArtifactsReady, stats.Files, stats.Bytes, &ready
	if err := rt.runs.PutActionArtifact(context.WithoutCancel(ctx), rec); err != nil {
		return fmt.Errorf("record delivered copy: %w", err)
	}
	s.logger.Info("action artifacts delivered", "execution_id", executionID, "from", pkg.Machine, "to", machine, "path", rec.Path,
		"files", stats.Files, "bytes", stats.Bytes, "duration", time.Since(started).String())
	return nil
}

// publishDeliveryChange announces a changed delivery so the Actions window
// rereads the execution.
func (s *Service) publishDeliveryChange(executionID string) {
	run, err := s.actions.runs.GetActionRun(context.Background(), executionID)
	if err != nil {
		s.logger.Error("read execution to announce its delivery", "execution_id", executionID, "error", err)
		return
	}
	s.publishActionEvent(run, run.Status)
}

// CleanupActionArtifacts removes interrupted copies and expired artifact
// directories. It runs once at daemon startup, in the background: machines
// are cleaned concurrently, each removal is bounded, and a machine that fails
// is not tried again in this pass, so an unreachable machine costs one short
// attempt and blocks nothing else. Failures are logged and kept on their
// records for the next startup. Executions with an artifact operation in
// flight are skipped.
func (s *Service) CleanupActionArtifacts(ctx context.Context) {
	if s.actions == nil {
		return
	}
	now := time.Now().UTC()
	recs, err := s.actions.runs.ActionArtifactsToClean(ctx, now)
	if err != nil {
		s.logger.Error("action artifact cleanup could not list its work", "error", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	byMachine := map[string][]domain.ActionArtifactRecord{}
	for _, rec := range recs {
		byMachine[rec.Machine] = append(byMachine[rec.Machine], rec)
	}
	machines := make([]string, 0, len(byMachine))
	for machine := range byMachine {
		machines = append(machines, machine)
	}
	sort.Strings(machines)
	var wg sync.WaitGroup
	for _, machine := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.cleanupMachineArtifacts(ctx, machine, byMachine[machine], now)
		}()
	}
	wg.Wait()
}

func (s *Service) cleanupMachineArtifacts(ctx context.Context, machine string, recs []domain.ActionArtifactRecord, now time.Time) {
	rt := s.actions
	var unreachable error
	removed, failed := 0, 0
	for _, stale := range recs {
		if !rt.artifacts.claimCleanup(stale) {
			continue
		}
		func() {
			defer rt.artifacts.releaseCleanup(stale)
			// Reread under the claim: a copy may have finished since the list.
			rec, ok := s.currentArtifact(ctx, stale)
			if !ok || !needsCleanup(rec, now) {
				return
			}
			if unreachable != nil {
				rec.CleanupError = "not attempted at startup: " + config.MachineLabel(machine) + " failed earlier in the same cleanup (" + unreachable.Error() + ")"
				failed++
				s.putArtifact(ctx, rec)
				return
			}
			opCtx, cancel := context.WithTimeout(ctx, actionCleanupTimeout)
			err := actionpkg.Remove(opCtx, artifactLocation(rec), rec.ManagedRoot)
			cancel()
			if err != nil {
				failed++
				rec.CleanupError = err.Error()
				if machine != "" {
					unreachable = err
				}
				s.logger.Error("action artifact cleanup failed; retried at next startup",
					"execution_id", rec.ExecutionID, "kind", rec.Kind, "machine", machine, "path", rec.Path, "error", err)
				s.putArtifact(ctx, rec)
				return
			}
			removed++
			rec.CleanupError = ""
			switch {
			case expired(rec, now):
				rec.State = domain.ActionArtifactsExpired
			case rec.State == domain.ActionArtifactsCopying:
				rec.State, rec.Error = domain.ActionArtifactsFailed, interruptedCopyError
			}
			s.putArtifact(ctx, rec)
		}()
	}
	s.logger.Info("action artifact cleanup finished", "machine", config.MachineLabel(machine), "removed", removed, "failed", failed)
}

func needsCleanup(rec domain.ActionArtifactRecord, now time.Time) bool {
	if rec.ManagedRoot == "" || rec.State == domain.ActionArtifactsExpired {
		return false
	}
	return rec.State == domain.ActionArtifactsCopying || rec.CleanupError != "" || expired(rec, now)
}

func (s *Service) currentArtifact(ctx context.Context, rec domain.ActionArtifactRecord) (domain.ActionArtifactRecord, bool) {
	recs, err := s.actions.runs.ActionArtifacts(ctx, rec.ExecutionID)
	if err != nil {
		s.logger.Error("reread action artifact for cleanup", "execution_id", rec.ExecutionID, "error", err)
		return domain.ActionArtifactRecord{}, false
	}
	for _, current := range recs {
		if current.Kind == rec.Kind && current.Machine == rec.Machine {
			return current, true
		}
	}
	return domain.ActionArtifactRecord{}, false
}

func (s *Service) putArtifact(ctx context.Context, rec domain.ActionArtifactRecord) {
	if err := s.actions.runs.PutActionArtifact(context.WithoutCancel(ctx), rec); err != nil {
		s.logger.Error("record action artifact cleanup", "execution_id", rec.ExecutionID, "kind", rec.Kind, "error", err)
	}
}
