package api

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestSystemHome(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/system/home", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var env domain.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}

	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data to be map, got %T", env.Data)
	}

	home, ok := data["home"].(string)
	if !ok {
		t.Fatalf("expected home to be string, got %T", data["home"])
	}
	if home == "" {
		t.Fatal("expected non-empty home directory")
	}

	expectedHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir failed: %v", err)
	}
	if home != expectedHome {
		t.Fatalf("expected home %q, got %q", expectedHome, home)
	}
}
