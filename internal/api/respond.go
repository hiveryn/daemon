package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/hiveryn/daemon/internal/domain"
)

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	env := domain.Envelope{
		Data:     v,
		Logs:     []domain.LogEntry{},
		Commands: []any{},
		Meta: domain.Meta{
			RequestID: requestIDFromContext(r.Context()),
		},
	}
	writeRawJSON(w, status, env)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details any) {
	env := domain.Envelope{
		Error: &domain.ErrorBody{
			Code:       code,
			Message:    message,
			Details:    details,
			Stacktrace: string(debug.Stack()),
		},
		Logs:     []domain.LogEntry{},
		Commands: []any{},
		Meta: domain.Meta{
			RequestID: requestIDFromContext(r.Context()),
		},
	}
	writeRawJSON(w, status, env)
}

func writeRawJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
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

func mapDomainError(w http.ResponseWriter, r *http.Request, err error) bool {
	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), validationErr.Error(), map[string]string{"field": validationErr.Field})
		return true
	}
	var conflictErr *domain.ConflictError
	if errors.As(err, &conflictErr) {
		writeError(w, r, http.StatusConflict, string(domain.ErrCodeConflict), conflictErr.Error(), map[string]string{"resource": conflictErr.Resource, "field": conflictErr.Field})
		return true
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, string(domain.ErrCodeNotFound), "resource not found", nil)
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

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}
