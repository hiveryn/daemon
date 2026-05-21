package api

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/hiveryn/daemon/internal/config"
)

func handleSystemHome(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Error("resolve home directory", "error", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "failed to resolve home directory", nil)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"home": home})
}

type systemRuntimeHandler struct {
	runtime     config.Runtime
	bindAddress string
	port        int
	baseURL     string
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
}

func (h *systemRuntimeHandler) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, systemRuntimeResponse{
		Environment: h.runtime.Environment,
		Home:        h.runtime.Home,
		ConfigPath:  h.runtime.ConfigPath,
		DBPath:      h.runtime.DBPath,
		LogDir:      h.runtime.LogDir,
		BindAddress: h.bindAddress,
		Port:        h.port,
		BaseURL:     h.baseURL,
	})
}
