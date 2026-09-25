package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeHiverynYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, architectConfigFileName), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readHiverynYAML(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, architectConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAddAvailableActionAppendsAndPreservesEverythingElse(t *testing.T) {
	t.Parallel()

	const original = `# project config
name: Hiveryn
repos:
    daemon: /tmp/daemon # the daemon
availableActions:
    - demo-evidence
`
	dir := writeHiverynYAML(t, original)

	update, err := AddAvailableAction(dir, "hiveryn", "nova-export")
	if err != nil {
		t.Fatalf("AddAvailableAction: %v", err)
	}
	if !update.Changed || !slices.Equal(update.AvailableActions, []string{"demo-evidence", "nova-export"}) {
		t.Fatalf("update = %+v", update)
	}
	want := strings.Replace(original, "    - demo-evidence\n", "    - demo-evidence\n    - nova-export\n", 1)
	if got := readHiverynYAML(t, dir); got != want {
		t.Fatalf("hiveryn.yaml =\n%s\nwant\n%s", got, want)
	}
	info, err := os.Stat(filepath.Join(dir, architectConfigFileName))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, %v; want the original 0640", info.Mode().Perm(), err)
	}

	// Repeating it succeeds without a duplicate or a write.
	before := readHiverynYAML(t, dir)
	again, err := AddAvailableAction(dir, "hiveryn", "nova-export")
	if err != nil || again.Changed || !slices.Equal(again.AvailableActions, []string{"demo-evidence", "nova-export"}) {
		t.Fatalf("repeat = %+v, %v; want unchanged", again, err)
	}
	if readHiverynYAML(t, dir) != before {
		t.Fatal("repeat rewrote the file")
	}

	// The result loads exactly as the loader sees it.
	resolved, err := ValidateArchitectConfig(dir, "hiveryn")
	if err != nil || resolved.Repos["daemon"] != "/tmp/daemon" || !slices.Equal(resolved.AvailableActions, update.AvailableActions) {
		t.Fatalf("validated = %+v, %v", resolved, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("workspace holds %d entries, want only hiveryn.yaml (no temporary file left)", len(entries))
	}
}

func TestAddAvailableActionCreatesTheList(t *testing.T) {
	t.Parallel()

	for name, content := range map[string]string{
		"absent": "name: Hiveryn\n",
		"null":   "name: Hiveryn\navailableActions:\n",
		"empty":  "name: Hiveryn\navailableActions: []\n",
	} {
		dir := writeHiverynYAML(t, content)
		update, err := AddAvailableAction(dir, "hiveryn", "demo")
		if err != nil || !update.Changed || !slices.Equal(update.AvailableActions, []string{"demo"}) {
			t.Fatalf("%s: update = %+v, %v", name, update, err)
		}
		resolved, err := ValidateArchitectConfig(dir, "hiveryn")
		if err != nil || resolved.Name != "Hiveryn" || !slices.Equal(resolved.AvailableActions, []string{"demo"}) {
			t.Fatalf("%s: validated = %+v, %v", name, resolved, err)
		}
	}
}

func TestAddAvailableActionRefusesAndLeavesTheFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, content, action string
		invalidConfig         bool
	}{
		{"bad name", "name: Hiveryn\n", "Bad Name", false},
		{"unknown key", "name: Hiveryn\nprompts: {}\n", "demo", true},
		{"missing name", "repos: {}\n", "demo", true},
		{"not a list", "name: Hiveryn\navailableActions: demo\n", "demo", true},
		{"duplicate entry", "name: Hiveryn\navailableActions: [demo, demo]\n", "other", true},
		{"malformed YAML", "name: [Hiveryn\n", "demo", true},
	}
	for _, tc := range cases {
		dir := writeHiverynYAML(t, tc.content)
		_, err := AddAvailableAction(dir, "hiveryn", tc.action)
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		if errors.Is(err, ErrInvalidArchitectConfig) != tc.invalidConfig {
			t.Errorf("%s: err = %v, invalid-config = %v, want %v", tc.name, err, !tc.invalidConfig, tc.invalidConfig)
		}
		if readHiverynYAML(t, dir) != tc.content {
			t.Errorf("%s: file was changed", tc.name)
		}
	}

	// A missing file is a filesystem failure, reported as such.
	_, err := AddAvailableAction(t.TempDir(), "hiveryn", "demo")
	if err == nil || errors.Is(err, ErrInvalidArchitectConfig) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v, want a not-exist filesystem error", err)
	}
}

func TestAddAvailableActionFollowsASymlinkedFile(t *testing.T) {
	t.Parallel()

	target := writeHiverynYAML(t, "name: Hiveryn\n")
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(target, architectConfigFileName), filepath.Join(dir, architectConfigFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := AddAvailableAction(dir, "hiveryn", "demo"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(dir, architectConfigFileName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v, %v", info, err)
	}
	if got := readHiverynYAML(t, target); !strings.Contains(got, "- demo") {
		t.Fatalf("target = %q", got)
	}
}

func TestReplaceIfUnchangedRefusesAConcurrentEdit(t *testing.T) {
	t.Parallel()

	dir := writeHiverynYAML(t, "name: Edited\n")
	path := filepath.Join(dir, architectConfigFileName)
	err := replaceIfUnchanged(path, []byte("name: Hiveryn\n"), []byte("name: Hiveryn\navailableActions: [demo]\n"), 0o600)
	if !errors.Is(err, ErrArchitectConfigChanged) {
		t.Fatalf("err = %v, want ErrArchitectConfigChanged", err)
	}
	if got := readHiverynYAML(t, dir); got != "name: Edited\n" {
		t.Fatalf("concurrent edit overwritten: %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestAddAvailableActionIsVisibleToTheReloadingSource(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	path := filepath.Join(configDir, configFileName)
	writeYAML(t, path, map[string]any{"port": 4201, "bind_address": "127.0.0.1", "log_level": "info"})
	workspace := writeArchitect(t, map[string]any{"name": "Hiveryn"})
	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]string{"hiveryn": workspace})
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewReloadingSource(path, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := AddAvailableAction(workspace, "hiveryn", "demo"); err != nil {
		t.Fatal(err)
	}
	current, err := source.Current()
	if err != nil || !slices.Equal(current.Architects["hiveryn"].AvailableActions, []string{"demo"}) {
		t.Fatalf("current = %+v, %v; want demo available without a restart", current.Architects["hiveryn"], err)
	}
	if status := source.LoadStatus(); status.Error != "" {
		t.Fatalf("load status = %+v", status)
	}
}
