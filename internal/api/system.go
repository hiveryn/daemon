package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/config"
)

type systemRuntimeHandler struct {
	runtime      config.Runtime
	bindAddress  string
	port         int
	baseURL      string
	configSource config.Source
}

type systemRuntimeResponse struct {
	Environment string `json:"environment"`
	Home        string `json:"home"`
	ConfigPath  string `json:"config_path"`
	DBPath      string `json:"db_path"`
	LogDir      string `json:"log_dir"`
	BindAddress string `json:"bind_address"`
	Port        int    `json:"port"`
	BaseURL     string `json:"base_url"`
	// Config reports the most recent reload of the YAML configuration. A
	// non-empty error means an on-disk edit is broken and the daemon is serving
	// the last valid config instead — visible here rather than silently masked.
	Config config.LoadStatus `json:"config"`
}

func (h *systemRuntimeHandler) get(w http.ResponseWriter, r *http.Request) {
	var status config.LoadStatus
	if h.configSource != nil {
		status = h.configSource.LoadStatus()
	}
	writeJSON(w, r, http.StatusOK, systemRuntimeResponse{
		Environment: h.runtime.Environment,
		Home:        h.runtime.Home,
		ConfigPath:  h.runtime.ConfigPath,
		DBPath:      h.runtime.DBPath,
		LogDir:      h.runtime.LogDir,
		BindAddress: h.bindAddress,
		Port:        h.port,
		BaseURL:     h.baseURL,
		Config:      status,
	})
}
