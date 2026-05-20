package api

import "net/http"

func (h *profilesHandler) list(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string][]agentProfileResponse{
		"agent_profiles": listAgentProfiles(cfg),
	})
}

func (h *profilesHandler) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	name := r.PathValue("name")
	profile, ok := getAgentProfile(cfg, name)
	if !ok {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "agent_profile "+name+" not found", map[string]string{
			"resource": "agent_profile",
			"id":       name,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, profile)
}
