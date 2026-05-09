package api

import "net/http"

func (h *reposHandler) list(w http.ResponseWriter, r *http.Request) {
	architectKey := r.PathValue("key")
	repos, ok := listRepos(h.config, architectKey)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect "+architectKey+" not found", map[string]string{
			"resource": "architect",
			"id":       architectKey,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]repoResponse{"repos": repos})
}

func (h *reposHandler) get(w http.ResponseWriter, r *http.Request) {
	architectKey := r.PathValue("key")
	repoKey := r.PathValue("repoKey")
	repo, architectExists, repoExists := getRepo(h.config, architectKey, repoKey)
	if !architectExists {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect "+architectKey+" not found", map[string]string{
			"resource": "architect",
			"id":       architectKey,
		})
		return
	}
	if !repoExists {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "repo "+repoKey+" not found", map[string]string{
			"resource": "repo",
			"id":       repoKey,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, repo)
}
