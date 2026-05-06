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

type architectGroupsHandler struct {
	repo   domain.ArchitectGroupRepository
	logger *slog.Logger
}

type architectsHandler struct {
	repo   domain.ArchitectRepository
	logger *slog.Logger
}

type reposHandler struct {
	repo   domain.RepoRepository
	logger *slog.Logger
}

func NewHandler(profileRepo domain.ProfileRepository, groupRepo domain.ArchitectGroupRepository, architectRepo domain.ArchitectRepository, repoRepo domain.RepoRepository, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	ph := &profilesHandler{repo: profileRepo, logger: logger}
	gh := &architectGroupsHandler{repo: groupRepo, logger: logger}
	ah := &architectsHandler{repo: architectRepo, logger: logger}
	rh := &reposHandler{repo: repoRepo, logger: logger}

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/agent-profiles", ph.list)
	mux.HandleFunc("POST /api/agent-profiles", ph.create)
	mux.HandleFunc("GET /api/agent-profiles/{id}", ph.get)
	mux.HandleFunc("PUT /api/agent-profiles/{id}", ph.update)
	mux.HandleFunc("DELETE /api/agent-profiles/{id}", ph.delete)
	mux.HandleFunc("GET /api/architect-groups", gh.list)
	mux.HandleFunc("POST /api/architect-groups", gh.create)
	mux.HandleFunc("GET /api/architect-groups/{id}", gh.get)
	mux.HandleFunc("PATCH /api/architect-groups/{id}", gh.update)
	mux.HandleFunc("DELETE /api/architect-groups/{id}", gh.delete)
	mux.HandleFunc("GET /api/architects", ah.list)
	mux.HandleFunc("POST /api/architects", ah.create)
	mux.HandleFunc("GET /api/architects/{id}", ah.get)
	mux.HandleFunc("PATCH /api/architects/{id}", ah.update)
	mux.HandleFunc("DELETE /api/architects/{id}", ah.delete)
	mux.HandleFunc("GET /api/architects/{id}/repos", rh.list)
	mux.HandleFunc("POST /api/architects/{id}/repos", rh.create)
	mux.HandleFunc("GET /api/architects/{id}/repos/{repoId}", rh.get)
	mux.HandleFunc("PATCH /api/architects/{id}/repos/{repoId}", rh.update)
	mux.HandleFunc("DELETE /api/architects/{id}/repos/{repoId}", rh.delete)

	return requestID(recovery(accessLog(logger, mux)))
}
