package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/archevents"
	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/archive"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/logging"
	"github.com/hiveryn/daemon/internal/server"
	"github.com/hiveryn/daemon/internal/sessionruntime"
	"github.com/hiveryn/daemon/internal/store"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

func Run(configPath, databasePath string, portOverride int) error {
	runtime, err := config.ResolveRuntime(configPath, databasePath)
	if err != nil {
		return err
	}

	logManager, err := logging.NewWithDir(config.DefaultLogLevel, runtime.LogDir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := logManager.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: failed to close log files: %v\n", closeErr)
		}
	}()

	logger := logManager.AppLogger()
	slog.SetDefault(logger)

	// Actions are registered in actions.yaml, no longer discovered: the first
	// start without a registry registers every checkout found in the old
	// library directory, so no existing Action disappears.
	registered, err := config.BootstrapActionRegistry(filepath.Dir(runtime.ConfigPath), runtime.ActionsDir)
	if err != nil {
		return fmt.Errorf("bootstrap the Action registry: %w", err)
	}
	if len(registered) > 0 {
		logger.Info("created the Action registry from the existing Actions library", "registry", runtime.ActionsRegistry, "library", runtime.ActionsDir, "actions", registered)
	}

	cfg, err := config.Load(runtime.ConfigPath)
	if err != nil {
		return err
	}

	if portOverride != 0 {
		cfg.Port = portOverride
	}

	if err := logManager.SetLevel(cfg.LogLevel); err != nil {
		return err
	}

	configSource, err := config.NewReloadingSource(runtime.ConfigPath, cfg)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, runtime.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error("failed to close database", "error", closeErr)
		}
	}()

	resolvedBaseURL := baseURL(cfg)

	sessionStore := store.NewSessionStore(db)
	ticketService := architectfs.NewTicketService(architectfs.ConfigLocator(configSource))
	workspaceService := workspacefs.NewService(ticketService)
	service, err := sessionruntime.New(ctx, cfg, configSource, sessionStore, ticketService, logger, resolvedBaseURL)
	if err != nil {
		return err
	}
	if cfg.ArchiveAgentEvents {
		archiver, err := archive.New(runtime.Home, logger)
		if err != nil {
			return fmt.Errorf("create event archiver: %w", err)
		}
		service.SetEventArchiver(archiver)
		logger.Info("agent event archival enabled", "dir", runtime.Home)
	}
	service.SetDeferredIntentRepository(store.NewDeferredIntentStore(db))
	service.SetAgentMessageRepository(store.NewAgentMessageStore(db))
	service.SetActions(store.NewActionRunStore(db), sessionruntime.ActionPaths{
		Registry:  runtime.ActionsRegistry,
		Runs:      runtime.ActionRunsDir,
		Artifacts: runtime.ActionArtifactsDir,
	})
	// Before restore: every question still pending in the log belongs to an
	// agent call that died with the previous daemon.
	if err := service.ReconcileQuestions(ctx); err != nil {
		return fmt.Errorf("reconcile questions: %w", err)
	}
	if err := service.RestoreRunningSessions(ctx); err != nil {
		return err
	}
	// After restore: an execution whose agent session did not come back is
	// failed as interrupted; restored executions keep running.
	if err := service.ReconcileActionRuns(ctx); err != nil {
		return err
	}
	if err := service.ReconcileIntents(ctx); err != nil {
		return err
	}
	// After restore: pending_commit notices are settled against their
	// tickets and the dispatcher starts delivering to the restored sessions.
	if err := service.StartMessageDelivery(ctx); err != nil {
		return fmt.Errorf("start agent message delivery: %w", err)
	}
	// Interrupted copies and expired Action artifacts are removed once per
	// start, in the background: an unreachable machine must not hold up the
	// daemon, and failures stay recorded for the next start.
	go service.CleanupActionArtifacts(context.WithoutCancel(ctx))
	architectHub := archevents.New(logger)
	// Intent side effects (ticket created/concluded) publish workspace_changed
	// from inside the service, because a generic approve path cannot know which
	// tool touched a ticket.
	service.SetArchitectPublisher(architectHub.Publish)
	// A remote reference answer that arrives after a board read stopped
	// waiting for it reaches clients as a ticket update, so they read again.
	ticketService.SetReferencesChanged(func(architectPath string) {
		current, err := configSource.Current()
		if err != nil {
			logger.Error("cannot announce resolved ticket references", "architect_path", architectPath, "error", err)
			return
		}
		for key, architect := range current.Architects {
			if filepath.Clean(architect.Path) != filepath.Clean(architectPath) {
				continue
			}
			architectHub.Publish(key, domain.ArchitectEvent{
				Type:         domain.ArchitectEventType,
				ArchitectKey: key,
				Reason:       domain.ArchitectEventTicketUpdated,
				At:           time.Now().UTC(),
			})
		}
	})

	handler := api.NewHandler(api.Dependencies{
		Config:          cfg,
		ConfigSource:    configSource,
		Runtime:         runtime,
		BaseURL:         resolvedBaseURL,
		Logger:          logger,
		RequestLogger:   logManager.RequestLogger(),
		Sessions:        service,
		Tickets:         ticketService,
		Workspaces:      workspaceService,
		Actions:         service,
		Questions:       service,
		IngestHandler:   service.IngestHandler(),
		ArchitectEvents: architectHub,
	})

	srv := server.New(cfg.BindAddress, cfg.Port, handler)
	logger.Info("daemon listening", "addr", srv.Addr())

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-sigCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		logger.Error("runtime shutdown failed", "error", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("daemon stopped")
	return nil
}

func baseURL(cfg config.Config) string {
	return "http://" + net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Port))
}
