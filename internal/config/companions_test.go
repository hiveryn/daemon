package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompanionConfiguration(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, companions string) (ArchitectConfig, error) {
		t.Helper()
		dir := t.TempDir()
		content := "name: Test\nrepos:\n  local: /tmp/local\n  remote:\n    path: /home/other/repo\n    machine: buildbox\n" + companions
		if err := os.WriteFile(filepath.Join(dir, "hiveryn.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return ValidateArchitectConfig(dir, "test")
	}
	a, err := write(t, "companions:\n  browser:\n    repo: local\n    variant: claude-opus\n  far:\n    repo: remote\n    variant: bb-codex\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.Companions["browser"] != (CompanionConfig{Repo: "local", Variant: "claude-opus"}) || a.Companions["far"].Repo != "remote" {
		t.Fatalf("companions = %+v", a.Companions)
	}
	if a, err := write(t, ""); err != nil || len(a.Companions) != 0 {
		t.Fatalf("omitted companions = %+v %v", a.Companions, err)
	}
	for companions, want := range map[string]string{
		"companions:\n  b:\n    repo: nowhere\n    variant: v\n":         `repo "nowhere" is not a key of repos`,
		"companions:\n  b:\n    variant: v\n":                            "companions.b.repo is required",
		"companions:\n  b:\n    repo: local\n":                           "companions.b.variant is required",
		"companions:\n  bad name:\n    repo: local\n    variant: v\n":    "companion names must start",
		"companions:\n  b:\n    repo: local\n    variant: v\n    x: 1\n": "field x not found",
	} {
		if _, err := write(t, companions); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err = %v, want %q", companions, err, want)
		}
	}

	cfg := Config{Variants: map[string]VariantConfig{
		"claude-opus": {Agent: "claude"},
		"bb-codex":    {Agent: "codex", Machine: "buildbox"},
		"oc":          {Agent: "opencode"},
	}}
	if v, err := cfg.CompanionVariant("browser", "claude-opus", ""); err != nil || v.Agent != "claude" {
		t.Fatalf("local claude = %+v %v", v, err)
	}
	if v, err := cfg.CompanionVariant("far", "bb-codex", "buildbox"); err != nil || v.Agent != "codex" {
		t.Fatalf("remote codex = %+v %v", v, err)
	}
	if _, err := cfg.CompanionVariant("far", "claude-opus", "buildbox"); err == nil || !strings.Contains(err.Error(), "machine buildbox") {
		t.Fatalf("local variant on buildbox = %v", err)
	}
	if _, err := cfg.CompanionVariant("b", "oc", ""); err == nil || !strings.Contains(err.Error(), "claude and codex variants only") {
		t.Fatalf("opencode companion = %v", err)
	}
	if _, err := cfg.CompanionVariant("b", "nope", ""); err == nil {
		t.Fatal("unknown variant accepted")
	}
}
