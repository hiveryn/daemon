package logging

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerWritesStructuredAppLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	manager, err := New("debug")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	manager.AppLogger().Error(
		"daemon exploded",
		"ctx", "session=abc123 ticket=xyz",
		"error", errors.New("boom"),
		"body", map[string]any{"count": 2, "ok": false},
	)

	entry := readSingleJSONL[map[string]any](t, filepath.Join(os.Getenv("HOME"), ".hiveryn", "logs", daemonLogName))
	assertRFC3339Millis(t, entry["ts"].(string))
	if entry["lvl"] != "error" {
		t.Fatalf("expected lvl=error, got %#v", entry["lvl"])
	}
	if entry["src"] != "daemon" {
		t.Fatalf("expected src=daemon, got %#v", entry["src"])
	}
	if entry["msg"] != "daemon exploded" {
		t.Fatalf("expected msg, got %#v", entry["msg"])
	}
	if entry["file"] != "logging_test.go" {
		t.Fatalf("expected file=logging_test.go, got %#v", entry["file"])
	}
	if entry["fn"] != "TestManagerWritesStructuredAppLog" {
		t.Fatalf("expected function name, got %#v", entry["fn"])
	}
	if _, ok := entry["line"].(float64); !ok {
		t.Fatalf("expected numeric line, got %#v", entry["line"])
	}
	if entry["ctx"] != "session=abc123 ticket=xyz" {
		t.Fatalf("expected ctx, got %#v", entry["ctx"])
	}
	errBody, ok := entry["err"].(map[string]any)
	if !ok {
		t.Fatalf("expected err object, got %#v", entry["err"])
	}
	if errBody["message"] != "boom" {
		t.Fatalf("expected err message, got %#v", errBody["message"])
	}
	if errBody["stack"] == "" {
		t.Fatalf("expected non-empty err stack, got %#v", errBody["stack"])
	}
	body, ok := entry["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body object, got %#v", entry["body"])
	}
	if body["count"] != float64(2) || body["ok"] != false {
		t.Fatalf("unexpected body %#v", body)
	}
}

func readSingleJSONL[T any](t *testing.T, path string) T {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal(%s): %v\n%s", path, err, string(data))
	}
	return out
}

func assertRFC3339Millis(t *testing.T, value string) {
	t.Helper()
	if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", value); err != nil {
		t.Fatalf("expected RFC3339 millis timestamp, got %q: %v", value, err)
	}
}
