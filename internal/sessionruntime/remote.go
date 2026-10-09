package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

type remoteMissingError struct{ message string }

func (e *remoteMissingError) Error() string { return e.message }

type remoteWorker struct {
	mu           sync.Mutex
	session      domain.Session
	run          domain.SessionRun
	gateway      *http.Server
	port         int
	connection   string
	closed       bool
	reconnecting bool
}

func remoteSocket(sessionID string) string { return "hiveryn-" + sessionID }
func tmux(sessionID string, args ...string) string {
	return remoteexec.Args(append([]string{"tmux", "-L", remoteSocket(sessionID)}, args...)...)
}

func (s *Service) startRemoteTerminal(ctx context.Context, session domain.Session, run domain.SessionRun, profile config.VariantConfig, kind agentruntime.AgentKind, req agentruntime.StartRequest, size terminalSize) (string, agentruntime.LaunchSpec, error) {
	s.remoteMu.Lock()
	w := s.remotes[session.ID]
	s.remoteMu.Unlock()
	if w == nil {
		gateway, port, err := s.remoteGateway(session)
		if err != nil {
			return "", agentruntime.LaunchSpec{}, err
		}
		w = &remoteWorker{session: session, run: run, gateway: gateway, port: port, connection: "disconnected"}
		s.remoteMu.Lock()
		s.remotes[session.ID] = w
		s.remoteMu.Unlock()
	}
	if req.Resume {
		if err := s.repo.UpdateRunAgentStatus(ctx, run.ID, ""); err != nil {
			return "", agentruntime.LaunchSpec{}, err
		}
	}
	if !req.Resume {
		if err := s.prepareRemoteWorker(ctx, w, profile, kind, req); err != nil {
			_ = w.gateway.Close()
			s.remoteMu.Lock()
			delete(s.remotes, session.ID)
			s.remoteMu.Unlock()
			return "", agentruntime.LaunchSpec{}, err
		}
	}
	s.cancelReceiverBridge(session.ID)
	s.storeBridgeCancel(session.ID, s.startReceiverBridge(session.ID))
	id, err := s.attachRemote(ctx, w, size)
	if err != nil {
		if !s.remoteMissing(w, err) {
			s.publishRemoteConnection(w, "disconnected", err.Error())
			s.remoteDisconnected(w)
		}
		id = uuid.NewString()
	}
	return id, agentruntime.LaunchSpec{Command: "ssh", Workdir: session.Workdir}, nil
}

func (s *Service) prepareRemoteWorker(ctx context.Context, w *remoteWorker, profile config.VariantConfig, kind agentruntime.AgentKind, req agentruntime.StartRequest) error {
	session := w.session
	commands := []string{"tmux", string(kind)}
	if kind == agentruntime.AgentClaude || kind == agentruntime.AgentCodex {
		commands = append(commands, "node")
	}
	for _, command := range commands {
		if _, err := remoteexec.Run(ctx, session.SSH, "command -v "+remoteexec.Quote(command), nil); err != nil {
			return fmt.Errorf("remote prerequisite %s is unavailable: %w", command, err)
		}
	}
	fs, err := remoteexec.NewFileSystem(ctx, session.SSH)
	if err != nil {
		return err
	}
	remoteHome, err := fs.UserHomeDir()
	if err != nil {
		return err
	}
	fs.TempDir = path.Join(remoteHome, ".local/state/hiveryn-workers", session.ID)
	resources, ok := s.repo.(remoteResourceStore)
	if !ok {
		return fmt.Errorf("remote resource persistence unavailable")
	}
	if err := resources.AddRemoteResource(ctx, session.ID, session.SSH, remoteSocket(session.ID), fs.TempDir); err != nil {
		return err
	}
	if err := fs.MkdirAll(fs.TempDir, 0700); err != nil {
		return err
	}
	for k, v := range profile.Env {
		fs.Env[k] = v
	}
	adapter := s.adapters[kind]
	setup := setupRequestForAgent(adapter, fs.Env)
	setup.FileSystem = fs
	if _, err := adapter.EnsureSetup(ctx, setup); err != nil {
		return fmt.Errorf("remote %s setup: %w", kind, err)
	}
	req.ID = session.ID
	req.Agent = kind
	req.FileSystem = fs
	req.DisableNativeQuestions = true
	req.ClaudeAutoMemory = profile.ClaudeAutoMemory
	endpoint := "http://127.0.0.1:" + strconv.Itoa(session.RemotePort)
	req.HookEndpoint = endpoint + "/hooks/" + session.RemoteToken
	req.Env = cloneStringMap(profile.Env)
	req.Env["HIVERYN_WORKER_TOKEN"] = session.RemoteToken
	servers, err := s.mcpServersForSession(session.SessionType, session.ArchitectKey, session.ID, profile.MCP)
	if err != nil {
		return err
	}
	servers[0] = agentruntime.MCPServerConfig{Name: config.ReservedMCPServerName, URL: endpoint + "/mcp", BearerTokenEnvVar: "HIVERYN_WORKER_TOKEN", ToolTimeout: hiverynMCPToolTimeout}
	req.MCPServers = servers
	req.Instructions += "\nIf Hiveryn MCP becomes unreachable, stop work and wait for Hiveryn to return. A failed call is not approval or success; do not blindly replay a mutation with an unknown outcome. Laptop path references and local Action outputs may be inaccessible here.\n"
	spec, err := adapter.PrepareLaunch(ctx, req)
	if err != nil {
		return fmt.Errorf("remote prepare launch: %w", err)
	}
	script := "#!/bin/sh\nset -eu\ncd " + remoteexec.Quote(spec.Workdir) + "\n"
	script += "unset TMUX TMUX_PANE STY TERM_PROGRAM TERM_PROGRAM_VERSION\nexport TERM=xterm-256color COLORTERM=truecolor\n"
	for _, key := range configKeys(spec.Env) {
		script += "export " + remoteexec.Quote(key+"="+spec.Env[key]) + "\n"
	}
	if len(spec.CleanupPaths) > 0 {
		script += "trap " + remoteexec.Quote("rm -f "+remoteexec.Args(spec.CleanupPaths...)) + " EXIT\n"
	}
	script += remoteexec.Args(append([]string{spec.Command}, spec.Args...)...) + "\n"
	launchPath, err := fs.WriteTemp("hiveryn-worker-", []byte(script))
	if err != nil {
		return err
	}
	// No existing server is ever reused for a new launch. All subsequent paths
	// attach only; an SSH error can never cause a duplicate provider invocation.
	command := tmux(session.ID, "has-session") + " 2>/dev/null && { echo 'owned tmux server already exists' >&2; exit 1; }; "
	command += tmux(session.ID, "-f", "/dev/null", "new-session", "-d", "-s", "worker", "-c", session.Workdir, "sh -c "+remoteexec.Quote(tmux(session.ID, "wait-for", "launch")+"; sh "+remoteexec.Quote(launchPath)+"; code=$?; rm -f "+remoteexec.Quote(launchPath)+"; exit $code")) + " && " + tmux(session.ID, "set-option", "-t", "worker", "remain-on-exit", "on") + " && " + tmux(session.ID, "set-option", "-t", "worker", "status", "off")
	if _, err := remoteexec.Run(ctx, session.SSH, command, nil); err != nil {
		return fmt.Errorf("create remote worker tmux (launch may require reconciliation): %w", err)
	}
	return nil
}

func (s *Service) attachRemote(ctx context.Context, w *remoteWorker, size terminalSize) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return "", fmt.Errorf("remote worker closed")
	}
	// Missing/dead tmux is a worker condition, distinct from SSH unreachability.
	script := "export LC_ALL=C; if state=$(" + tmux(w.session.ID, "display-message", "-p", "-t", "worker:0", "#{pane_dead}") + " 2>&1); then printf '%s' \"$state\"; else case \"$state\" in 'no server running on '*|'error connecting to '*'(No such file or directory)') printf missing;; *) printf '%s\\n' \"$state\" >&2; exit 1;; esac; fi"
	state, err := remoteexec.Run(ctx, w.session.SSH, script, nil)
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(string(state)) {
	case "missing":
		return "", &remoteMissingError{"Managed remote tmux session is missing; no replacement worker was launched."}
	case "1":
		return "", &remoteMissingError{"Remote worker exited; its tmux pane is retained for inspection. No replacement worker was launched."}
	}
	args := []string{"-tt", "-o", "ExitOnForwardFailure=yes", "-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", w.session.RemotePort, w.port)}
	args = append(args, remoteexec.Options(w.session.SSH)...)
	marker := "hiveryn-connected-" + uuid.NewString()
	id := uuid.NewString()
	endMarker := "hiveryn-detached-" + uuid.NewString()
	ready := &remoteReady{marker: []byte(marker), endMarker: []byte(endMarker), ready: make(chan struct{}), onDetached: func() { _ = s.terminal.Kill(context.Background(), w.session.ID, id); s.remoteDisconnected(w) }}
	exited := make(chan struct{})
	args = append(args, "sh -c "+remoteexec.Quote("printf "+remoteexec.Quote("\033]0;"+marker+"\007")+"; "+tmux(w.session.ID, "wait-for", "-S", "launch")+"; "+tmux(w.session.ID, "attach-session", "-t", "worker")+"; code=$?; printf "+remoteexec.Quote("\033]0;"+endMarker+"\007")+"; exit \"$code\""))
	if err := s.terminal.Start(ctx, terminalStartSpec{SessionID: w.session.ID, TerminalID: id, Name: mainTerminalName, Command: "ssh", Args: args, Size: size, Observer: ready, OnExit: func(terminalExit) { close(exited); go s.remoteDisconnected(w) }}); err != nil {
		return "", err
	}
	select {
	case <-ready.ready:
	case <-exited:
		return "", fmt.Errorf("SSH attachment exited before connection was established: %s", ready.failure())
	case <-ctx.Done():
		_ = s.terminal.Kill(context.Background(), w.session.ID, id)
		return "", ctx.Err()
	case <-time.After(20 * time.Second):
		_ = s.terminal.Kill(context.Background(), w.session.ID, id)
		return "", fmt.Errorf("SSH attachment did not become ready: %s", ready.failure())
	}
	w.connection = "connected"
	s.publishRemoteConnection(w, "connected", "")
	return id, nil
}
func (s *Service) publishRemoteConnection(w *remoteWorker, state, message string) {
	if err := s.appendAndPublishSessionEvent(context.Background(), domain.AppendSessionEventParams{SessionID: w.session.ID, RunID: w.run.ID, Type: "connection", Status: state, Message: message, At: time.Now().UTC()}); err != nil {
		s.logger.Error("publish remote connection", "session_id", w.session.ID, "error", err)
	}
}
func (s *Service) remoteDisconnected(w *remoteWorker) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.connection = "disconnected"
	if w.reconnecting {
		w.mu.Unlock()
		return
	}
	w.reconnecting = true
	w.mu.Unlock()
	if err := s.repo.UpdateRunAgentStatus(context.Background(), w.run.ID, ""); err != nil {
		s.logger.Error("clear stale remote activity", "error", err)
	}
	s.publishRemoteConnection(w, "disconnected", "SSH attachment lost; remote worker may still be running. Activity is stale.")
	go func() {
		defer func() {
			w.mu.Lock()
			w.reconnecting = false
			retry := !w.closed && w.connection == "disconnected"
			w.mu.Unlock()
			if retry {
				s.remoteDisconnected(w)
			}
		}()
		for {
			time.Sleep(3 * time.Second)
			w.mu.Lock()
			closed := w.closed
			w.mu.Unlock()
			if closed {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			id, err := s.attachRemote(ctx, w, terminalSize{Cols: defaultPTYCols, Rows: defaultPTYRows})
			cancel()
			if err != nil {
				if s.remoteMissing(w, err) {
					return
				}
				s.publishRemoteConnection(w, "disconnected", err.Error())
				continue
			}
			previousID := s.mainTerminalID(w.session.ID)
			if err := s.replaceSessionMainTerminalID(w.session.ID, id); err != nil {
				s.logger.Error("replace remote terminal", "error", err)
			}
			s.publishEvent(w.session.ID, domain.SessionEvent{SessionID: w.session.ID, Type: "main_terminal_resumed", Raw: map[string]any{"main_terminal_id": id, "previous_terminal_id": previousID}, At: time.Now().UTC()})
			return
		}
	}()
}

// stopRemote confirms termination before allowing any local end-state mutation.
func (s *Service) stopRemote(ctx context.Context, session domain.Session) error {
	return s.stopRemoteResources(ctx, session)
}
func (s *Service) closeRemote(id string) {
	s.remoteMu.Lock()
	w := s.remotes[id]
	delete(s.remotes, id)
	s.remoteMu.Unlock()
	if w != nil {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		_ = w.gateway.Close()
	}
}

// remoteReady recognizes only this attachment's unique setup marker, emitted
// after OpenSSH has established the reverse forwarding and remote command.
type remoteReady struct {
	endMarker  []byte
	onDetached func()
	detached   sync.Once
	mu         sync.Mutex
	output     []byte
	connected  bool
	marker     []byte
	tail       []byte
	ready      chan struct{}
	once       sync.Once
}

func (r *remoteReady) Output(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.connected {
		r.output = append(r.output, b...)
		if len(r.output) > 8192 {
			r.output = append([]byte(nil), r.output[len(r.output)-8192:]...)
		}
	}
	r.tail = append(r.tail, b...)
	if bytes.Contains(r.tail, r.marker) {
		r.connected = true
		r.output = nil
		r.once.Do(func() { close(r.ready) })
	}
	if len(r.endMarker) > 0 && bytes.Contains(r.tail, r.endMarker) {
		r.detached.Do(func() {
			if r.onDetached != nil {
				go r.onDetached()
			}
		})
	}
	if len(r.tail) > len(r.marker)+len(r.endMarker) {
		r.tail = append([]byte(nil), r.tail[len(r.tail)-len(r.marker)-len(r.endMarker):]...)
	}
}
func (*remoteReady) Resize(uint16, uint16) {}

func (s *Service) remoteMissing(w *remoteWorker, err error) bool {
	var missing *remoteMissingError
	if !errors.As(err, &missing) {
		return false
	}
	w.mu.Lock()
	w.connection = "missing"
	w.mu.Unlock()
	if err := s.repo.UpdateRunAgentStatus(context.Background(), w.run.ID, ""); err != nil {
		s.logger.Error("clear missing worker activity", "error", err)
	}
	s.publishRemoteConnection(w, "missing", missing.Error())
	return true
}

func (r *remoteReady) failure() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.output))
}
