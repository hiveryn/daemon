package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/api"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/server"
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

	profileRepo := store.NewProfileStore(db)
	groupRepo := store.NewArchitectGroupStore(db)
	architectRepo := store.NewArchitectStore(db)
	repoRepo := store.NewRepoStore(db)
	handler := api.NewHandler(profileRepo, groupRepo, architectRepo, repoRepo, logger)

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
