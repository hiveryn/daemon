package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/hiveryn/daemon/internal/domain"
)

const (
	replayChunkLimit = 256
	outputQueueSize  = 64
)

var errTerminalNotFound = errors.New("terminal not found")

type terminalManager interface {
	Start(context.Context, terminalStartSpec) error
	Attach(context.Context, string, string) (domain.TerminalAttachment, error)
	Kill(context.Context, string, string) error
	KillBySession(context.Context, string) error
	ListBySession(string) []domain.TerminalInfo
	Shutdown(context.Context) error
}

type terminalStartSpec struct {
	SessionID    string
	Name         string
	Command      string
	Args         []string
	Env          map[string]string
	Workdir      string
	Size         terminalSize
	CleanupPaths []string
	OnExit       func(terminalExit)
}

type terminalSize struct {
	Cols uint16
	Rows uint16
}

type terminalExit struct {
	SessionID string
	Name      string
	Err       error
}

type ptyTerminalManager struct {
	logger *slog.Logger

	mu        sync.RWMutex
	processes map[string]*terminalProcess
}

type terminalProcess struct {
	id           string
	sessionID    string
	name         string
	cmd          *exec.Cmd
	pty          *os.File
	cleanupPaths []string
	logger       *slog.Logger
	onExit       func(terminalExit)

	mu          sync.Mutex
	closing     bool
	cleanupOnce sync.Once
	outputNext  uint64
	outputSubs  map[uint64]chan []byte
	replayBuf   [][]byte
	done        chan struct{}
}

type terminalAttachment struct {
	output <-chan []byte
	write  func([]byte) error
	resize func(uint16, uint16) error
	close  func() error
}

func terminalKey(sessionID, name string) string {
	return sessionID + ":" + name
}

func sessionPrefix(sessionID string) string {
	return sessionID + ":"
}

func newPTYTerminalManager(logger *slog.Logger) *ptyTerminalManager {
	return &ptyTerminalManager{
		logger:    logger,
		processes: map[string]*terminalProcess{},
	}
}

func (m *ptyTerminalManager) Start(ctx context.Context, spec terminalStartSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if spec.SessionID == "" {
		return fmt.Errorf("missing terminal session ID")
	}
	if spec.Name == "" {
		return fmt.Errorf("missing terminal name")
	}
	if spec.Command == "" {
		return fmt.Errorf("missing terminal command")
	}
	if spec.Size.Cols == 0 {
		spec.Size.Cols = defaultPTYCols
	}
	if spec.Size.Rows == 0 {
		spec.Size.Rows = defaultPTYRows
	}

	key := terminalKey(spec.SessionID, spec.Name)
	if err := m.reserve(key); err != nil {
		return err
	}

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Workdir
	cmd.Env = mergeProcessEnv(spec.Env)

	m.logger.Info("[pty] start",
		"terminal_key", key,
		"cols", spec.Size.Cols,
		"rows", spec.Size.Rows,
	)

	ptyFile, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: spec.Size.Cols, Rows: spec.Size.Rows})
	if err != nil {
		m.remove(key)
		cleanupPaths(spec.CleanupPaths, m.logger)
		return fmt.Errorf("start pty: %w", err)
	}

	process := &terminalProcess{
		id:           key,
		sessionID:    spec.SessionID,
		name:         spec.Name,
		cmd:          cmd,
		pty:          ptyFile,
		cleanupPaths: append([]string(nil), spec.CleanupPaths...),
		logger:       m.logger.With("terminal_key", key),
		onExit:       spec.OnExit,
		outputSubs:   map[uint64]chan []byte{},
		done:         make(chan struct{}),
	}

	m.mu.Lock()
	m.processes[key] = process
	m.mu.Unlock()

	go m.streamOutput(process)
	go m.waitForExit(process)
	return nil
}

func (m *ptyTerminalManager) Attach(_ context.Context, sessionID, name string) (domain.TerminalAttachment, error) {
	key := terminalKey(sessionID, name)
	process := m.get(key)
	if process == nil {
		return nil, &domain.ConflictError{Resource: "session", Field: "status", Message: "terminal is not running"}
	}
	return process.attach()
}

func (m *ptyTerminalManager) Kill(ctx context.Context, sessionID, name string) error {
	key := terminalKey(sessionID, name)
	process := m.get(key)
	if process == nil {
		return errTerminalNotFound
	}

	process.markClosing()
	process.closePTY()
	if process.cmd.Process != nil {
		if err := process.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill process: %w", err)
		}
	}
	return waitForChannel(ctx, process.done)
}

func (m *ptyTerminalManager) KillBySession(ctx context.Context, sessionID string) error {
	prefix := sessionPrefix(sessionID)
	var processes []*terminalProcess

	m.mu.RLock()
	for key, process := range m.processes {
		if process != nil && strings.HasPrefix(key, prefix) {
			processes = append(processes, process)
		}
	}
	m.mu.RUnlock()

	for _, process := range processes {
		process.markClosing()
		process.closePTY()
		if process.cmd.Process != nil {
			if err := process.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				process.logger.Warn("kill process during session teardown failed", "error", err)
			}
		}
	}

	var killErr error
	for _, process := range processes {
		if err := waitForChannel(ctx, process.done); err != nil && killErr == nil {
			killErr = err
		}
	}
	return killErr
}

func (m *ptyTerminalManager) ListBySession(sessionID string) []domain.TerminalInfo {
	prefix := sessionPrefix(sessionID)

	m.mu.RLock()
	defer m.mu.RUnlock()

	var terminals []domain.TerminalInfo
	for _, process := range m.processes {
		if process != nil && strings.HasPrefix(process.id, prefix) {
			status := "running"
			if process.isClosing() {
				status = "exited"
			}
			terminals = append(terminals, domain.TerminalInfo{
				Name:      process.name,
				SessionID: process.sessionID,
				Status:    status,
			})
		}
	}
	return terminals
}

func (m *ptyTerminalManager) Shutdown(ctx context.Context) error {
	processes := m.list()
	for _, process := range processes {
		process.markClosing()
		process.closePTY()
		if process.cmd.Process != nil {
			if err := process.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				process.logger.Warn("kill process during shutdown failed", "error", err)
			}
		}
	}

	var shutdownErr error
	for _, process := range processes {
		if err := waitForChannel(ctx, process.done); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}

func (m *ptyTerminalManager) reserve(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.processes[key]; exists {
		return &domain.ConflictError{Resource: "terminal", Field: "name", Message: "terminal already exists"}
	}
	m.processes[key] = nil
	return nil
}

func (m *ptyTerminalManager) get(key string) *terminalProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.processes[key]
}

func (m *ptyTerminalManager) remove(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.processes, key)
}

func (m *ptyTerminalManager) list() []*terminalProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()
	processes := make([]*terminalProcess, 0, len(m.processes))
	for _, process := range m.processes {
		if process != nil {
			processes = append(processes, process)
		}
	}
	return processes
}

func (m *ptyTerminalManager) streamOutput(process *terminalProcess) {
	buffer := make([]byte, 4096)
	for {
		n, err := process.read(buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			process.broadcast(chunk)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				process.logger.Debug("pty read ended", "error", err)
			}
			process.closeOutputs()
			process.closePTY()
			return
		}
	}
}

func (m *ptyTerminalManager) waitForExit(process *terminalProcess) {
	err := process.cmd.Wait()
	process.cleanup()

	killed := process.isClosing()
	if !killed && process.onExit != nil {
		process.onExit(terminalExit{SessionID: process.sessionID, Name: process.name, Err: err})
	}

	m.remove(process.id)
	close(process.done)
}

func (p *terminalProcess) attach() (domain.TerminalAttachment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || p.pty == nil {
		return nil, &domain.ConflictError{Resource: "session", Field: "status", Message: "terminal is closing"}
	}

	ch := make(chan []byte, len(p.replayBuf)+outputQueueSize)
	p.outputNext++
	subID := p.outputNext
	p.outputSubs[subID] = ch
	for _, chunk := range p.replayBuf {
		ch <- append([]byte(nil), chunk...)
	}

	return &terminalAttachment{
		output: ch,
		write:  p.write,
		resize: p.resize,
		close: func() error {
			p.detach(subID)
			return nil
		},
	}, nil
}

func (p *terminalProcess) read(buffer []byte) (int, error) {
	p.mu.Lock()
	ptyFile := p.pty
	p.mu.Unlock()
	if ptyFile == nil {
		return 0, os.ErrClosed
	}
	return ptyFile.Read(buffer)
}

func (p *terminalProcess) write(data []byte) error {
	p.mu.Lock()
	ptyFile := p.pty
	closing := p.closing
	p.mu.Unlock()
	if closing || ptyFile == nil {
		return fmt.Errorf("session is closing")
	}
	_, err := ptyFile.Write(data)
	return err
}

func (p *terminalProcess) resize(cols, rows uint16) error {
	p.mu.Lock()
	ptyFile := p.pty
	closing := p.closing
	p.mu.Unlock()
	if closing || ptyFile == nil {
		p.logger.Info("[pty] resize skipped (closing)", "cols", cols, "rows", rows)
		return fmt.Errorf("session is closing")
	}
	p.logger.Info("[pty] resize", "cols", cols, "rows", rows)
	if err := pty.Setsize(ptyFile, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		p.logger.Warn("[pty] resize failed", "cols", cols, "rows", rows, "error", err)
		return err
	}
	return nil
}

func (p *terminalProcess) detach(subID uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.outputSubs[subID]; existing != nil {
		delete(p.outputSubs, subID)
		close(existing)
	}
}

func (p *terminalProcess) broadcast(chunk []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	replayChunk := append([]byte(nil), chunk...)
	p.replayBuf = append(p.replayBuf, replayChunk)
	if len(p.replayBuf) > replayChunkLimit {
		p.replayBuf = p.replayBuf[len(p.replayBuf)-replayChunkLimit:]
	}

	for id, ch := range p.outputSubs {
		payload := append([]byte(nil), replayChunk...)
		select {
		case ch <- payload:
		default:
			delete(p.outputSubs, id)
			close(ch)
		}
	}
}

func (p *terminalProcess) closeOutputs() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.outputSubs {
		delete(p.outputSubs, id)
		close(ch)
	}
}

func (p *terminalProcess) markClosing() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closing = true
}

func (p *terminalProcess) isClosing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closing
}

func (p *terminalProcess) closePTY() {
	p.mu.Lock()
	ptyFile := p.pty
	p.pty = nil
	p.mu.Unlock()
	if ptyFile != nil {
		_ = ptyFile.Close()
	}
}

func (p *terminalProcess) cleanup() {
	p.cleanupOnce.Do(func() {
		cleanupPaths(p.cleanupPaths, p.logger)
	})
}

func (t *terminalAttachment) Output() <-chan []byte {
	return t.output
}

func (t *terminalAttachment) Write(data []byte) error {
	return t.write(data)
}

func (t *terminalAttachment) Resize(cols, rows uint16) error {
	return t.resize(cols, rows)
}

func (t *terminalAttachment) Close() error {
	return t.close()
}

func waitForChannel(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func mergeProcessEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	for key, value := range extra {
		env[key] = value
	}

	out := make([]string, 0, len(env))
	for _, key := range configKeys(env) {
		out = append(out, key+"="+env[key])
	}
	return out
}

func cleanupPaths(paths []string, logger *slog.Logger) {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Warn("cleanup path failed", "path", path, "error", err)
		}
	}
}
