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

// writeGitRepo creates a temp directory containing a .git subdirectory,
// simulating a real git repo, and returns its absolute path.
func writeGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return dir
}

// newConfigTestHandler wires a reloading handler over a single architect whose
// workspace is a temp dir (with a hiveryn.yaml written by writeAPIArchitects),
// and returns the handler, that workspace path, and the "daemon" repo's path
// (a real git repo, since config writes now validate repo paths on disk).
func newConfigTestHandler(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	configDir := t.TempDir()
	workspace := t.TempDir()
	daemonRepo := writeGitRepo(t)
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Name:  "Hiveryn",
			Path:  workspace,
			Repos: map[string]string{"daemon": daemonRepo},
		},
	})
	return newReloadingTestHandler(t, cfgPath, nil), workspace, daemonRepo
}

func readConfig(t *testing.T, handler http.Handler) readArchitectConfigResponse {
	t.Helper()
	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/config", nil)
	if status != http.StatusOK {
		t.Fatalf("read status = %d, body: %s", status, body)
	}
	var out readArchitectConfigResponse
	decodeEnvelopeData(t, body, &out)
	return out
}

func TestConfigReadReturnsDocAndVersion(t *testing.T) {
	handler, _, daemonRepo := newConfigTestHandler(t)
	out := readConfig(t, handler)
	if out.Version == "" {
		t.Fatal("expected non-empty version")
	}
	if out.Config.Repos["daemon"] != daemonRepo {
		t.Fatalf("repos = %#v", out.Config.Repos)
	}
	if out.Resolved.Repos["daemon"] != daemonRepo {
		t.Fatalf("resolved repos = %#v", out.Resolved.Repos)
	}
}

func TestConfigUpdateReplacesWholeDoc(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)
	read := readConfig(t, handler)

	desktopRepo := writeGitRepo(t)
	doc := read.Config
	doc.Repos["desktop"] = desktopRepo

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config", map[string]any{
		"config":  doc,
		"version": read.Version,
	})
	if status != http.StatusOK {
		t.Fatalf("update status = %d, body: %s", status, body)
	}
	var out updateArchitectConfigResponse
	decodeEnvelopeData(t, body, &out)
	if out.Config.Repos["desktop"] != desktopRepo {
		t.Fatalf("desktop not persisted: %#v", out.Config.Repos)
	}
	if out.Version == read.Version {
		t.Fatal("expected version to change after update")
	}
}

func TestConfigUpdateVersionConflict(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)
	read := readConfig(t, handler)

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config", map[string]any{
		"config":  read.Config,
		"version": "stale-token",
	})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, body: %s", status, body)
	}
}

func TestConfigUpdateMissingVersionValidation(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)
	read := readConfig(t, handler)

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config", map[string]any{
		"config":  read.Config,
		"version": "",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body: %s", status, body)
	}
}

func TestConfigUpdateInvalidConfigValidation(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)
	read := readConfig(t, handler)

	doc := read.Config
	doc.Prompts.Ticket.Kickoffs = []ticketKickoffDocWire{{Path: "k.md", Repos: []string{"ghost"}}}

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config", map[string]any{
		"config":  doc,
		"version": read.Version,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body: %s", status, body)
	}
}

func TestConfigUpdateScaffoldsMissingPrompt(t *testing.T) {
	handler, workspace, _ := newConfigTestHandler(t)
	read := readConfig(t, handler)

	doc := read.Config
	doc.Prompts.Ticket.Kickoffs = []ticketKickoffDocWire{{Path: "prompts/kickoff.md", Repos: []string{}}}

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config", map[string]any{
		"config":  doc,
		"version": read.Version,
	})
	if status != http.StatusOK {
		t.Fatalf("update status = %d, body: %s", status, body)
	}
	var out updateArchitectConfigResponse
	decodeEnvelopeData(t, body, &out)

	wantAbs := filepath.Join(workspace, "prompts/kickoff.md")
	found := false
	for _, c := range out.Created {
		if c == wantAbs {
			found = true
		}
	}
	if !found {
		t.Fatalf("created = %v, want to contain %q", out.Created, wantAbs)
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
		t.Fatal("scaffolded content does not match embedded default")
	}
}

func TestConfigReadWarnsMissingWiredPrompt(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)
	read := readConfig(t, handler)

	// Wiring a prompt path scaffolds the file; delete it afterward so a fresh
	// read observes the missing-file warning + exists=false.
	doc := read.Config
	doc.Prompts.Architect.System = "prompts/SYSTEM.md"
	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/config", map[string]any{
		"config":  doc,
		"version": read.Version,
	})
	if status != http.StatusOK {
		t.Fatalf("update status = %d, body: %s", status, body)
	}
	var updated updateArchitectConfigResponse
	decodeEnvelopeData(t, body, &updated)
	if updated.Resolved.Prompts.Architect.System == nil || !updated.Resolved.Prompts.Architect.System.Exists {
		t.Fatalf("expected scaffolded system prompt to exist: %#v", updated.Resolved.Prompts.Architect.System)
	}

	// delete the scaffolded file, then read → warning + exists=false
	if err := os.Remove(updated.Resolved.Prompts.Architect.System.Path); err != nil {
		t.Fatalf("remove scaffolded file: %v", err)
	}
	after := readConfig(t, handler)
	if len(after.Warnings) == 0 {
		t.Fatal("expected a warning for the missing wired prompt")
	}
	if after.Resolved.Prompts.Architect.System == nil || after.Resolved.Prompts.Architect.System.Exists {
		t.Fatalf("expected exists=false, got %#v", after.Resolved.Prompts.Architect.System)
	}
}

func TestConfigReadDefaultPrompt(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/default-prompt?kind=ticket-kickoff", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	var out readDefaultPromptResponse
	decodeEnvelopeData(t, body, &out)
	if out.Template == "" {
		t.Fatal("expected non-empty template")
	}
	if len(out.Variables) == 0 {
		t.Fatal("expected ticket-kickoff variables")
	}

	// architect-system → template, no variables
	status, body = request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/default-prompt?kind=architect-system", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	decodeEnvelopeData(t, body, &out)
	if out.Template == "" {
		t.Fatal("expected non-empty architect-system template")
	}
	if len(out.Variables) != 0 {
		t.Fatalf("architect-system variables = %v, want none", out.Variables)
	}

	// bogus kind → 400
	status, _ = request(t, handler, http.MethodGet, "/api/architects/hiveryn/config/default-prompt?kind=bogus", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bogus kind status = %d", status)
	}
}

func TestConfigUnknownArchitect404(t *testing.T) {
	handler, _, _ := newConfigTestHandler(t)
	status, body := request(t, handler, http.MethodGet, "/api/architects/ghost/config", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != "NOT_FOUND" {
		t.Fatalf("code = %q", errBody.Code)
	}
}
