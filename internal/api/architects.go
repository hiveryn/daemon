package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *architectsHandler) list(w http.ResponseWriter, r *http.Request) {
	architects, err := h.repo.List(r.Context())
	if err != nil {
		logHandlerError(h.logger, "list architects failed", "", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to list architects", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]domain.Architect{"architects": architects})
}

func (h *architectsHandler) create(w http.ResponseWriter, r *http.Request) {
	var input domain.ArchitectInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	architect, err := h.repo.Create(r.Context(), input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "create architect failed", "", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to create architect", nil)
		return
	}
	writeJSON(w, r, http.StatusCreated, architect)
}

func (h *architectsHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	architect, err := h.repo.Get(r.Context(), id)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "get architect failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to load architect", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, architect)
}

func (h *architectsHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input domain.ArchitectUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	architect, err := h.repo.Update(r.Context(), id, input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "update architect failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to update architect", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, architect)
}

func (h *architectsHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.repo.Delete(r.Context(), id); err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "delete architect failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to delete architect", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
