package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *profilesHandler) list(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.repo.List(r.Context())
	if err != nil {
		logHandlerError(h.logger, "list agent profiles failed", "", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list agent profiles")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]domain.AgentProfile{"agent_profiles": profiles})
}

func (h *profilesHandler) create(w http.ResponseWriter, r *http.Request) {
	var input domain.AgentProfileInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	profile, err := h.repo.Create(r.Context(), input)
	if err != nil {
		if mapDomainError(w, err) {
			return
		}
		logHandlerError(h.logger, "create agent profile failed", "", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create agent profile")
		return
	}
	writeJSON(w, http.StatusCreated, profile)
}

func (h *profilesHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	profile, err := h.repo.Get(r.Context(), id)
	if err != nil {
		if mapDomainError(w, err) {
			return
		}
		logHandlerError(h.logger, "get agent profile failed", id, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load agent profile")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (h *profilesHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input domain.AgentProfileInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	profile, err := h.repo.Update(r.Context(), id, input)
	if err != nil {
		if mapDomainError(w, err) {
			return
		}
		logHandlerError(h.logger, "update agent profile failed", id, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to update agent profile")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (h *profilesHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.repo.Delete(r.Context(), id); err != nil {
		if mapDomainError(w, err) {
			return
		}
		logHandlerError(h.logger, "delete agent profile failed", id, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete agent profile")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
