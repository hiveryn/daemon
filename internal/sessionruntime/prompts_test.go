package sessionruntime

import (
	"testing"

	"github.com/hiveryn/daemon/internal/config"
)

func TestSelectTicketKickoff(t *testing.T) {
	t.Parallel()

	architect := config.ArchitectConfig{
		TicketKickoffs: []config.TicketKickoff{
			{Path: "/tmp/default.md"},
			{Path: "/tmp/daemon.md", Repos: []string{"daemon", "shared"}},
		},
	}

	tests := []struct {
		name string
		repo string
		want string
	}{
		{name: "repo-scoped wins", repo: "daemon", want: "/tmp/daemon.md"},
		{name: "repo-scoped matches second key", repo: "shared", want: "/tmp/daemon.md"},
		{name: "falls back to default", repo: "desktop", want: "/tmp/default.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectTicketKickoff(architect, tt.repo); got != tt.want {
				t.Fatalf("selectTicketKickoff(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

func TestSelectTicketKickoffEmptyUsesEmbeddedDefault(t *testing.T) {
	t.Parallel()

	// No configured kickoffs => "" so the embedded default is used.
	if got := selectTicketKickoff(config.ArchitectConfig{}, "daemon"); got != "" {
		t.Fatalf("expected empty selection, got %q", got)
	}

	// Only repo-scoped entries and no match => "" (embedded default).
	architect := config.ArchitectConfig{
		TicketKickoffs: []config.TicketKickoff{
			{Path: "/tmp/daemon.md", Repos: []string{"daemon"}},
		},
	}
	if got := selectTicketKickoff(architect, "desktop"); got != "" {
		t.Fatalf("expected empty selection for unmatched repo, got %q", got)
	}
}
