package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

type actionsHandler struct {
	logger  *slog.Logger
	actions domain.ActionService
}

func (h *actionsHandler) ready(w http.ResponseWriter, r *http.Request) bool {
	if h.actions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "actions runtime not configured", nil)
		return false
	}
	return true
}

func (h *actionsHandler) list(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	list, err := h.actions.ListActions(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, list)
}

func (h *actionsHandler) get(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	def, err := h.actions.GetAction(r.Context(), r.PathValue("name"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, def)
}

// launch is the user's manual launch. It needs no approval intent: the user
// starting the execution is the approval.
func (h *actionsHandler) launch(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	var input domain.LaunchActionRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}
	result, err := h.actions.LaunchAction(r.Context(), r.PathValue("name"), input)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, result)
}

func (h *actionsHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "limit must be an integer between 1 and 500", map[string]string{"field": "limit"})
			return
		}
		limit = n
	}
	runs, err := h.actions.ListActionRuns(r.Context(), r.URL.Query().Get("action"), limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]domain.ActionRun{"runs": runs})
}

func (h *actionsHandler) getRun(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	run, err := h.actions.GetActionRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, run)
}

func (h *actionsHandler) cancelRun(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	run, err := h.actions.CancelActionRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, run)
}

// conclude is the action agent's concludeSession, addressed by its session.
func (h *actionsHandler) conclude(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	var input domain.ConcludeActionRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return
	}
	run, err := h.actions.ConcludeAction(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, run)
}

func (h *actionsHandler) recentConclusions(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	conclusions, err := h.actions.RecentActionConclusions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]domain.ActionConclusion{"conclusions": conclusions})
}

// events streams execution status changes. There is no backlog: a client
// refetches executions when it (re)connects.
func (h *actionsHandler) events(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "streaming not supported", nil)
		return
	}

	sub := h.actions.SubscribeActionEvents()
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if _, err := fmt.Fprint(w, ":\n\n"); err != nil {
		return
	}
	flusher.Flush()

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-sub.C():
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				h.logger.Error("[sse] marshal action event error", "error", err)
				return
			}
			if err := writeSSEEventData(w, data); err != nil {
				h.logger.Warn("[sse] action event write error", "error", err)
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
