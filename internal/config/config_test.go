package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadCreatesDefaultConfigWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("expected default config, got %#v", cfg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected generated config file to be non-empty")
	}
}

func TestSaveAndLoadYAML(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	input := Config{
		Port:        4312,
		BindAddress: "127.0.0.1",
		LogLevel:    "debug",
		AgentProfiles: map[string]AgentProfileConfig{
			"codex-personal": {
				Agent: "codex",
				Args:  []string{"--dangerously-bypass-approvals-and-sandbox"},
				Env: map[string]string{
					"CODEX_HOME": "/Users/kareem/.codex-personal",
				},
			},
		},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Path:  "/Users/kareem/architects/hiveryn",
				Group: "personal",
				Repos: map[string]string{
					"daemon": "/Users/kareem/hiveryn/daemon",
				},
			},
		},
	}

	if err := input.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if !reflect.DeepEqual(loaded, input) {
		t.Fatalf("expected %#v, got %#v", input, loaded)
	}
}

func TestValidateRejectsNonLocalBindAddress(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:          DefaultPort,
		BindAddress:   "0.0.0.0",
		LogLevel:      DefaultLogLevel,
		AgentProfiles: map[string]AgentProfileConfig{},
		Architects:    map[string]ArchitectConfig{},
	}.Validate()
	if err == nil {
		t.Fatal("expected bind_address validation error")
	}
}

func TestValidateRejectsBlankArchitectGroup(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:          DefaultPort,
		BindAddress:   DefaultBindAddress,
		LogLevel:      DefaultLogLevel,
		AgentProfiles: map[string]AgentProfileConfig{},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Path:  "/Users/kareem/architects/hiveryn",
				Group: "",
				Repos: map[string]string{},
			},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected architect group validation error")
	}
}
