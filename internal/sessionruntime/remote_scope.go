package sessionruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

func newRemoteCredentials(machine string) (string, int, error) {
	if machine == "" {
		return "", 0, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", 0, err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(40000))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(b), 20000 + int(n.Int64()), nil
}
func (s *Service) validateSessionLocation(ctx context.Context, cfg config.Config, a config.ArchitectConfig, session domain.Session) error {
	if session.SessionType != domain.SessionTypeTicket {
		return nil
	}
	hasRemote := session.Machine != ""
	for _, machine := range a.RepoMachines {
		if machine != "" {
			hasRemote = true
		}
	}
	if !hasRemote {
		return nil
	}
	ticket, err := s.tickets.GetTicket(ctx, a.Path, session.ContextID)
	if err != nil {
		return err
	}
	machine, err := cfg.ScopeMachine(a, ticket.Repo, ticket.AdditionalRepos)
	if err != nil {
		return &domain.ValidationError{Field: "repos", Message: err.Error()}
	}
	if machine != session.Machine || (machine != "" && cfg.Machines[machine].SSH != session.SSH) {
		return &domain.ValidationError{Field: "machine", Message: "repository execution location changed since session creation; discard and recreate the session"}
	}
	if machine != "" {
		for _, p := range append([]string{session.Workdir}, session.AdditionalWorkdirs...) {
			if _, err := remoteexec.Run(ctx, session.SSH, "test -d "+remoteexec.Quote(p), nil); err != nil {
				return fmt.Errorf("remote repository %s: %w", p, err)
			}
		}
	}
	return nil
}
