package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadCreatesDefaultConfigWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	expected := Default()
	if !reflect.DeepEqual(cfg, expected) {
		t.Fatalf("expected %#v, got %#v", expected, cfg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected generated config file to be non-empty")
	}
}

func TestSaveAndLoadCoreConfig(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	path := filepath.Join(configDir, configFileName)
	input := Config{
		Port:                      4312,
		BindAddress:               "127.0.0.1",
		LogLevel:                  "debug",
		Shell:                     "/bin/zsh",
		DesktopHealthPollInterval: "2500ms",
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
	}

	if err := input.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loaded.Port != input.Port || loaded.BindAddress != input.BindAddress || loaded.LogLevel != input.LogLevel || loaded.Shell != input.Shell || loaded.DesktopHealthPollInterval != input.DesktopHealthPollInterval {
		t.Fatalf("core fields mismatch: expected port=%d addr=%s level=%s shell=%s interval=%s, got port=%d addr=%s level=%s shell=%s interval=%s",
			input.Port, input.BindAddress, input.LogLevel, input.Shell, input.DesktopHealthPollInterval,
			loaded.Port, loaded.BindAddress, loaded.LogLevel, loaded.Shell, loaded.DesktopHealthPollInterval)
	}
}

func TestLoadAllFiles(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()

	writeYAML(t, filepath.Join(configDir, configFileName), map[string]interface{}{
		"port":                         4201,
		"bind_address":                 "127.0.0.1",
		"log_level":                    "info",
		"shell":                        "/bin/zsh",
		"desktop_health_poll_interval": "3s",
	})

	writeYAML(t, filepath.Join(configDir, variantsFileName), map[string]VariantConfig{
		"codex-personal": {
			Agent: "codex",
			Args:  []string{"--dangerously-bypass-approvals-and-sandbox"},
			Env:   map[string]string{"CODEX_HOME": "/Users/kareem/.codex-personal"},
		},
	})

	writeYAML(t, filepath.Join(configDir, architectsFileName), map[string]ArchitectConfig{
		"hiveryn": {
			Path:  "/Users/kareem/architects/hiveryn",
			Group: "personal",
			Repos: map[string]string{"daemon": "/Users/kareem/hiveryn/daemon"},
		},
	})

	writeYAML(t, filepath.Join(configDir, tabsFileName), map[string][]TabEntry{
		"architect": {
			{Type: "kanban"},
			{Type: "terminal", Command: "yazi"},
		},
	})

	cfg, err := Load(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(cfg.Variants) != 1 || cfg.Variants["codex-personal"].Agent != "codex" {
		t.Fatalf("expected 1 variant, got %d", len(cfg.Variants))
	}
	if len(cfg.Architects) != 1 || cfg.Architects["hiveryn"].Group != "personal" {
		t.Fatalf("expected 1 architect, got %d", len(cfg.Architects))
	}
	if len(cfg.Tabs) != 1 || len(cfg.Tabs["architect"]) != 2 {
		t.Fatalf("expected 2 tabs for architect, got %d", len(cfg.Tabs["architect"]))
	}
	if cfg.Shell != "/bin/zsh" {
		t.Fatalf("expected shell to load, got %q", cfg.Shell)
	}
	if cfg.DesktopHealthPollInterval != "3s" {
		t.Fatalf("expected desktop health poll interval to load, got %q", cfg.DesktopHealthPollInterval)
	}
}

func TestLoadMissingOptionalFiles(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()

	writeYAML(t, filepath.Join(configDir, configFileName), map[string]interface{}{
		"port":         4201,
		"bind_address": "127.0.0.1",
		"log_level":    "info",
	})

	cfg, err := Load(filepath.Join(configDir, configFileName))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(cfg.Variants) != 0 {
		t.Fatalf("expected empty variants when file missing, got %d", len(cfg.Variants))
	}
	if len(cfg.Architects) != 0 {
		t.Fatalf("expected empty architects when file missing, got %d", len(cfg.Architects))
	}
	if len(cfg.Tabs) != 0 {
		t.Fatalf("expected empty tabs when file missing, got %d", len(cfg.Tabs))
	}
}

func TestValidateRejectsInvalidDesktopHealthPollInterval(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:                      DefaultPort,
		BindAddress:               DefaultBindAddress,
		LogLevel:                  DefaultLogLevel,
		DesktopHealthPollInterval: "nope",
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected desktop health poll interval validation error")
	}
}

func TestValidateRejectsNonPositiveDesktopHealthPollInterval(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:                      DefaultPort,
		BindAddress:               DefaultBindAddress,
		LogLevel:                  DefaultLogLevel,
		DesktopHealthPollInterval: "0s",
		Variants:                  map[string]VariantConfig{},
		Architects:                map[string]ArchitectConfig{},
		Tabs:                      map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected non-positive desktop health poll interval validation error")
	}
}

func TestValidateRejectsNonLocalBindAddress(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: "0.0.0.0",
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs:        map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected bind_address validation error")
	}
}

func TestValidateRejectsBlankArchitectGroup(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects: map[string]ArchitectConfig{
			"hiveryn": {
				Path:  "/Users/kareem/architects/hiveryn",
				Group: "",
				Repos: map[string]string{},
			},
		},
		Tabs: map[string][]TabEntry{},
	}.Validate()
	if err == nil {
		t.Fatal("expected architect group validation error")
	}
}

func TestValidateRejectsInvalidTabType(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"architect": {
				{Type: "invalid"},
			},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected tab type validation error")
	}
}

func TestValidateAllowsUnnamedTerminalTabs(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"worker": {
				{Type: "terminal"},
			},
		},
	}.Validate()
	if err != nil {
		t.Fatalf("expected unnamed terminal tabs to be valid, got %v", err)
	}
}

func TestValidateAllowsDuplicateTerminalCommands(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"architect": {
				{Type: "terminal", Command: "yazi"},
				{Type: "terminal", Command: "yazi"},
			},
		},
	}.Validate()
	if err != nil {
		t.Fatalf("expected duplicate terminal commands to be valid, got %v", err)
	}
}

func TestValidateRejectsNonTerminalWithCommand(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"architect": {
				{Type: "event-log", Command: "ls"},
			},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected non-terminal command rejection error")
	}
}

func TestValidateRejectsBlankTabSessionType(t *testing.T) {
	t.Parallel()

	err := Config{
		Port:        DefaultPort,
		BindAddress: DefaultBindAddress,
		LogLevel:    DefaultLogLevel,
		Variants:    map[string]VariantConfig{},
		Architects:  map[string]ArchitectConfig{},
		Tabs: map[string][]TabEntry{
			"": {},
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected blank tab session type validation error")
	}
}

func writeYAML(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %q: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
