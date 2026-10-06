package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNotificationsNtfy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("notifications:\n  ntfy:\n    server: https://ntfy.sh/\n    topic: kareem-hiveryn\n    token: tk_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ntfy := cfg.Notifications.Ntfy
	if ntfy == nil || ntfy.Server != "https://ntfy.sh" || ntfy.Topic != "kareem-hiveryn" || ntfy.Token != "tk_secret" {
		t.Fatalf("ntfy = %+v", ntfy)
	}
	clone := cfg.Clone()
	clone.Notifications.Ntfy.Topic = "changed"
	if cfg.Notifications.Ntfy.Topic != "kareem-hiveryn" {
		t.Fatal("Clone shares the ntfy config")
	}
}

func TestNotificationsValidation(t *testing.T) {
	cases := []struct {
		name string
		ntfy NtfyConfig
		want string
	}{
		{"missing server", NtfyConfig{Topic: "t"}, "server is required"},
		{"bad scheme", NtfyConfig{Server: "ftp://x", Topic: "t"}, "absolute http(s) URL"},
		{"embedded credentials", NtfyConfig{Server: "https://user:pw_secret@ntfy.sh", Topic: "t"}, "must not embed credentials"},
		{"embedded credentials, bad scheme", NtfyConfig{Server: "ftp://user:pw_secret@ntfy.sh", Topic: "t"}, "must not embed credentials"},
		{"auth query", NtfyConfig{Server: "https://ntfy.sh?auth=secret", Topic: "t"}, "query"},
		{"missing topic", NtfyConfig{Server: "https://ntfy.sh"}, "topic is required"},
		{"bad topic", NtfyConfig{Server: "https://ntfy.sh", Topic: "a/b"}, "topic must be"},
		{"token and basic", NtfyConfig{Server: "https://ntfy.sh", Topic: "t", Token: "tk_secret", Username: "u", Password: "pw_secret"}, "not both"},
		{"username without password", NtfyConfig{Server: "https://ntfy.sh", Topic: "t", Username: "u"}, "together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			ntfy := tc.ntfy
			cfg.Notifications = &NotificationsConfig{Ntfy: &ntfy}
			cfg.normalize()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation error leaks a credential: %v", err)
			}
		})
	}
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config without notifications must stay valid: %v", err)
	}
}
