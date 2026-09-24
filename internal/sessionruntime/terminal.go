package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/hiveryn/daemon/internal/domain"
)

const (
	replayBufferSize = 64 * 1024
	outputQueueSize  = 512
	mainTerminalName = "main"

	// ptyTermType / ptyColorTerm describe the terminal the desktop actually
	// renders PTY output with (@xterm/xterm).
	ptyTermType  = "xterm-256color"
	ptyColorTerm = "truecolor"
)

// inheritedTerminalIdentityKeys are the parent-terminal identity variables that
// must not survive into a Hiveryn PTY. TMUX/TMUX_PANE and STY advertise "you
// are inside a multiplexer" (tmux and GNU screen respectively);
// TERM_PROGRAM/TERM_PROGRAM_VERSION name the emulator that launched the daemon.
// None of them describe the pty Hiveryn hands the child. GNU screen's WINDOW is
// handled separately: the name is generic enough to belong to something else,
// so it is only dropped alongside the STY that proves screen set it.
var inheritedTerminalIdentityKeys = []string{
	"TMUX",
	"TMUX_PANE",
	"STY",
	"TERM_PROGRAM",
	"TERM_PROGRAM_VERSION",
}

// decModeState tracks DEC private modes a TUI sets once at startup and never
// re-sends. The replay buffer is a 64 KB ring; once a long-running TUI's
// output exceeds that, its initial \e[?1049h (alt screen) and
// \e[?1000h/\e[?1002h/\e[?1003h/\e[?1006h (mouse) bytes are evicted. Any new
// xterm client then attaches in a terminal with mouse mode OFF — clicks and
// scrolls silently fail because xterm's MouseService never sees the
// enable-mouse-events flag flip. We watch every chunk for these mode-setting
// sequences and replay the live mode state on each attach.
type decModeState struct {
	mouse1003 bool // all-motion mouse reporting (\e[?1003h)
	mouse1002 bool // button+drag reporting (\e[?1002h)
	mouse1000 bool // X10 button-only reporting (\e[?1000h)
	mouse1006 bool // SGR extended encoding (\e[?1006h)
	altScreen bool // alternate screen + saved cursor (\e[?1049h)
}

// restoreSeq returns the bytes needed to restore this state in a fresh
// terminal. Empty when no special modes are active.
func (m *decModeState) restoreSeq() []byte {
	var buf []byte
	if m.altScreen {
		buf = append(buf, []byte("\x1b[?1049h")...)
	}
	// Mouse tracking modes are mutually exclusive — pick the highest enabled.
	switch {
	case m.mouse1003:
		buf = append(buf, []byte("\x1b[?1003h")...)
	case m.mouse1002:
		buf = append(buf, []byte("\x1b[?1002h")...)
	case m.mouse1000:
		buf = append(buf, []byte("\x1b[?1000h")...)
	}
	if m.mouse1006 {
		buf = append(buf, []byte("\x1b[?1006h")...)
	}
	return buf
}

// scanDECModes mutates m to reflect mode changes contained in chunk. Looks
// for exact byte sequences; sequences split across the 4 KB chunk boundary
// can theoretically be missed but in practice never are for the short
// CSI ? N h/l forms we care about.
func (m *decModeState) scanDECModes(chunk []byte) {
	if bytes.Contains(chunk, []byte("\x1b[?1049h")) {
		m.altScreen = true
	} else if bytes.Contains(chunk, []byte("\x1b[?1049l")) {
		m.altScreen = false
	}
	switch {
	case bytes.Contains(chunk, []byte("\x1b[?1003h")):
		m.mouse1003, m.mouse1002, m.mouse1000 = true, false, false
	case bytes.Contains(chunk, []byte("\x1b[?1003l")):
		m.mouse1003 = false
	case bytes.Contains(chunk, []byte("\x1b[?1002h")):
		m.mouse1003, m.mouse1002, m.mouse1000 = false, true, false
	case bytes.Contains(chunk, []byte("\x1b[?1002l")):
		m.mouse1002 = false
	case bytes.Contains(chunk, []byte("\x1b[?1000h")):
		m.mouse1003, m.mouse1002, m.mouse1000 = false, false, true
	case bytes.Contains(chunk, []byte("\x1b[?1000l")):
		m.mouse1000 = false
	}
	if bytes.Contains(chunk, []byte("\x1b[?1006h")) {
		m.mouse1006 = true
	} else if bytes.Contains(chunk, []byte("\x1b[?1006l")) {
		m.mouse1006 = false
	}
}

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
	TerminalID   string
	Name         string
	Command      string
	Args         []string
	Env          map[string]string
	Workdir      string
	Size         terminalSize
	CleanupPaths []string
	OnExit       func(terminalExit)
	// Observer, when set, sees the terminal's output and size changes in
	// order. Its calls run on the terminal's output and resize paths, so it
	// must not block.
	Observer terminalObserver
}

// terminalObserver follows what a terminal displays without being an
// attached client: it never writes to the terminal.
type terminalObserver interface {
	Output([]byte)
	Resize(cols, rows uint16)
}

type terminalSize struct {
	Cols uint16
	Rows uint16
}

type terminalExit struct {
	SessionID  string
	TerminalID string
	Name       string
	Err        error
}

type ptyTerminalManager struct {
	logger *slog.Logger

	mu        sync.RWMutex
	processes map[string]*terminalProcess
}

type terminalProcess struct {
	key          string
	terminalID   string
	sessionID    string
	name         string
	command      string
	cmd          *exec.Cmd
	pty          *os.File
	cleanupPaths []string
	logger       *slog.Logger
	onExit       func(terminalExit)
	observer     terminalObserver

	mu          sync.Mutex
	closing     bool
	cleanupOnce sync.Once
	outputNext  uint64
	outputSubs  map[uint64]chan []byte
	replayBuf   []byte
	decModes    decModeState
	done        chan struct{}
}

type terminalAttachment struct {
	output <-chan []byte
	write  func([]byte) error
	resize func(uint16, uint16) error
	close  func() error
}

func terminalKey(sessionID, terminalID string) string {
	return sessionID + ":" + terminalID
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
	if spec.TerminalID == "" {
		return fmt.Errorf("missing terminal ID")
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

	key := terminalKey(spec.SessionID, spec.TerminalID)
	if err := m.reserve(key); err != nil {
		return err
	}

	argv := append([]string{spec.Command}, spec.Args...)
	env := mergeProcessEnv(spec.Env, spec.Workdir)
	// Never log env: it carries the daemon's inherited secrets verbatim. The
	// key count is enough to tell "the child got an environment" from "it got
	// nothing"; individual values are not diagnosable from logs by design.
	m.logger.Info("[pty] exec",
		"terminal_key", key,
		"terminal_id", spec.TerminalID,
		"command", spec.Command,
		"args", spec.Args,
		"argv", argv,
		"env_count", len(env),
		"term", ptyTermType,
		"workdir", spec.Workdir,
	)

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Workdir
	cmd.Env = env

	m.logger.Info("[pty] start",
		"terminal_key", key,
		"terminal_id", spec.TerminalID,
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
		key:          key,
		terminalID:   spec.TerminalID,
		sessionID:    spec.SessionID,
		name:         spec.Name,
		command:      spec.Command,
		cmd:          cmd,
		pty:          ptyFile,
		cleanupPaths: append([]string(nil), spec.CleanupPaths...),
		logger:       m.logger.With("terminal_key", key),
		onExit:       spec.OnExit,
		observer:     spec.Observer,
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

func (m *ptyTerminalManager) Attach(_ context.Context, sessionID, terminalID string) (domain.TerminalAttachment, error) {
	key := terminalKey(sessionID, terminalID)
	process := m.get(key)
	if process == nil {
		return nil, &domain.ConflictError{Resource: "session", Field: "status", Message: "terminal is not running"}
	}
	return process.attach()
}

func (m *ptyTerminalManager) Kill(ctx context.Context, sessionID, terminalID string) error {
	key := terminalKey(sessionID, terminalID)
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
		if process != nil && strings.HasPrefix(process.key, prefix) {
			status := "running"
			if process.isClosing() {
				status = "exited"
			}
			terminals = append(terminals, domain.TerminalInfo{
				TerminalID: process.terminalID,
				SessionID:  process.sessionID,
				Command:    process.command,
				Status:     status,
			})
		}
	}
	sort.Slice(terminals, func(i, j int) bool {
		return terminals[i].TerminalID < terminals[j].TerminalID
	})
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
		return &domain.ConflictError{Resource: "terminal", Field: "terminal_id", Message: "terminal already exists"}
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
			if process.observer != nil {
				process.observer.Output(chunk)
			}
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
		process.onExit(terminalExit{SessionID: process.sessionID, TerminalID: process.terminalID, Name: process.name, Err: err})
	}

	m.remove(process.key)
	close(process.done)
}

func (p *terminalProcess) attach() (domain.TerminalAttachment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || p.pty == nil {
		return nil, &domain.ConflictError{Resource: "session", Field: "status", Message: "terminal is closing"}
	}

	ch := make(chan []byte, outputQueueSize)
	p.outputNext++
	subID := p.outputNext

	// 1. Replay buffer paints the visible screen content.
	if len(p.replayBuf) > 0 {
		ch <- append([]byte(nil), p.replayBuf...)
	}

	// 2. Live DEC mode state, sent AFTER replay so it wins over any
	//    intermediate disable bytes still inside the replay window.
	//    This is the actual fix for the regression where mouse events stop
	//    working in long-running TUIs (lazygit, opencode, claude) — the
	//    mouse-enable bytes get evicted from replayBuf after ~64 KB of
	//    redraws, but the TUI never re-emits them.
	if restore := p.decModes.restoreSeq(); len(restore) > 0 {
		p.logger.Info("[pty] replaying DEC mode state on attach",
			"sub_id", subID,
			"bytes", len(restore),
			"alt_screen", p.decModes.altScreen,
			"mouse1003", p.decModes.mouse1003,
			"mouse1002", p.decModes.mouse1002,
			"mouse1000", p.decModes.mouse1000,
			"mouse1006", p.decModes.mouse1006,
		)
		ch <- restore
	}

	p.outputSubs[subID] = ch

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
	if p.observer != nil {
		p.observer.Resize(cols, rows)
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

	p.replayBuf = append(p.replayBuf, chunk...)
	if len(p.replayBuf) > replayBufferSize {
		p.replayBuf = p.replayBuf[len(p.replayBuf)-replayBufferSize:]
	}

	// Sample mode-setting bytes before they age out of replayBuf. This is
	// what makes attach() able to restore mouse/alt-screen state even after
	// the TUI's startup bytes have been overwritten by hours of redraws.
	before := p.decModes
	p.decModes.scanDECModes(chunk)
	if before != p.decModes {
		p.logger.Info("[pty] DEC mode state changed",
			"alt_screen", p.decModes.altScreen,
			"mouse1003", p.decModes.mouse1003,
			"mouse1002", p.decModes.mouse1002,
			"mouse1000", p.decModes.mouse1000,
			"mouse1006", p.decModes.mouse1006,
		)
	}

	for id, ch := range p.outputSubs {
		payload := append([]byte(nil), chunk...)
		select {
		case ch <- payload:
			continue
		default:
		}

		// Channel full: coalesce the backlog into a single ordered payload
		// rather than evicting. Dropping bytes is not an option — a hole
		// mid-escape-sequence leaves xterm's parser in an unknown state with no
		// recovery path — but draining the queued chunks and concatenating them
		// in receive order (oldest first) with the new chunk preserves byte
		// order with zero holes. This absorbs transient consumer slowness (most
		// notably the attach handshake window opened by the attach-time resize)
		// that previously dropped the WS the instant 512 chunks queued up.
		queued := make([][]byte, 0, len(ch)+1)
		total := len(payload)
	drain:
		for {
			select {
			case b := <-ch:
				queued = append(queued, b)
				total += len(b)
			default:
				break drain
			}
		}

		// Backstop: a consumer that has fallen further behind than the replay
		// buffer is genuinely stuck (dead WS, paused tab). Reconnect+replay
		// delivers the same 64 KB of state with a clean parser reset, so evict
		// rather than grow an unbounded coalesced buffer. The desktop detects
		// the close via onTerminalClosed and auto-reconnects.
		if total > replayBufferSize {
			p.logger.Warn("[pty] subscriber too far behind, evicting",
				"sub_id", id, "bytes", total)
			delete(p.outputSubs, id)
			close(ch)
			continue
		}

		merged := make([]byte, 0, total)
		for _, b := range queued {
			merged = append(merged, b...)
		}
		merged = append(merged, payload...)

		// The channel was just drained to empty under p.mu, so this send always
		// succeeds without blocking. The select guards the (impossible) case
		// where the consumer is closing the channel concurrently.
		select {
		case ch <- merged:
		default:
			p.logger.Warn("[pty] subscriber channel unwritable after drain, evicting",
				"sub_id", id, "bytes", len(merged))
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

// mergeProcessEnv builds the environment for a Hiveryn PTY: the daemon's own
// environment, with the launcher's terminal identity replaced by the terminal
// Hiveryn actually provides, then the caller's deliberate overrides, then PWD.
//
// Override order is deliberate. Terminal identity is applied to the inherited
// copy BEFORE extra, so a variant profile that sets TERM (or deliberately
// re-exports TMUX) still wins; PWD is applied last because the pty's working
// directory is a fact, not a preference.
func mergeProcessEnv(extra map[string]string, workdir string) []string {
	env := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	applyTerminalIdentity(env)
	for key, value := range extra {
		env[key] = value
	}
	if workdir != "" {
		env["PWD"] = workdir
	}

	out := make([]string, 0, len(env))
	for _, key := range configKeys(env) {
		out = append(out, key+"="+env[key])
	}
	return out
}

// applyTerminalIdentity makes the child process describe the terminal it is
// actually attached to — the desktop's @xterm/xterm client — instead of
// whatever terminal launched the daemon.
//
// Inheriting the launcher's identity is not merely inaccurate, it corrupts
// output. Started from inside tmux, a shell sees TERM=tmux-256color, and
// oh-my-zsh's termsupport.zsh then emits window titles as tmux's
// ESC k <title> ESC \ form instead of the OSC form. xterm.js does not
// implement ESC k, so it prints the title text into the buffer: typing `ls`
// renders a second literal "ls" immediately before the listing. The same
// branch fires under GNU screen (TERM=screen-*).
//
// The multiplexer/emulator markers are removed rather than rewritten. There is
// no tmux server or host emulator behind a Hiveryn pty, so any tool that
// branches on them would branch wrongly; an absent variable is the honest
// answer, while an invented TERM_PROGRAM value would just move the guesswork.
func applyTerminalIdentity(env map[string]string) {
	// xterm.js is xterm-compatible with a 256-color palette and 24-bit SGR
	// support, so this is a description of the client, not a lowest common
	// denominator.
	env["TERM"] = ptyTermType
	env["COLORTERM"] = ptyColorTerm
	if _, inScreen := env["STY"]; inScreen {
		delete(env, "WINDOW")
	}
	for _, key := range inheritedTerminalIdentityKeys {
		delete(env, key)
	}
}

func cleanupPaths(paths []string, logger *slog.Logger) {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Warn("cleanup path failed", "path", path, "error", err)
		}
	}
}
