package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *architectsHandler) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string][]architectResponse{
		"architects": listArchitects(h.config),
	})
}

func (h *architectsHandler) get(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	architect, ok := getArchitect(h.config, key, true)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect "+key+" not found", map[string]string{
			"resource": "architect",
			"id":       key,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, architect)
}

func (h *architectsHandler) spawn(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	key := r.PathValue("key")
	var request struct {
		ProfileName string `json:"profile_name"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	result, err := h.sessions.SpawnArchitectSession(r.Context(), domain.SpawnArchitectSessionRequest{
		ArchitectKey: key,
		ProfileName:  request.ProfileName,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]string{
		"session_id": result.Session.ID,
		"ws_url":     websocketURL(r, "/ws/session/"+result.Session.ID),
	})
}
