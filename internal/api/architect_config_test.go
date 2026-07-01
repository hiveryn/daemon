package api

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/sessionruntime"
)

// newConfigTestHandler wires a reloading handler over a single architect whose
// workspace is a temp dir, and returns the handler and that workspace path.
func newConfigTestHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	configDir := t.TempDir()
	workspace := t.TempDir()
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Name:  "Hiveryn",
			Path:  workspace,
			Repos: map[string]string{"daemon": "/repos/daemon"},
		},
	})
	return newReloadingTestHandler(t, cfgPath, nil), workspace
}

func TestConfigListRepos(t *testing.T) {
	handler, _ := newConfigTestHandler(t)
	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/repos", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	var out struct {
		Repos []repoResponse `json:"repos"`
	}
	decodeEnvelopeData(t, body, &out)
	if len(out.Repos) != 1 || out.Repos[0].Key != "daemon" {
		t.Fatalf("repos = %#v", out.Repos)
	}
}

func TestConfigAddAndRemoveRepo(t *testing.T) {
	handler, _ := newConfigTestHandler(t)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/config/repos", map[string]any{
		"key": "desktop", "path": "/repos/desktop",
	})
	if status != http.StatusOK {
		t.Fatalf("add status = %d, body: %s", status, body)
	}
	var added repoResponse
	decodeEnvelopeData(t, body, &added)
	if added.Key != "desktop" || added.Path != "/repos/desktop" {
		t.Fatalf("added = %#v", added)
	}

	// duplicate → conflict
	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/config/repos", map[string]any{
		"key": "desktop", "path": "/x",
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate add status = %d, body: %s", status, body)
	}

	status, body = request(t, handler, http.MethodDelete, "/api/architects/hiveryn/config/repos/desktop", nil)
	if status != http.StatusOK {
		t.Fatalf("remove status = %d, body: %s", status, body)
	}
}

func TestConfigAddKickoffScaffoldsDefault(t *testing.T) {
	handler, workspace := newConfigTestHandler(t)

	status, body := requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/config/kickoffs", map[string]any{
		"path": "prompts/kickoff.md",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	var out addKickoffResponse
	decodeEnvelopeData(t, body, &out)
	if !out.Created {
		t.Fatal("expected created = true")
	}
	if !out.Default {
		t.Fatal("expected default = true for no-repos kickoff")
	}
	wantAbs := filepath.Join(workspace, "prompts/kickoff.md")
	if out.Path != wantAbs {
		t.Fatalf("path = %q, want %q", out.Path, wantAbs)
	}

	got, err := os.ReadFile(wantAbs)
	if err != nil {
		t.Fatalf("read scaffolded file: %v", err)
	}
	want, err := sessionruntime.DefaultPromptTemplate("ticket-kickoff")
	if err != nil {
		t.Fatalf("default template: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("scaffolded content does not match embedded default")
	}

	// second default → conflict
	status, body = requestJSON(t, handler, http.MethodPost, "/api/architects/hiveryn/config/kickoffs", map[string]any{
		"path": "prompts/other.md",
	})
	if status != http.StatusConflict {
		t.Fatalf("second default status = %d, body: %s", status, body)
	}
}

func TestConfigSetArchitectSystemScaffolds(t *testing.T) {
	handler, workspace := newConfigTestHandler(t)

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config/architect-prompts/system", map[string]any{
		"path": "prompts/SYSTEM.md",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	var out setPromptResponse
	decodeEnvelopeData(t, body, &out)
	if !out.Created {
		t.Fatal("expected created = true")
	}

	got, err := os.ReadFile(filepath.Join(workspace, "prompts/SYSTEM.md"))
	if err != nil {
		t.Fatalf("read scaffolded file: %v", err)
	}
	want, _ := sessionruntime.DefaultPromptTemplate("architect-system")
	if !bytes.Equal(got, want) {
		t.Fatal("scaffolded system prompt does not match embedded default")
	}

	// getArchitectPrompts should now report it
	status, body = request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/architect-prompts", nil)
	if status != http.StatusOK {
		t.Fatalf("get prompts status = %d, body: %s", status, body)
	}
	var prompts architectPromptsResponse
	decodeEnvelopeData(t, body, &prompts)
	if prompts.System == nil || prompts.System.Path != filepath.Join(workspace, "prompts/SYSTEM.md") {
		t.Fatalf("system prompt = %#v", prompts.System)
	}
	if prompts.Kickoff != nil {
		t.Fatalf("kickoff should be nil, got %#v", prompts.Kickoff)
	}
}

func TestConfigDescribePromptSchema(t *testing.T) {
	handler, _ := newConfigTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/prompt-schema?kind=ticket", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	var out struct {
		Variables []sessionruntime.PromptVariable `json:"variables"`
	}
	decodeEnvelopeData(t, body, &out)
	if len(out.Variables) == 0 {
		t.Fatal("expected non-empty variables")
	}

	// missing kind → 400
	status, _ = request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/prompt-schema", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("missing kind status = %d", status)
	}
}

func TestConfigUnknownArchitect404(t *testing.T) {
	handler, _ := newConfigTestHandler(t)
	status, body := requestJSON(t, handler, http.MethodPost, "/api/architects/ghost/config/repos", map[string]any{
		"key": "x", "path": "/x",
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != "NOT_FOUND" {
		t.Fatalf("code = %q", errBody.Code)
	}
}
