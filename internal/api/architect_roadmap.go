package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

type architectRoadmapHandler struct {
	config       config.Config
	configSource config.Source
	logger       *slog.Logger
	roadmaps     domain.RoadmapService
}

// resolveArchitectWorkspace resolves the workspace path for the {key} path
// param from fresh config, writing a 404 and returning ok=false when the
// architect is unknown. Shared by the config and roadmap handlers.
func resolveArchitectWorkspace(w http.ResponseWriter, r *http.Request, cfg config.Config, source config.Source) (key, workspace string, ok bool) {
	current, err := currentConfig(cfg, source)
	if err != nil {
		writeDomainError(w, r, err)
		return "", "", false
	}
	key = r.PathValue("key")
	architect, exists := current.Architects[key]
	if !exists {
		writeArchitectNotFound(w, r, key)
		return "", "", false
	}
	return key, architect.Path, true
}

func (h *architectRoadmapHandler) read(w http.ResponseWriter, r *http.Request) {
	_, workspace, ok := resolveArchitectWorkspace(w, r, h.config, h.configSource)
	if !ok {
		return
	}
	query := domain.RoadmapQuery{
		View: r.URL.Query().Get("view"),
		ID:   r.URL.Query().Get("id"),
	}
	if raw := r.URL.Query().Get("depth"); raw != "" {
		depth, err := strconv.Atoi(raw)
		if err != nil {
			writeDomainError(w, r, &domain.ValidationError{Field: "depth", Message: "must be an integer"})
			return
		}
		query.Depth = &depth
	}
	view, err := h.roadmaps.Read(r.Context(), workspace, query)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, view)
}

func (h *architectRoadmapHandler) update(w http.ResponseWriter, r *http.Request) {
	_, workspace, ok := resolveArchitectWorkspace(w, r, h.config, h.configSource)
	if !ok {
		return
	}
	var params domain.UpdateRoadmapParams
	if err := decodeJSON(r, &params); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), nil)
		return
	}
	result, err := h.roadmaps.Update(r.Context(), workspace, params)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, result)
}
