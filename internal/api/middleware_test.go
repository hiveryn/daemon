package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/logging"
)

func TestAccessLogWritesStructuredRequestEntry(t *testing.T) {
	t.Setenv("HIVERYN_HOME", filepath.Join(t.TempDir(), ".hiveryn"))
	manager := newRequestLogManager(t)

	handler := requestID(accessLog(discardLogger(), manager.RequestLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusCreated, map[string]any{"ok": true})
	})))

	status, _ := requestJSON(t, handler, http.MethodPost, "/api/sessions", map[string]any{"name": "demo"})
	if status != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, status)
	}

	entry := readRequestLogEntry(t)
	assertRequestTimestamp(t, entry["ts"].(string))
	if entry["method"] != http.MethodPost {
		t.Fatalf("expected POST, got %#v", entry["method"])
	}
	if entry["path"] != "/api/sessions" {
		t.Fatalf("expected path, got %#v", entry["path"])
	}
	if entry["status"] != float64(http.StatusCreated) {
		t.Fatalf("expected status 201, got %#v", entry["status"])
	}
	if _, ok := entry["duration_ms"].(float64); !ok {
		t.Fatalf("expected duration_ms number, got %#v", entry["duration_ms"])
	}
	ctx, ok := entry["ctx"].(string)
	if !ok || ctx == "" {
		t.Fatalf("expected ctx string, got %#v", entry["ctx"])
	}
	reqBody, ok := entry["req_body"].(map[string]any)
	if !ok || reqBody["name"] != "demo" {
		t.Fatalf("unexpected req_body %#v", entry["req_body"])
	}
	envelope, ok := entry["res_envelope"].(map[string]any)
	if !ok {
		t.Fatalf("expected response envelope, got %#v", entry["res_envelope"])
	}
	meta, ok := envelope["meta"].(map[string]any)
	if !ok || meta["request_id"] == "" {
		t.Fatalf("expected meta.request_id, got %#v", envelope["meta"])
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok || data["ok"] != true {
		t.Fatalf("unexpected response data %#v", envelope["data"])
	}
}

func TestAccessLogCapturesRecoveredPanics(t *testing.T) {
	t.Setenv("HIVERYN_HOME", filepath.Join(t.TempDir(), ".hiveryn"))
	manager := newRequestLogManager(t)

	handler := requestID(accessLog(discardLogger(), manager.RequestLogger(), recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))

	status, body := request(t, handler, http.MethodGet, "/api/panic", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, status, string(body))
	}

	entry := readRequestLogEntry(t)
	if entry["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("expected logged 500, got %#v", entry["status"])
	}
	envelope, ok := entry["res_envelope"].(map[string]any)
	if !ok {
		t.Fatalf("expected response envelope, got %#v", entry["res_envelope"])
	}
	errBody, ok := envelope["error"].(map[string]any)
	if !ok || errBody["code"] != "INTERNAL" {
		t.Fatalf("expected INTERNAL error envelope, got %#v", envelope["error"])
	}
}

func TestAccessLogOmitsEnvelopeForSSE(t *testing.T) {
	t.Setenv("HIVERYN_HOME", filepath.Join(t.TempDir(), ".hiveryn"))
	manager := newRequestLogManager(t)

	handler := requestID(accessLog(discardLogger(), manager.RequestLogger(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: hello\n\n")
	})))

	status, _ := request(t, handler, http.MethodGet, "/api/events", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, status)
	}

	entry := readRequestLogEntry(t)
	if _, ok := entry["res_envelope"]; ok {
		t.Fatalf("expected SSE log to omit res_envelope, got %#v", entry["res_envelope"])
	}
}

func newRequestLogManager(t *testing.T) *logging.Manager {
	t.Helper()

	manager, err := logging.New("debug")
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("manager.Close: %v", err)
		}
	})
	return manager
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readRequestLogEntry(t *testing.T) map[string]any {
	t.Helper()

	runtime, err := config.ResolveRuntime("", "")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(runtime.LogDir, "requests.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(requests.jsonl): %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("Unmarshal request log: %v\n%s", err, string(data))
	}
	return entry
}

func assertRequestTimestamp(t *testing.T, value string) {
	t.Helper()
	if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", value); err != nil {
		t.Fatalf("expected RFC3339 millis timestamp, got %q: %v", value, err)
	}
}
