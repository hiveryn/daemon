package sessionruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"

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
	return nil
}

// checkRemoteWorkdirs confirms every repository directory of a remote session
// in one SSH command, naming each path that is missing or not a directory. A
// failed SSH command is reported as such, never as a missing repository.
func checkRemoteWorkdirs(ctx context.Context, session domain.Session) error {
	paths := append([]string{session.Workdir}, session.AdditionalWorkdirs...)
	var script strings.Builder
	for i, p := range paths {
		fmt.Fprintf(&script, "if test -d %s; then :; elif test -e %s; then echo 'file %d'; else echo 'missing %d'; fi\n", remoteexec.Quote(p), remoteexec.Quote(p), i, i)
	}
	out, err := remoteexec.Run(ctx, session.SSH, script.String(), nil)
	if err != nil {
		return fmt.Errorf("check remote repositories on %s: %w", session.Machine, err)
	}
	var problems []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		kind, index, _ := strings.Cut(line, " ")
		i, err := strconv.Atoi(index)
		if err != nil || i < 0 || i >= len(paths) || (kind != "file" && kind != "missing") {
			return fmt.Errorf("check remote repositories on %s: unexpected response %q", session.Machine, line)
		}
		if kind == "file" {
			problems = append(problems, "path is not a directory: "+paths[i])
		} else {
			problems = append(problems, "path does not exist: "+paths[i])
		}
	}
	if len(problems) > 0 {
		return &domain.ValidationError{Field: "workdir", Message: "remote repository on machine " + session.Machine + ": " + strings.Join(problems, "; ")}
	}
	return nil
}
