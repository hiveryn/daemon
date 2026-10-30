package api

import (
	"log/slog"
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

// Companions. Agent-facing routes are addressed by the calling session, which
// the daemon resolves and authorizes (a companion's tools: messages, replies,
// requestAction, shutdown completion; an architect's: messages to its
// companions, replies and shutdown requests). requestAction BLOCKS until the
// request resolves (at most domain.ActionRequestTimeout). The listing and the
// action decision are the desktop's.
type companionsHandler struct {
	logger     *slog.Logger
	companions domain.CompanionService
}

func (h *companionsHandler) ready(w http.ResponseWriter, r *http.Request) bool {
	if h.companions == nil {
		writeError(w, r, http.StatusNotImplemented, "NOT_IMPLEMENTED", "companion service not configured", nil)
		return false
	}
	return true
}

func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var input T
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "invalid request body: "+err.Error(), nil)
		return input, false
	}
	return input, true
}

func respond[T any](w http.ResponseWriter, r *http.Request, value T, err error) {
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, value)
}

func (h *companionsHandler) list(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	list, err := h.companions.ListArchitectCompanions(r.Context(), r.PathValue("key"))
	respond(w, r, list, err)
}

func (h *companionsHandler) sendToArchitect(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.CompanionMessageRequest](w, r); ok {
		res, err := h.companions.SendMessageToArchitect(r.Context(), r.PathValue("id"), input)
		respond(w, r, res, err)
	}
}

func (h *companionsHandler) replyToArchitect(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.CompanionReplyRequest](w, r); ok {
		res, err := h.companions.ReplyToArchitectMessage(r.Context(), r.PathValue("id"), input)
		respond(w, r, res, err)
	}
}

func (h *companionsHandler) sendToCompanion(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.CompanionMessageRequest](w, r); ok {
		res, err := h.companions.SendMessageToCompanion(r.Context(), r.PathValue("id"), r.PathValue("name"), input)
		respond(w, r, res, err)
	}
}

func (h *companionsHandler) replyToCompanion(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.CompanionReplyRequest](w, r); ok {
		res, err := h.companions.ReplyToCompanionMessage(r.Context(), r.PathValue("id"), input)
		respond(w, r, res, err)
	}
}

func (h *companionsHandler) requestShutdown(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.CompanionShutdownRequest](w, r); ok {
		res, err := h.companions.RequestCompanionShutdown(r.Context(), r.PathValue("id"), input)
		respond(w, r, res, err)
	}
}

func (h *companionsHandler) completeShutdown(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.CompleteCompanionShutdownRequest](w, r); ok {
		res, err := h.companions.CompleteCompanionShutdown(r.Context(), r.PathValue("id"), input)
		respond(w, r, res, err)
		// The companion is stopped only once this acknowledgement is out.
		if err == nil && http.NewResponseController(w).Flush() == nil {
			h.companions.CompanionShutdownAcknowledged(r.PathValue("id"))
		}
	}
}

func (h *companionsHandler) requestAction(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	input, ok := decodeBody[domain.RequestActionRequest](w, r)
	if !ok {
		return
	}
	res, err := h.companions.RequestAction(r.Context(), r.PathValue("id"), input)
	if err != nil && r.Context().Err() != nil {
		// The caller is gone; the request was resolved as cancelled.
		if h.logger != nil {
			h.logger.Info("action request ended by caller", "session_id", r.PathValue("id"), "error", err)
		}
		return
	}
	respond(w, r, res, err)
}

func (h *companionsHandler) listActions(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	actions, err := h.companions.ListActions(r.Context(), r.PathValue("id"))
	respond(w, r, map[string][]domain.ActionRequest{"actions": actions}, err)
}

func (h *companionsHandler) decideAction(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	if input, ok := decodeBody[domain.ActionDecisionRequest](w, r); ok {
		res, err := h.companions.DecideAction(r.Context(), r.PathValue("id"), r.PathValue("actionID"), input)
		respond(w, r, res, err)
	}
}
