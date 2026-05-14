package api

import (
	"net/http"
	"strconv"
	"strings"

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
		Cols        uint16 `json:"cols,omitempty"`
		Rows        uint16 `json:"rows,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	h.logger.Info("[spawn] request",
		"architect_key", key,
		"profile_name", request.ProfileName,
		"cols", request.Cols,
		"rows", request.Rows,
	)

	result, err := h.sessions.SpawnArchitectSession(r.Context(), domain.SpawnArchitectSessionRequest{
		ArchitectKey: key,
		ProfileName:  request.ProfileName,
		Cols:         request.Cols,
		Rows:         request.Rows,
	})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]string{
		"session_id": result.Session.ID,
		"ws_url":     websocketURL(r, "/ws/session/"+result.Session.ID+"/terminal/main"),
	})
}

func (h *architectsHandler) listConclusions(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	summaries, err := h.sessions.ListConclusions(r.Context(), r.PathValue("key"), limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string][]domain.ConclusionSummary{
		"conclusions": summaries,
	})
}

func (h *architectsHandler) readRecentConclusion(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	conclusion, err := h.sessions.ReadRecentConclusion(r.Context(), r.PathValue("key"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, conclusion)
}

func (h *architectsHandler) readConclusion(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "session service not configured", nil)
		return
	}

	conclusion, err := h.sessions.ReadConclusion(r.Context(), r.PathValue("key"), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, conclusion)
}
