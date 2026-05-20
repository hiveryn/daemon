package api

import "net/http"

func (h *architectGroupsHandler) list(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]architectGroupResponse{
		"architect_groups": listArchitectGroups(cfg),
	})
}

func (h *architectGroupsHandler) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	name := r.PathValue("name")
	group, ok := getArchitectGroup(cfg, name)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "architect_group "+name+" not found", map[string]string{
			"resource": "architect_group",
			"id":       name,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, group)
}
