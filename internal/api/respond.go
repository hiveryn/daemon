package api

import (
	"context"
	"encoding/json"
	"fmt"
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

func writeSSEEventData(w http.ResponseWriter, data []byte) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}
