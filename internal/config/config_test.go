package config

import (
	"path/filepath"
	"testing"
)

func TestLoadReturnsDefaultsWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg != Default() {
		t.Fatalf("expected default config, got %#v", cfg)
	}
}

func TestSaveAndLoadYAML(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.yaml")
	input := Config{
		Port:        4312,
		BindAddress: "127.0.0.1",
		LogLevel:    "debug",
	}

	if err := input.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loaded != input {
		t.Fatalf("expected %#v, got %#v", input, loaded)
	}
}

func TestSaveAndLoadJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "daemon.json")
	input := Config{
		Port:        4313,
		BindAddress: "localhost",
		LogLevel:    "warn",
	}

	if err := input.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loaded != input {
		t.Fatalf("expected %#v, got %#v", input, loaded)
	}
}

func TestValidateRejectsNonLocalBindAddress(t *testing.T) {
	t.Parallel()

	err := Config{Port: DefaultPort, BindAddress: "0.0.0.0", LogLevel: DefaultLogLevel}.Validate()
	if err == nil {
		t.Fatal("expected bind_address validation error")
	}
}
