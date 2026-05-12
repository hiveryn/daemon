package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/server"
	"github.com/hiveryn/daemon/internal/sessionruntime"
	"github.com/hiveryn/daemon/internal/store"
)

func Run(configPath, databasePath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger, err := newLogger(cfg.LogLevel)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error("failed to close database", "error", closeErr)
		}
	}()

	sessionStore := store.NewSessionStore(db)
	service, err := sessionruntime.New(ctx, cfg, sessionStore, logger, baseURL(cfg))
	if err != nil {
		return err
	}
	if err := service.FailRunningSessions(ctx); err != nil {
		return err
	}
	ticketService := architectfs.NewTicketService()

	handler := api.NewHandler(api.Dependencies{
		Config:        cfg,
		Logger:        logger,
		Sessions:      service,
		Tickets:       ticketService,
		IngestHandler: service.IngestHandler(),
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

func newLogger(level string) (*slog.Logger, error) {
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		return nil, err
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel})
	return slog.New(handler), nil
}

func baseURL(cfg config.Config) string {
	return "http://" + net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Port))
}
