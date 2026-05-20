package api

import "net/http"

func (h *shortcutsHandler) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := currentConfig(h.config, h.configSource)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, cloneShortcuts(cfg.Shortcuts))
}

func cloneShortcuts(src map[string]map[string]string) map[string]map[string]string {
	if len(src) == 0 {
		return map[string]map[string]string{}
	}
	dst := make(map[string]map[string]string, len(src))
	for section, bindings := range src {
		clone := make(map[string]string, len(bindings))
		for action, key := range bindings {
			clone[action] = key
		}
		dst[section] = clone
	}
	return dst
}
