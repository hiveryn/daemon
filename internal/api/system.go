package api

import (
	"log/slog"
	"net/http"
	"os"
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
