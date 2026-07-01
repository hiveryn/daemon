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

func TestAddRepo(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")

	abs, err := AddRepo(ws, testArchitectKey, "desktop", "/repos/desktop")
	if err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if abs != "/repos/desktop" {
		t.Fatalf("AddRepo returned path %q, want /repos/desktop", abs)
	}

	architect := reload(t, ws)
	if architect.Repos["desktop"] != "/repos/desktop" {
		t.Fatalf("desktop repo = %q", architect.Repos["desktop"])
	}
	if architect.Repos["daemon"] != "/repos/daemon" {
		t.Fatalf("daemon repo not preserved: %q", architect.Repos["daemon"])
	}
	if architect.Name != "Hiveryn" {
		t.Fatalf("name not preserved: %q", architect.Name)
	}
}

func TestAddRepoDuplicateConflict(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	_, err := AddRepo(ws, testArchitectKey, "daemon", "/other")
	assertMutationKind(t, err, MutationErrorConflict)
}

func TestRemoveRepo(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n  desktop: /repos/desktop\n")
	if err := RemoveRepo(ws, testArchitectKey, "desktop"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	architect := reload(t, ws)
	if _, ok := architect.Repos["desktop"]; ok {
		t.Fatal("desktop repo not removed")
	}
	if _, ok := architect.Repos["daemon"]; !ok {
		t.Fatal("daemon repo should remain")
	}
}

func TestRemoveRepoNotFound(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	err := RemoveRepo(ws, testArchitectKey, "ghost")
	assertMutationKind(t, err, MutationErrorNotFound)
}

func TestRemoveRepoReferencedByKickoffConflict(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\nprompts:\n  ticket:\n    kickoffs:\n      - path: k.md\n        repos: [daemon]\n")
	err := RemoveRepo(ws, testArchitectKey, "daemon")
	assertMutationKind(t, err, MutationErrorConflict)
}

func TestAddKickoffDefaultAndStoreRelative(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")

	abs, err := AddKickoff(ws, testArchitectKey, "prompts/kickoff.md", nil)
	if err != nil {
		t.Fatalf("AddKickoff: %v", err)
	}
	wantAbs := filepath.Join(ws, "prompts/kickoff.md")
	if abs != wantAbs {
		t.Fatalf("AddKickoff returned %q, want %q", abs, wantAbs)
	}

	// stored form should be relative to the workspace
	raw, err := os.ReadFile(filepath.Join(ws, architectConfigFileName))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	if got := string(raw); !strings.Contains(got, "path: prompts/kickoff.md") {
		t.Fatalf("expected relative path stored, yaml:\n%s", got)
	}

	architect := reload(t, ws)
	if len(architect.TicketKickoffs) != 1 {
		t.Fatalf("kickoffs = %d, want 1", len(architect.TicketKickoffs))
	}
	if architect.TicketKickoffs[0].Path != wantAbs {
		t.Fatalf("resolved kickoff path = %q, want %q", architect.TicketKickoffs[0].Path, wantAbs)
	}
}

func TestAddKickoffSecondDefaultConflict(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\nprompts:\n  ticket:\n    kickoffs:\n      - path: a.md\n")
	_, err := AddKickoff(ws, testArchitectKey, "b.md", nil)
	assertMutationKind(t, err, MutationErrorConflict)
}

func TestAddKickoffUnknownRepo(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	_, err := AddKickoff(ws, testArchitectKey, "a.md", []string{"ghost"})
	assertMutationKind(t, err, MutationErrorValidation)
}

func TestAddKickoffRepoAlreadyClaimedConflict(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\nprompts:\n  ticket:\n    kickoffs:\n      - path: a.md\n        repos: [daemon]\n")
	_, err := AddKickoff(ws, testArchitectKey, "b.md", []string{"daemon"})
	assertMutationKind(t, err, MutationErrorConflict)
}

func TestAddKickoffDuplicatePathConflict(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\nprompts:\n  ticket:\n    kickoffs:\n      - path: a.md\n        repos: [daemon]\n")
	_, err := AddKickoff(ws, testArchitectKey, "a.md", nil)
	assertMutationKind(t, err, MutationErrorConflict)
}

func TestUpdateKickoff(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n  desktop: /repos/desktop\nprompts:\n  ticket:\n    kickoffs:\n      - path: a.md\n        repos: [daemon]\n")
	if _, err := UpdateKickoff(ws, testArchitectKey, "a.md", []string{"daemon", "desktop"}); err != nil {
		t.Fatalf("UpdateKickoff: %v", err)
	}
	architect := reload(t, ws)
	if len(architect.TicketKickoffs[0].Repos) != 2 {
		t.Fatalf("repos = %v, want 2 entries", architect.TicketKickoffs[0].Repos)
	}
}

func TestUpdateKickoffNotFound(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	_, err := UpdateKickoff(ws, testArchitectKey, "ghost.md", nil)
	assertMutationKind(t, err, MutationErrorNotFound)
}

func TestRemoveKickoffReturnsFallbackRepos(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n  desktop: /repos/desktop\nprompts:\n  ticket:\n    kickoffs:\n      - path: a.md\n        repos: [daemon, desktop]\n")
	fallback, err := RemoveKickoff(ws, testArchitectKey, "a.md")
	if err != nil {
		t.Fatalf("RemoveKickoff: %v", err)
	}
	if len(fallback) != 2 {
		t.Fatalf("fallback = %v, want [daemon desktop]", fallback)
	}
	architect := reload(t, ws)
	if len(architect.TicketKickoffs) != 0 {
		t.Fatalf("kickoffs = %d, want 0", len(architect.TicketKickoffs))
	}
}

func TestRemoveKickoffNotFound(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	_, err := RemoveKickoff(ws, testArchitectKey, "ghost.md")
	assertMutationKind(t, err, MutationErrorNotFound)
}

func TestSetArchitectPromptsStoreRelativeReturnAbsolute(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")

	sysAbs, err := SetArchitectSystem(ws, testArchitectKey, "prompts/SYSTEM.md")
	if err != nil {
		t.Fatalf("SetArchitectSystem: %v", err)
	}
	kickAbs, err := SetArchitectKickoff(ws, testArchitectKey, "prompts/KICKOFF.md")
	if err != nil {
		t.Fatalf("SetArchitectKickoff: %v", err)
	}

	architect := reload(t, ws)
	if architect.SystemPromptPath != sysAbs || sysAbs != filepath.Join(ws, "prompts/SYSTEM.md") {
		t.Fatalf("system prompt path = %q (returned %q)", architect.SystemPromptPath, sysAbs)
	}
	if architect.KickoffPromptPath != kickAbs || kickAbs != filepath.Join(ws, "prompts/KICKOFF.md") {
		t.Fatalf("kickoff prompt path = %q (returned %q)", architect.KickoffPromptPath, kickAbs)
	}

	raw, _ := os.ReadFile(filepath.Join(ws, architectConfigFileName))
	if !strings.Contains(string(raw), "system: prompts/SYSTEM.md") {
		t.Fatalf("expected relative system path stored, yaml:\n%s", raw)
	}
}

func TestSetArchitectPromptAbsoluteOutsideWorkspaceStoredAbsolute(t *testing.T) {
	ws := writeWorkspace(t, "name: Hiveryn\nrepos:\n  daemon: /repos/daemon\n")
	abs, err := SetArchitectSystem(ws, testArchitectKey, "/elsewhere/SYSTEM.md")
	if err != nil {
		t.Fatalf("SetArchitectSystem: %v", err)
	}
	if abs != "/elsewhere/SYSTEM.md" {
		t.Fatalf("returned %q, want /elsewhere/SYSTEM.md", abs)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, architectConfigFileName))
	if !strings.Contains(string(raw), "system: /elsewhere/SYSTEM.md") {
		t.Fatalf("expected absolute path stored, yaml:\n%s", raw)
	}
}
