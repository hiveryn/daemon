package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}

func decodeJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

func mapDomainError(w http.ResponseWriter, err error) bool {
	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		writeError(w, http.StatusBadRequest, "validation_error", validationErr.Error())
		return true
	}
	var conflictErr *domain.ConflictError
	if errors.As(err, &conflictErr) {
		writeError(w, http.StatusConflict, "conflict", conflictErr.Error())
		return true
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return true
	}
	return false
}

func logHandlerError(logger *slog.Logger, context, id string, err error) {
	if id != "" {
		logger.Error(context, "id", id, "error", err)
	} else {
		logger.Error(context, "error", err)
	}
}
