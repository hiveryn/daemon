package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *architectGroupsHandler) list(w http.ResponseWriter, r *http.Request) {
	groups, err := h.repo.List(r.Context())
	if err != nil {
		logHandlerError(h.logger, "list architect groups failed", "", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to list architect groups", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]domain.ArchitectGroup{"architect_groups": groups})
}

func (h *architectGroupsHandler) create(w http.ResponseWriter, r *http.Request) {
	var input domain.ArchitectGroupInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	group, err := h.repo.Create(r.Context(), input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "create architect group failed", "", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to create architect group", nil)
		return
	}
	writeJSON(w, r, http.StatusCreated, group)
}

func (h *architectGroupsHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	group, err := h.repo.Get(r.Context(), id)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "get architect group failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to load architect group", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, group)
}

func (h *architectGroupsHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input domain.ArchitectGroupUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	group, err := h.repo.Update(r.Context(), id, input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "update architect group failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to update architect group", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, group)
}

func (h *architectGroupsHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.repo.Delete(r.Context(), id); err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "delete architect group failed", id, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to delete architect group", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
