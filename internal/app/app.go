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
	"strconv"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/archevents"
	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/logging"
	"github.com/hiveryn/daemon/internal/server"
	"github.com/hiveryn/daemon/internal/sessionruntime"
	"github.com/hiveryn/daemon/internal/store"
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

	configSource, err := config.NewArchitectsReloadingSource(runtime.ConfigPath, cfg)
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
	ticketService := architectfs.NewTicketService()
	service, err := sessionruntime.New(ctx, cfg, configSource, sessionStore, ticketService, logger, resolvedBaseURL)
	if err != nil {
		return err
	}
	if err := service.RestoreRunningSessions(ctx); err != nil {
		return err
	}
	architectHub := archevents.New()

	handler := api.NewHandler(api.Dependencies{
		Config:          cfg,
		ConfigSource:    configSource,
		Runtime:         runtime,
		BaseURL:         resolvedBaseURL,
		Logger:          logger,
		RequestLogger:   logManager.RequestLogger(),
		Sessions:        service,
		Tickets:         ticketService,
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
