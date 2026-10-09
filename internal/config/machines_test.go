package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryLocations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "name: Test\nrepos:\n  local: ~/local\n  remote:\n    path: /home/other/repo\n    machine: buildbox\n"
	if err := os.WriteFile(filepath.Join(dir, "hiveryn.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := ValidateArchitectConfig(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	if a.Repos["remote"] != "/home/other/repo" || a.RepoMachines["remote"] != "buildbox" {
		t.Fatal(a)
	}
	cfg := Config{Machines: map[string]MachineConfig{"buildbox": {SSH: "my-alias"}}}
	if _, err := cfg.ScopeMachine(a, "local", []string{"remote"}); err == nil {
		t.Fatal("cross-machine scope accepted")
	}
	if machine, err := cfg.ScopeMachine(a, "remote", nil); err != nil || machine != "buildbox" {
		t.Fatalf("%s %v", machine, err)
	}
	delete(cfg.Machines, "buildbox")
	if _, err := cfg.ScopeMachine(a, "remote", nil); err == nil {
		t.Fatal("unknown machine accepted")
	}
	for _, entry := range []string{"{path: ~/remote, machine: buildbox}", "{path: /repo, machine: buildbox, typo: true}", "{path: /repo, path: /other}"} {
		_, err := decodeArchitectFile([]byte("name: Test\nrepos:\n  r: " + entry + "\n"))
		if err == nil {
			f, _ := decodeArchitectFile([]byte("name: Test\nrepos:\n  r: " + entry + "\n"))
			_, err = architectConfigFromFile("test", dir, f)
		}
		if err == nil {
			t.Fatalf("accepted %s", entry)
		}
	}
}
func TestMachineValidationAndClone(t *testing.T) {
	t.Parallel()
	for _, alias := range []string{"", "-oProxyCommand=bad", "user@host", "host name", "host\nother"} {
		c := Config{Machines: map[string]MachineConfig{"box": {SSH: alias}}}
		if c.validateMachines() == nil {
			t.Fatalf("accepted alias %q", alias)
		}
	}
	c := Config{Machines: map[string]MachineConfig{"box": {SSH: "box"}}, Architects: map[string]ArchitectConfig{"a": {Repos: map[string]string{"r": "/r"}, RepoMachines: map[string]string{"r": "box"}}}}
	copy := c.Clone()
	copy.Machines["box"] = MachineConfig{SSH: "other"}
	copy.Architects["a"].RepoMachines["r"] = "other"
	if c.Machines["box"].SSH != "box" || c.Architects["a"].RepoMachines["r"] != "box" {
		t.Fatal("clone aliases location maps")
	}
	c.Architects["a"].RepoMachines["r"] = "missing"
	if err := c.validateMachines(); err == nil || !strings.Contains(err.Error(), "unknown machine") {
		t.Fatal(err)
	}
}

func TestVariantMachines(t *testing.T) {
	t.Parallel()
	c := Config{
		Machines: map[string]MachineConfig{"bk": {SSH: "bk"}},
		Variants: map[string]VariantConfig{
			"local-claude": {Agent: "claude"},
			"bk-codex":     {Agent: "codex", Machine: "bk"},
			"bk-claude":    {Agent: "claude", Machine: "bk"},
		},
	}
	if err := c.validateMachines(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.VariantsForMachine("bk"), ","); got != "bk-claude,bk-codex" {
		t.Fatal(got)
	}
	if got := strings.Join(c.VariantsForMachine(""), ","); got != "local-claude" {
		t.Fatalf("unassigned variants must be local only: %s", got)
	}
	if v, err := c.VariantForMachine("bk-codex", "bk", "worker"); err != nil || v.Machine != "bk" {
		t.Fatal(v, err)
	}
	for _, tc := range []struct {
		name, machine string
		want          []string
	}{
		{"local-claude", "bk", []string{`"local-claude" is configured for local`, "runs on machine bk", "bk-claude (claude), bk-codex (codex)"}},
		{"bk-codex", "", []string{"configured for machine bk", "runs on local", "local-claude (claude)"}},
		{"nope", "bk", []string{"not a configured agent variant", "machine bk"}},
		{"", "", []string{"is required", "local"}},
		{"bk-codex", "other", []string{"No agent variant is configured for machine other", "machine: other"}},
	} {
		_, err := c.VariantForMachine(tc.name, tc.machine, "worker")
		if err == nil {
			t.Fatalf("%s on %q accepted", tc.name, tc.machine)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s on %q: %v lacks %q", tc.name, tc.machine, err, want)
			}
		}
	}
	if c.Clone().Variants["bk-codex"].Machine != "bk" {
		t.Fatal("clone drops variant machine")
	}
	c.Variants["ghost"] = VariantConfig{Agent: "codex", Machine: "missing"}
	if err := c.validateMachines(); err == nil || !strings.Contains(err.Error(), "variants.ghost.machine") {
		t.Fatal(err)
	}
}
