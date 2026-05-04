package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *profilesHandler) list(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.repo.List(r.Context())
	if err != nil {
		logHandlerError(h.logger, "list agent profiles failed", "", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to list agent profiles", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]domain.AgentProfile{"agent_profiles": profiles})
}

func (h *profilesHandler) create(w http.ResponseWriter, r *http.Request) {
	var input domain.AgentProfileInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	profile, err := h.repo.Create(r.Context(), input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "create agent profile failed", "", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to create agent profile", nil)
		return
	}
	writeJSON(w, r, http.StatusCreated, profile)
}

func (h *profilesHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	profile, err := h.repo.Get(r.Context(), id)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "get agent profile failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to load agent profile", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, profile)
}

func (h *profilesHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input domain.AgentProfileInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	profile, err := h.repo.Update(r.Context(), id, input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "update agent profile failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to update agent profile", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, profile)
}

func (h *profilesHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.repo.Delete(r.Context(), id); err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "delete agent profile failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to delete agent profile", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
