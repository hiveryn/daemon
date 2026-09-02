package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/roadmapfs"
	"gopkg.in/yaml.v3"
)

func TestAgentProfilesReadOnlyAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/agent-profiles", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		AgentProfiles []agentProfileResponse `json:"agent_profiles"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.AgentProfiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(listed.AgentProfiles))
	}
	if listed.AgentProfiles[0].Name != "claude-sonnet" {
		t.Fatalf("expected sorted profile list, got %#v", listed.AgentProfiles)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/agent-profiles/codex-personal", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var profile agentProfileResponse
	decodeEnvelopeData(t, getBody, &profile)
	if profile.Name != "codex-personal" || profile.Agent != "codex" {
		t.Fatalf("unexpected profile payload: %#v", profile)
	}
	if profile.Env["CODEX_HOME"] == "" {
		t.Fatalf("expected CODEX_HOME env to be present: %#v", profile.Env)
	}
}

func TestAgentProfilesMutationEndpointsRemoved(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		path := "/api/agent-profiles"
		if method != http.MethodPost {
			path = "/api/agent-profiles/codex-personal"
		}
		status, _ := requestJSON(t, handler, method, path, map[string]any{"name": "ignored"})
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("expected %s %s to return %d, got %d", method, path, http.StatusMethodNotAllowed, status)
		}
	}
}

func TestAgentProfileNotFound(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/agent-profiles/missing", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, status, string(body))
	}

	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND code, got %q", errBody.Code)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/health", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload map[string]string
	decodeEnvelopeData(t, body, &payload)
	if payload["status"] != "ok" {
		t.Fatalf("expected health status ok, got %q", payload["status"])
	}
}

func TestDesktopConfig(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/config/desktop", nil)
	if status != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload desktopConfigResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.HealthPollIntervalMS != 1000 {
		t.Fatalf("expected health poll interval 1000ms, got %d", payload.HealthPollIntervalMS)
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := config.ResolveRuntime("", "")
	if err != nil {
		t.Fatalf("resolve runtime: %v", err)
	}
	cfg := testConfig()
	return NewHandler(Dependencies{
		Config:  cfg,
		Runtime: runtime,
		BaseURL: "http://127.0.0.1:4200",
		Logger:  logger,
	})
}

func newReloadingTestHandler(t *testing.T, cfgPath string, tickets domain.TicketService) http.Handler {
	t.Helper()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(Dependencies{
		Config:       cfg,
		ConfigSource: source,
		Logger:       logger,
		Tickets:      tickets,
		Roadmaps:     roadmapfs.NewService(tickets),
	})
}

func writeReloadingConfigFiles(t *testing.T, configDir string, architects map[string]config.ArchitectConfig) string {
	t.Helper()

	path := filepath.Join(configDir, "config.yaml")
	writeYAMLConfigFile(t, path, map[string]any{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "debug",
	})
	writeYAMLConfigFile(t, filepath.Join(configDir, "variants.yaml"), map[string]config.VariantConfig{
		"codex-personal": {Agent: "codex"},
	})
	writeAPIArchitects(t, configDir, architects)
	return path
}

// writeAPIArchitects writes the bare architects.yaml registry (key -> path)
// plus a hiveryn.yaml in each architect's workspace, derived from the test's
// ArchitectConfig values.
func writeAPIArchitects(t *testing.T, configDir string, architects map[string]config.ArchitectConfig) {
	t.Helper()

	registry := map[string]string{}
	for key, architect := range architects {
		registry[key] = architect.Path
		name := architect.Name
		if name == "" {
			name = key
		}
		repos := map[string]string{}
		for repoKey, repoPath := range architect.Repos {
			repos[repoKey] = repoPath
		}
		writeYAMLConfigFile(t, filepath.Join(architect.Path, "hiveryn.yaml"), map[string]any{
			"name":  name,
			"repos": repos,
		})
	}
	writeYAMLConfigFile(t, filepath.Join(configDir, "architects.yaml"), registry)
}

func writeYAMLConfigFile(t *testing.T, path string, v any) {
	t.Helper()

	content, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal yaml %q: %v", path, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func testConfig() config.Config {
	return config.Config{
		Port:                      4200,
		BindAddress:               "127.0.0.1",
		LogLevel:                  "debug",
		DesktopHealthPollInterval: "1s",
		Variants: map[string]config.VariantConfig{
			"codex-personal": {
				Agent: "codex",
				Args:  []string{"--dangerously-bypass-approvals-and-sandbox"},
				Env: map[string]string{
					"CODEX_HOME": "/Users/kareem/.codex-personal",
				},
			},
			"claude-sonnet": {
				Agent: "claude",
				Args:  []string{"--dangerously-skip-permissions", "--model", "claude-sonnet-4-6"},
				Env:   map[string]string{},
			},
		},
		Architects: map[string]config.ArchitectConfig{
			"hiveryn": {
				Name: "Hiveryn",
				Path: "/Users/kareem/architects/hiveryn",
				Repos: map[string]string{
					"daemon":  "/Users/kareem/hiveryn/daemon",
					"desktop": "/Users/kareem/hiveryn/desktop",
				},
			},
			"litho": {
				Name: "Litho",
				Path: "/Users/kareem/architects/litho",
				Repos: map[string]string{
					"app": "/Users/kareem/litho/lithoapp",
				},
			},
		},
	}
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, body any) (int, []byte) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return request(t, handler, method, path, bytes.NewReader(payload))
}

func request(t *testing.T, handler http.Handler, method, path string, body io.Reader) (int, []byte) {
	t.Helper()

	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, respBody
}

func decodeEnvelopeData(t *testing.T, body []byte, dst any) {
	t.Helper()

	var env struct {
		Data json.RawMessage `json:"data"`
		Meta domain.Meta     `json:"meta"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, string(body))
	}
	if env.Meta.RequestID == "" {
		t.Fatal("expected non-empty meta.request_id")
	}
	if dst != nil {
		if err := json.Unmarshal(env.Data, dst); err != nil {
			t.Fatalf("decode envelope data: %v\ndata: %s", err, string(env.Data))
		}
	}
}

func decodeEnvelopeError(t *testing.T, body []byte) domain.ErrorBody {
	t.Helper()

	var env struct {
		Error domain.ErrorBody `json:"error"`
		Meta  domain.Meta      `json:"meta"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope error: %v\nbody: %s", err, string(body))
	}
	if env.Meta.RequestID == "" {
		t.Fatal("expected non-empty meta.request_id")
	}
	if env.Error.Code == "" {
		t.Fatal("expected non-empty error code in envelope")
	}
	return env.Error
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
