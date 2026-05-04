package api

import (
	"log/slog"
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

type profilesHandler struct {
	repo   domain.ProfileRepository
	logger *slog.Logger
}

func NewHandler(repo domain.ProfileRepository, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	ph := &profilesHandler{repo: repo, logger: logger}

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/agent-profiles", ph.list)
	mux.HandleFunc("POST /api/agent-profiles", ph.create)
	mux.HandleFunc("GET /api/agent-profiles/{id}", ph.get)
	mux.HandleFunc("PUT /api/agent-profiles/{id}", ph.update)
	mux.HandleFunc("DELETE /api/agent-profiles/{id}", ph.delete)

	return requestID(recovery(accessLog(logger, mux)))
}
