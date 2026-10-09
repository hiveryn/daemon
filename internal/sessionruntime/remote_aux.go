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
	RemoteDirectories(context.Context, string) (map[string][]string, error)
	RemoteResources(context.Context, string) (map[string][]string, error)
}

func (s *Service) prepareRemoteAux(ctx context.Context, session domain.Session, workdir domain.TerminalWorkdir, spec *terminalStartSpec) error {
	cfg, err := s.currentConfig()
	if err != nil {
		return err
	}
	machine, ok := cfg.Machines[workdir.Machine]
	if !ok {
		return fmt.Errorf("unknown machine %q", workdir.Machine)
	}
	if session.Machine == workdir.Machine {
		machine.SSH = session.SSH
	}
	resources, ok := s.repo.(remoteResourceStore)
	if !ok {
		return fmt.Errorf("remote resource persistence unavailable")
	}
	socket := "hiveryn-aux-" + spec.TerminalID
	if err := resources.AddRemoteResource(ctx, session.ID, machine.SSH, socket, ""); err != nil {
		return err
	}
	args := []string{"tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "terminal", "-c", workdir.Path}
	if spec.Command != "" {
		args = append(args, spec.Command)
	}
	command := remoteexec.Args(args...) + " && " + remoteexec.Args("tmux", "-L", socket, "set-option", "-t", "terminal", "status", "off")
	if _, err := remoteexec.Run(ctx, machine.SSH, command, nil); err != nil {
		return fmt.Errorf("create remote repository terminal: %w", err)
	}
	s.configureRemoteAuxAttachment(session, machine.SSH, socket, spec)
	return nil
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
			s.appendSessionTab(session.ID, sessionTabState{tab: domain.SessionTab{Type: "terminal", ID: id, Command: "ssh", Status: "running", Placement: domain.TerminalPlacementTab, WorkdirTitle: alias}, removeOnExit: true})
			if err := s.terminal.Start(ctx, spec); err != nil {
				s.logger.Error("restore remote repository terminal", "terminal_id", id, "error", err)
				spec.OnExit(terminalExit{SessionID: session.ID, TerminalID: id})
			}
		}
	}
}
