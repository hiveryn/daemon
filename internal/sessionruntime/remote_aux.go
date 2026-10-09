package sessionruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

type remoteResourceStore interface {
	AddRemoteResource(context.Context, string, string, string, string) error
	RemoveRemoteResource(context.Context, string, string, string) error
	RemoteDirectories(context.Context, string) (map[string][]string, error)
	RemoteResources(context.Context, string) (map[string][]string, error)
}

// remoteAuxTerminal names the owned tmux server behind one remote terminal.
type remoteAuxTerminal struct {
	alias  string
	socket string
}

// prepareRemoteAux creates the remote tmux server for an auxiliary terminal and
// points spec at it. Ownership is recorded before the server is created, so a
// daemon crash mid-creation still leaves it to session cleanup. If creation
// fails it is discarded here: an SSH command that failed or was cut off does
// not prove the server was never created.
func (s *Service) prepareRemoteAux(ctx context.Context, session domain.Session, workdir domain.TerminalWorkdir, spec *terminalStartSpec) (*remoteAuxTerminal, error) {
	cfg, err := s.currentConfig()
	if err != nil {
		return nil, err
	}
	machine, ok := cfg.Machines[workdir.Machine]
	if !ok {
		return nil, fmt.Errorf("unknown machine %q", workdir.Machine)
	}
	if session.Machine == workdir.Machine {
		machine.SSH = session.SSH
	}
	resources, ok := s.repo.(remoteResourceStore)
	if !ok {
		return nil, fmt.Errorf("remote resource persistence unavailable")
	}
	remote := remoteAuxTerminal{alias: machine.SSH, socket: "hiveryn-aux-" + spec.TerminalID}
	if err := resources.AddRemoteResource(ctx, session.ID, remote.alias, remote.socket, ""); err != nil {
		return nil, err
	}
	args := []string{"tmux", "-L", remote.socket, "-f", "/dev/null", "new-session", "-d", "-s", "terminal", "-c", workdir.Path}
	if spec.Command != "" {
		args = append(args, spec.Command)
	}
	command := remoteexec.Args(args...) + " && " + remoteexec.Args("tmux", "-L", remote.socket, "set-option", "-t", "terminal", "status", "off")
	if _, err := remoteexec.Run(ctx, remote.alias, command, nil); err != nil {
		return nil, s.discardRemoteAux(session.ID, remote, fmt.Errorf("create remote repository terminal on machine %s: %w", workdir.Machine, err))
	}
	s.configureRemoteAuxAttachment(session, remote.alias, remote.socket, spec)
	return &remote, nil
}

// discardRemoteAux removes the tmux server of a terminal whose creation failed
// and returns cause, annotated when that removal is not confirmed. Cleanup runs
// on a fresh bounded context because the creation's own may be exhausted.
// Ownership is dropped only after a confirmed kill; otherwise it stays recorded
// so session teardown retries it, and a daemon restart shows it as a tab rather
// than leaving a shell nobody can see.
func (s *Service) discardRemoteAux(sessionID string, remote remoteAuxTerminal, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCleanupTimeout)
	defer cancel()
	if _, err := remoteexec.Run(ctx, remote.alias, killRemoteSocketScript(remote.socket), nil); err != nil {
		s.logger.Error("cleanup of failed remote terminal not confirmed", "session_id", sessionID, "socket", remote.socket, "error", err)
		return fmt.Errorf("%w; removal of the possibly created remote terminal was not confirmed (%v) — it stays recorded and is removed when the session ends", cause, err)
	}
	if store, ok := s.repo.(remoteResourceStore); ok {
		if err := store.RemoveRemoteResource(ctx, sessionID, remote.alias, remote.socket); err != nil {
			s.logger.Error("drop ownership of removed remote terminal", "session_id", sessionID, "socket", remote.socket, "error", err)
		}
	}
	return cause
}

func (s *Service) configureRemoteAuxAttachment(session domain.Session, alias, socket string, spec *terminalStartSpec) {
	spec.Command = "ssh"
	spec.Workdir = ""
	spec.Args = append([]string{"-tt"}, remoteexec.Options(alias)...)
	spec.Args = append(spec.Args, remoteexec.Args("tmux", "-L", socket, "attach-session", "-t", "terminal"))
	// Reattach the same terminal identity after transport loss; never create a
	// second shell. A confirmed missing tmux terminal is reported as exited.
	copySpec := *spec
	spec.OnExit = func(exit terminalExit) {
		go func() {
			for {
				select {
				case <-s.remoteStop:
					return
				case <-time.After(3 * time.Second):
				}
				found := false
				for _, tab := range s.sessionTabs(session.ID) {
					if tab.ID == spec.TerminalID {
						found = true
					}
				}
				if !found {
					return
				}
				live, err := s.repo.GetSession(context.Background(), session.ID)
				if err != nil || live.CurrentRun == nil || live.CurrentRun.Status != domain.SessionRunStatusRunning {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				state, checkErr := remoteexec.Run(ctx, alias, "if "+remoteexec.Args("tmux", "-L", socket, "has-session", "-t", "terminal")+" 2>/dev/null; then printf present; else printf missing; fi", nil)
				err = checkErr
				cancel()
				if err != nil {
					s.logger.Warn("remote repository terminal unavailable", "terminal_id", spec.TerminalID, "error", err)
					continue
				}
				if string(state) == "missing" {
					s.handleAuxTerminalExit(exit)
					return
				}
				copySpec.OnExit = spec.OnExit
				if err := s.terminal.Start(context.Background(), copySpec); err != nil {
					s.logger.Error("reattach remote repository terminal", "error", err)
					continue
				}
				return
			}
		}()
	}
}

func (s *Service) stopRemoteResources(ctx context.Context, session domain.Session) error {
	resources := map[string][]string{}
	if store, ok := s.repo.(remoteResourceStore); ok {
		var err error
		resources, err = store.RemoteResources(ctx, session.ID)
		if err != nil {
			return err
		}
	}
	if session.Machine != "" {
		found := false
		for _, socket := range resources[session.SSH] {
			if socket == remoteSocket(session.ID) {
				found = true
			}
		}
		if !found {
			resources[session.SSH] = append(resources[session.SSH], remoteSocket(session.ID))
		}
	}
	for alias, sockets := range resources {
		for _, socket := range sockets {
			// A missing socket means no owned tmux server can remain. Any SSH failure
			// or other kill failure keeps the local session and ownership record.
			script := killRemoteSocketScript(socket)
			if _, err := remoteexec.Run(ctx, alias, script, nil); err != nil {
				return fmt.Errorf("remote termination not confirmed on %s; session retained: %w", alias, err)
			}
		}
	}
	if store, ok := s.repo.(remoteResourceStore); ok {
		dirs, err := store.RemoteDirectories(ctx, session.ID)
		if err != nil {
			return err
		}
		for alias, paths := range dirs {
			for _, dir := range paths {
				script := "dir=" + remoteexec.Quote(dir) + "; if test -d \"$dir\"; then rm -f \"$dir\"/* && rmdir \"$dir\"; fi"
				if _, err := remoteexec.Run(ctx, alias, script, nil); err != nil {
					return fmt.Errorf("remote runtime cleanup: %w", err)
				}
			}
		}
	}
	s.closeRemote(session.ID)
	return nil
}

func (s *Service) stopRemoteAux(ctx context.Context, sessionID, terminalID string) error {
	store, ok := s.repo.(remoteResourceStore)
	if !ok {
		return nil
	}
	resources, err := store.RemoteResources(ctx, sessionID)
	if err != nil {
		return err
	}
	for alias, sockets := range resources {
		for _, socket := range sockets {
			if socket == "hiveryn-aux-"+terminalID {
				if _, err := remoteexec.Run(ctx, alias, killRemoteSocketScript(socket), nil); err != nil {
					return fmt.Errorf("remote terminal termination not confirmed: %w", err)
				}
				// Confirmed gone: a restart must not restore it as a tab.
				if err := store.RemoveRemoteResource(ctx, sessionID, alias, socket); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func killRemoteSocketScript(socket string) string {
	return "export LC_ALL=C; if output=$(" + remoteexec.Args("tmux", "-L", socket, "kill-server") + " 2>&1); then exit 0; fi; case \"$output\" in 'no server running on '*|'error connecting to '*'(No such file or directory)') exit 0;; *) printf '%s\\n' \"$output\" >&2; exit 1;; esac"
}

func (s *Service) restoreRemoteAux(ctx context.Context, session domain.Session) {
	store, ok := s.repo.(remoteResourceStore)
	if !ok {
		return
	}
	resources, err := store.RemoteResources(ctx, session.ID)
	if err != nil {
		s.logger.Error("read remote terminal ownership", "error", err)
		return
	}
	for alias, sockets := range resources {
		for _, socket := range sockets {
			if !strings.HasPrefix(socket, "hiveryn-aux-") {
				continue
			}
			id := strings.TrimPrefix(socket, "hiveryn-aux-")
			spec := terminalStartSpec{SessionID: session.ID, TerminalID: id, Size: terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows}}
			s.configureRemoteAuxAttachment(session, alias, socket, &spec)
			s.appendSessionTab(session.ID, sessionTabState{tab: domain.SessionTab{Type: "terminal", ID: id, Command: "ssh", Status: "running", WorkdirTitle: alias}, removeOnExit: true})
			if err := s.terminal.Start(ctx, spec); err != nil {
				s.logger.Error("restore remote repository terminal", "terminal_id", id, "error", err)
				spec.OnExit(terminalExit{SessionID: session.ID, TerminalID: id})
			}
		}
	}
}
