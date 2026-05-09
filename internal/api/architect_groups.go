package api

import "net/http"

func (h *architectGroupsHandler) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string][]architectGroupResponse{
		"architect_groups": listArchitectGroups(h.config),
	})
}

func (h *architectGroupsHandler) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	group, ok := getArchitectGroup(h.config, name)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect_group "+name+" not found", map[string]string{
			"resource": "architect_group",
			"id":       name,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, group)
}
