package api

import (
	"net/http"

	"github.com/hiveryn/daemon/internal/config"
)

type desktopConfigHandler struct {
	config config.Config
}

type desktopConfigResponse struct {
	HealthPollIntervalMS int64 `json:"health_poll_interval_ms"`
}

func (h *desktopConfigHandler) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, desktopConfigResponse{
		HealthPollIntervalMS: h.config.DesktopHealthPollIntervalDuration().Milliseconds(),
	})
}
