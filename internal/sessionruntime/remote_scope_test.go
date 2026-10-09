package sessionruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestRemoteLaunchRechecksMachineScope(t *testing.T) {
	t.Parallel()
	a := config.ArchitectConfig{Repos: map[string]string{"primary": "/remote/a", "extra": "/remote/b"}, RepoMachines: map[string]string{"primary": "box", "extra": "other"}}
	cfg := config.Config{Machines: map[string]config.MachineConfig{"box": {SSH: "original"}, "other": {SSH: "other"}}}
	ticket := domain.Ticket{}
	ticket.Repo = "primary"
	ticket.AdditionalRepos = []string{"extra"}
	tickets := &fakeTicketService{ticket: ticket}
	s := &Service{tickets: tickets}
	session := domain.Session{SessionType: domain.SessionTypeTicket, Machine: "box", SSH: "original"}
	if err := s.validateSessionLocation(context.Background(), cfg, a, session); err == nil || !strings.Contains(err.Error(), "one machine") {
		t.Fatalf("cross-machine launch: %v", err)
	}
	a.RepoMachines["extra"] = "box"
	cfg.Machines["box"] = config.MachineConfig{SSH: "changed"}
	if err := s.validateSessionLocation(context.Background(), cfg, a, session); err == nil || !strings.Contains(err.Error(), "changed since session creation") {
		t.Fatalf("changed target: %v", err)
	}
	delete(cfg.Machines, "box")
	if err := s.validateSessionLocation(context.Background(), cfg, a, session); err == nil || !strings.Contains(err.Error(), "unknown machine") {
		t.Fatalf("removed target: %v", err)
	}
}
