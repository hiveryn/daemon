package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestActionRegistryLoadsLocalAndRemoteCheckouts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "machines.yaml"), "buildbox:\n  ssh: build-alias\n")
	writeFile(t, filepath.Join(dir, "actions.yaml"), "local-one: ~/checkouts/one\nlocal-two: {path: /abs/two}\nremote:\n  path: /home/me/actions/remote/\n  machine: buildbox\n")
	writeFile(t, filepath.Join(dir, "config.yaml"), "port: 4300\naction_packages:\n  max_bytes: 2048\n  retention_days: 7\n")
	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := map[string]ActionRegistration{
		"local-one": {Path: filepath.Join(home, "checkouts/one")},
		"local-two": {Path: "/abs/two"},
		"remote":    {Path: "/home/me/actions/remote", Machine: "buildbox"},
	}
	if len(cfg.Actions) != len(want) {
		t.Fatalf("actions = %+v", cfg.Actions)
	}
	for name, reg := range want {
		if cfg.Actions[name] != reg {
			t.Fatalf("%s = %+v, want %+v", name, cfg.Actions[name], reg)
		}
	}
	maxBytes, maxFiles, retention := cfg.ActionPackageLimits()
	if maxBytes != 2048 || maxFiles != DefaultActionPackageMaxFiles || retention.Hours() != 7*24 {
		t.Fatalf("limits = %d %d %s", maxBytes, maxFiles, retention)
	}
	if b, f, r := (Config{}).ActionPackageLimits(); b != 100<<20 || f != 10000 || r.Hours() != 30*24 {
		t.Fatalf("defaults = %d %d %s", b, f, r)
	}

	clone := cfg.Clone()
	clone.Actions["remote"] = ActionRegistration{}
	if cfg.Actions["remote"].Machine != "buildbox" {
		t.Fatal("clone aliases the action registry")
	}
}

func TestActionRegistryRejectsInvalidEntries(t *testing.T) {
	for name, registry := range map[string]string{
		"bad name":        "Bad Name: /x\n",
		"blank path":      "demo: \"\"\n",
		"relative remote": "demo: {path: actions/demo, machine: buildbox}\n",
		"tilde remote":    "demo: {path: ~/demo, machine: buildbox}\n",
		"unknown machine": "demo: {path: /demo, machine: elsewhere}\n",
		"unknown field":   "demo: {path: /demo, host: x}\n",
		"negative limit":  "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "machines.yaml"), "buildbox:\n  ssh: build-alias\n")
			config := "port: 4300\n"
			if name == "negative limit" {
				config += "action_packages:\n  max_files: -1\n"
			} else {
				writeFile(t, filepath.Join(dir, "actions.yaml"), registry)
			}
			writeFile(t, filepath.Join(dir, "config.yaml"), config)
			if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil {
				t.Fatalf("accepted %q", registry)
			}
		})
	}
}

func TestBootstrapRegistersExistingCheckoutsOnce(t *testing.T) {
	configDir := t.TempDir()
	library := filepath.Join(t.TempDir(), "actions")
	for _, name := range []string{"demo-evidence", "nova-export", ".hidden", "Odd Name"} {
		if err := os.MkdirAll(filepath.Join(library, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(library, "README.md"), "not an action")

	names, err := BootstrapActionRegistry(configDir, library)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "demo-evidence,nova-export" {
		t.Fatalf("registered %v", names)
	}
	data, err := os.ReadFile(ActionsRegistryPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	// An invalid directory name stays visible as a comment rather than
	// breaking the registry.
	if !strings.Contains(string(data), `# not registered: "Odd Name"`) {
		t.Fatalf("registry does not mention the skipped directory:\n%s", data)
	}
	registered, err := loadActions(ActionsRegistryPath(configDir))
	if err != nil || registered["demo-evidence"].Path != filepath.Join(library, "demo-evidence") || registered["nova-export"].Machine != "" || len(registered) != 2 {
		t.Fatalf("registry = %+v, %v\n%s", registered, err, data)
	}

	// An existing registry is never rewritten.
	edited := "demo-evidence: /elsewhere\n"
	writeFile(t, ActionsRegistryPath(configDir), edited)
	if names, err := BootstrapActionRegistry(configDir, library); err != nil || names != nil {
		t.Fatalf("second bootstrap = %v, %v", names, err)
	}
	if data, _ := os.ReadFile(ActionsRegistryPath(configDir)); string(data) != edited {
		t.Fatalf("registry rewritten: %s", data)
	}

	// Nothing to register: no registry is created.
	empty := t.TempDir()
	if names, err := BootstrapActionRegistry(empty, filepath.Join(empty, "absent")); err != nil || names != nil {
		t.Fatalf("empty bootstrap = %v, %v", names, err)
	}
	if _, err := os.Stat(ActionsRegistryPath(empty)); !os.IsNotExist(err) {
		t.Fatalf("registry created for an empty library: %v", err)
	}
}
