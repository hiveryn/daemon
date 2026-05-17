package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/logging"
)

type ctxKey int

const ctxKeyRequestID ctxKey = iota

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = generateRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"ctx", requestLogContext(r),
					"body", map[string]any{"panic": fmt.Sprint(rec), "path": r.URL.Path, "stack": string(debug.Stack())},
				)
				writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func accessLog(log *slog.Logger, requestLogger *logging.RequestLogger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, err := captureRequestBody(r)
		if err != nil {
			log.Warn("failed to capture request body", "ctx", requestLogContext(r), "error", err)
		}

		start := time.Now()
		ww := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(ww, r)
		status := ww.status
		if status == 0 {
			status = http.StatusOK
		}

		entry := logging.RequestEntry{
			Timestamp:  loggingTimestamp(start),
			Method:     r.Method,
			Path:       r.URL.Path,
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Context:    requestLogContext(r),
		}
		if parsed := parseJSONBody(requestBody); parsed != nil {
			entry.RequestBody = parsed
		}
		if envelope, ok := parseResponseEnvelope(log, r, ww); ok {
			entry.Response = envelope
		}
		if requestLogger != nil {
			requestLogger.Log(entry)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	buf         bytes.Buffer
	status      int
	wroteHeader bool
	contentType string
	truncated   bool
}

const maxLoggedResponseBytes = 1 << 20

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.contentType = sw.Header().Get("Content-Type")
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(p []byte) (int, error) {
	if !sw.wroteHeader {
		sw.WriteHeader(http.StatusOK)
	}
	if sw.shouldCaptureBody() && !sw.truncated {
		remaining := maxLoggedResponseBytes - sw.buf.Len()
		if remaining > 0 {
			if len(p) > remaining {
				_, _ = sw.buf.Write(p[:remaining])
				sw.truncated = true
			} else {
				_, _ = sw.buf.Write(p)
			}
		} else {
			sw.truncated = true
		}
	}
	return sw.ResponseWriter.Write(p)
}

func (sw *statusWriter) Flush() {
	if flusher, ok := sw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := sw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

func generateRequestID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (sw *statusWriter) shouldCaptureBody() bool {
	return strings.HasPrefix(strings.ToLower(sw.contentType), "application/json")
}

func captureRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	if len(data) == 0 {
		return nil, err
	}
	return data, err
}

func parseJSONBody(data []byte) any {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil
	}
	return body
}

func parseResponseEnvelope(log *slog.Logger, r *http.Request, sw *statusWriter) (any, bool) {
	if sw.status == http.StatusNoContent || !sw.shouldCaptureBody() || sw.buf.Len() == 0 {
		return nil, false
	}
	var envelope any
	if err := json.Unmarshal(sw.buf.Bytes(), &envelope); err != nil {
		log.Warn("failed to decode response envelope for request log", "ctx", requestLogContext(r), "error", err)
		return nil, false
	}
	return envelope, true
}

func requestLogContext(r *http.Request) string {
	parts := []string{"request_id=" + requestIDFromContext(r.Context())}
	if key := r.PathValue("key"); key != "" {
		parts = append(parts, "architect_key="+key)
	}
	if repoKey := r.PathValue("repoKey"); repoKey != "" {
		parts = append(parts, "repo_key="+repoKey)
	}
	if terminalID := r.PathValue("uuid"); terminalID != "" {
		parts = append(parts, "terminal_id="+terminalID)
	}
	if id := r.PathValue("id"); id != "" {
		switch {
		case strings.Contains(r.URL.Path, "/tickets/"):
			parts = append(parts, "ticket_id="+id)
		case strings.Contains(r.URL.Path, "/sessions/") || strings.Contains(r.URL.Path, "/ws/session/"):
			parts = append(parts, "session_id="+id)
		default:
			parts = append(parts, "id="+id)
		}
	}
	return strings.Join(parts, " ")
}

func loggingTimestamp(ts time.Time) string {
	return ts.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
