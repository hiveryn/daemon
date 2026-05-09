package api

import "net/http"

func (h *profilesHandler) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string][]agentProfileResponse{
		"agent_profiles": listAgentProfiles(h.config),
	})
}

func (h *profilesHandler) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	profile, ok := getAgentProfile(h.config, name)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "agent_profile "+name+" not found", map[string]string{
			"resource": "agent_profile",
			"id":       name,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, profile)
}
