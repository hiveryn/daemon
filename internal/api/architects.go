package api

import "net/http"

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
