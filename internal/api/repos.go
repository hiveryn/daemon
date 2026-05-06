package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

func (h *reposHandler) list(w http.ResponseWriter, r *http.Request) {
	architectID := r.PathValue("id")
	repos, err := h.repo.List(r.Context(), architectID)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "list repos failed", architectID, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to list repos", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]domain.Repo{"repos": repos})
}

func (h *reposHandler) create(w http.ResponseWriter, r *http.Request) {
	architectID := r.PathValue("id")
	var input domain.RepoInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	repo, err := h.repo.Create(r.Context(), architectID, input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "create repo failed", architectID, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to create repo", nil)
		return
	}
	writeJSON(w, r, http.StatusCreated, repo)
}

func (h *reposHandler) get(w http.ResponseWriter, r *http.Request) {
	architectID := r.PathValue("id")
	repoID := r.PathValue("repoId")
	repo, err := h.repo.Get(r.Context(), architectID, repoID)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "get repo failed", repoID, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to load repo", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, repo)
}

func (h *reposHandler) update(w http.ResponseWriter, r *http.Request) {
	architectID := r.PathValue("id")
	repoID := r.PathValue("repoId")
	var input domain.RepoUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error(), nil)
		return
	}

	repo, err := h.repo.Update(r.Context(), architectID, repoID, input)
	if err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "update repo failed", repoID, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to update repo", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, repo)
}

func (h *reposHandler) delete(w http.ResponseWriter, r *http.Request) {
	architectID := r.PathValue("id")
	repoID := r.PathValue("repoId")
	if err := h.repo.Delete(r.Context(), architectID, repoID); err != nil {
		if mapDomainError(w, r, err) {
			return
		}
		logHandlerError(h.logger, "delete repo failed", repoID, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to delete repo", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
