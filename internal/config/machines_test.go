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
