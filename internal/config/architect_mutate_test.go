package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testArchitectKey = "hiveryn"

// writeWorkspace creates a temp architect workspace containing a hiveryn.yaml
// with the given contents and returns the workspace path.
func writeWorkspace(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, architectConfigFileName), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write hiveryn.yaml: %v", err)
	}
	return dir
}

// reload parses the workspace's hiveryn.yaml through the loader, asserting it
// still loads (validation parity) and returning the resolved config.
func reload(t *testing.T, workspace string) ArchitectConfig {
	t.Helper()
	architect, err := loadArchitectFile(testArchitectKey, workspace)
	if err != nil {
		t.Fatalf("loadArchitectFile after mutation: %v", err)
	}
	if err := validateArchitect(testArchitectKey, architect); err != nil {
		t.Fatalf("validateArchitect after mutation: %v", err)
	}
	return architect
}

func rawYAML(t *testing.T, workspace string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace, architectConfigFileName))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	return string(raw)
}

func assertMutationKind(t *testing.T, err error, want MutationErrorKind) {
	t.Helper()
	var mErr *MutationError
	if !errors.As(err, &mErr) {
		t.Fatalf("expected *MutationError, got %T: %v", err, err)
	}
	if mErr.Kind != want {
		t.Fatalf("mutation error kind = %q, want %q (message: %s)", mErr.Kind, want, mErr.Message)
	}
}

func TestReadArchitectConfig(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: ~/repos/daemon\nprompts:\n  ticket:\n    kickoffs:\n      - path: k.md\n        repos: [daemon]\n")

	view, err := ReadArchitectConfig(ws, testArchitectKey)
	if err != nil {
		t.Fatalf("ReadArchitectConfig: %v", err)
	}
	if view.Version == "" {
		t.Fatal("expected non-empty version")
	}
	if view.Config.Repos["daemon"] != "~/repos/daemon" {
		t.Fatalf("repo path not verbatim: %q", view.Config.Repos["daemon"])
	}
	if len(view.Config.Prompts.Ticket) != 1 || view.Config.Prompts.Ticket[0].Path != "k.md" {
		t.Fatalf("kickoffs = %#v", view.Config.Prompts.Ticket)
	}
	// resolved repo path is home-expanded/absolute
	if !filepath.IsAbs(view.Resolved.Repos["daemon"]) {
		t.Fatalf("resolved repo not absolute: %q", view.Resolved.Repos["daemon"])
	}
}

func TestReadArchitectConfigMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadArchitectConfig(dir, testArchitectKey)
	assertMutationKind(t, err, MutationErrorNotFound)
}

func TestReplaceRoundTripPreservesNameAndVerbatimPaths(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: ~/repos/daemon\n")

	view, err := ReadArchitectConfig(ws, testArchitectKey)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	doc := view.Config
	doc.Repos["desktop"] = "/repos/desktop"

	out, err := ReplaceArchitectConfig(ws, testArchitectKey, view.Version, doc)
	if err != nil {
		t.Fatalf("ReplaceArchitectConfig: %v", err)
	}
	if out.Version == view.Version {
		t.Fatal("expected version to change after write (read-after-write)")
	}

	architect := reload(t, ws)
	if architect.Name != "Hiveryn" {
		t.Fatalf("name not preserved: %q", architect.Name)
	}
	if _, ok := architect.Repos["desktop"]; !ok {
		t.Fatal("desktop repo not added")
	}
	// untouched repo path stays verbatim in the file
	if !strings.Contains(rawYAML(t, ws), "daemon: ~/repos/daemon") {
		t.Fatalf("daemon repo path rewritten, yaml:\n%s", rawYAML(t, ws))
	}

	// the returned version is the live one — a chained edit with it succeeds
	doc2 := out.Config
	doc2.Repos["shared"] = "/repos/shared"
	if _, err := ReplaceArchitectConfig(ws, testArchitectKey, out.Version, doc2); err != nil {
		t.Fatalf("chained replace with returned version: %v", err)
	}
}

func TestReplaceVersionMissing(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)
	_, err := ReplaceArchitectConfig(ws, testArchitectKey, "  ", view.Config)
	assertMutationKind(t, err, MutationErrorValidation)
}

func TestReplaceVersionMismatch(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)
	_, err := ReplaceArchitectConfig(ws, testArchitectKey, "deadbeef", view.Config)
	assertMutationKind(t, err, MutationErrorConflict)
}

func TestReplaceValidationRefusalLeavesFileIntact(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)

	doc := view.Config
	doc.Prompts.Ticket = append(doc.Prompts.Ticket, TicketKickoffDoc{Path: "k.md", Repos: []string{"ghost"}})

	_, err := ReplaceArchitectConfig(ws, testArchitectKey, view.Version, doc)
	assertMutationKind(t, err, MutationErrorValidation)

	// file untouched — still loads, no kickoffs
	architect := reload(t, ws)
	if len(architect.TicketKickoffs) != 0 {
		t.Fatalf("kickoffs = %d, want 0 (refused write should not persist)", len(architect.TicketKickoffs))
	}
}

func TestReplaceMultipleDefaultKickoffsRejected(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)
	doc := view.Config
	doc.Prompts.Ticket = []TicketKickoffDoc{{Path: "a.md"}, {Path: "b.md"}}
	_, err := ReplaceArchitectConfig(ws, testArchitectKey, view.Version, doc)
	assertMutationKind(t, err, MutationErrorValidation)
}

func TestReplaceEmptyConfigPreservesName(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\nprompts:\n  ticket:\n    kickoffs:\n      - path: k.md\n        repos: [daemon]\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)

	if _, err := ReplaceArchitectConfig(ws, testArchitectKey, view.Version, ArchitectConfigDoc{}); err != nil {
		t.Fatalf("ReplaceArchitectConfig with empty doc: %v", err)
	}
	architect := reload(t, ws)
	if architect.Name != "Hiveryn" {
		t.Fatalf("name not preserved: %q", architect.Name)
	}
	if len(architect.Repos) != 0 {
		t.Fatalf("repos = %v, want empty", architect.Repos)
	}
	if len(architect.TicketKickoffs) != 0 {
		t.Fatalf("kickoffs = %d, want 0", len(architect.TicketKickoffs))
	}
}

func TestReplaceDropsUnknownTopLevelKeys(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\ncustom: keepme\nrepos:\n  daemon: /repos/daemon\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)
	if _, err := ReplaceArchitectConfig(ws, testArchitectKey, view.Version, view.Config); err != nil {
		t.Fatalf("ReplaceArchitectConfig: %v", err)
	}
	if strings.Contains(rawYAML(t, ws), "custom:") {
		t.Fatalf("unknown key not dropped, yaml:\n%s", rawYAML(t, ws))
	}
}

func TestReplaceWiresPromptsVerbatim(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	view, _ := ReadArchitectConfig(ws, testArchitectKey)

	doc := view.Config
	doc.Prompts.Architect.System = "prompts/SYSTEM.md"
	doc.Prompts.Ticket = append(doc.Prompts.Ticket, TicketKickoffDoc{Path: "prompts/k.md"})

	out, err := ReplaceArchitectConfig(ws, testArchitectKey, view.Version, doc)
	if err != nil {
		t.Fatalf("ReplaceArchitectConfig: %v", err)
	}

	raw := rawYAML(t, ws)
	if !strings.Contains(raw, "system: prompts/SYSTEM.md") {
		t.Fatalf("system path not stored verbatim, yaml:\n%s", raw)
	}
	if !strings.Contains(raw, "path: prompts/k.md") {
		t.Fatalf("kickoff path not stored verbatim, yaml:\n%s", raw)
	}
	// resolved surfaces the absolute path
	if out.Resolved.SystemPromptPath != filepath.Join(ws, "prompts/SYSTEM.md") {
		t.Fatalf("resolved system path = %q", out.Resolved.SystemPromptPath)
	}
}
