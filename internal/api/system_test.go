package api

import (
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
)

func TestSystemRuntime(t *testing.T) {
	t.Setenv("HIVERYN_HOME", filepath.Join(t.TempDir(), "runtime-dev"))
	t.Setenv("HIVERYN_ENV", "development")

	runtime, err := config.ResolveRuntime("", "")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(Dependencies{
		Config: config.Config{
			Port:                      4202,
			BindAddress:               "127.0.0.1",
			LogLevel:                  "debug",
			DesktopHealthPollInterval: "1s",
			Variants:                  map[string]config.VariantConfig{},
			Architects:                map[string]config.ArchitectConfig{},
			Tabs:                      map[string][]config.TabEntry{},
			Shortcuts:                 map[string]map[string]string{},
		},
		Runtime: runtime,
		BaseURL: "http://127.0.0.1:4202",
		Logger:  logger,
	})

	status, body := request(t, handler, http.MethodGet, "/api/system/runtime", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var data struct {
		Environment string `json:"environment"`
		Home        string `json:"home"`
		ConfigPath  string `json:"config_path"`
		DBPath      string `json:"db_path"`
		LogDir      string `json:"log_dir"`
		BindAddress string `json:"bind_address"`
		Port        int    `json:"port"`
		BaseURL     string `json:"base_url"`
	}
	decodeEnvelopeData(t, body, &data)

	if data.Environment != "development" {
		t.Fatalf("expected environment %q, got %q", "development", data.Environment)
	}
	if data.Home != runtime.Home {
		t.Fatalf("expected home %q, got %q", runtime.Home, data.Home)
	}
	if data.ConfigPath != runtime.ConfigPath {
		t.Fatalf("expected config path %q, got %q", runtime.ConfigPath, data.ConfigPath)
	}
	if data.DBPath != runtime.DBPath {
		t.Fatalf("expected db path %q, got %q", runtime.DBPath, data.DBPath)
	}
	if data.LogDir != runtime.LogDir {
		t.Fatalf("expected log dir %q, got %q", runtime.LogDir, data.LogDir)
	}
	if data.BindAddress != "127.0.0.1" {
		t.Fatalf("expected bind address %q, got %q", "127.0.0.1", data.BindAddress)
	}
	if data.Port != 4202 {
		t.Fatalf("expected port %d, got %d", 4202, data.Port)
	}
	if data.BaseURL != "http://127.0.0.1:4202" {
		t.Fatalf("expected base URL %q, got %q", "http://127.0.0.1:4202", data.BaseURL)
	}
}
