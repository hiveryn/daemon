package remoteexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Connection is one OpenSSH master shared by the commands of a bounded
// operation (a worker launch), so they pay the handshake once instead of per
// command. It is private to its owner: the control socket lives in a fresh
// 0700 directory of this daemon's account, ControlPersist is off, and Close
// stops the master and removes the directory. Authentication, proxies and every
// other option still come from the account's configuration for the alias;
// only master/control settings are fixed for this connection's own commands.
//
// A command reaches it only through a context carrying it (WithConnection) and
// only for the same alias. Anything that must outlive the owner — an attached
// worker PTY, a reconnect — opens its own connection. A master that has gone
// away is not an error: OpenSSH falls back to a direct connection.
type Connection struct {
	alias  string
	dir    string
	socket string
	cmd    *exec.Cmd
	exited chan struct{}
	stderr bytes.Buffer // guarded by the process until exited is closed

	mu      sync.Mutex
	closed  bool
	ops     int
	failed  int
	direct  int
	busy    time.Duration
	slowest time.Duration
	stages  []stage
}

type stage struct {
	name       string
	start, end time.Time
	ops        int
}

// ConnectionStats summarizes the commands run on a connection. It never holds
// scripts, inputs or output.
type ConnectionStats struct {
	Operations int           // commands run while the master was usable
	Failed     int           // commands, shared or direct, that returned an error
	Direct     int           // commands for this alias run after the master was gone
	Busy       time.Duration // summed command time
	Slowest    time.Duration
	Stage      string // the stage last entered (see Stage)
	Stages     []StageTiming
}

// StageTiming is the elapsed time and command count of one named stage.
type StageTiming struct {
	Name       string
	Elapsed    time.Duration
	Operations int
}

// masterReadyPoll is how often Dial looks for the control socket.
const masterReadyPoll = 25 * time.Millisecond

// masterStopGrace bounds a master's own shutdown before it is killed.
const masterStopGrace = 2 * time.Second

// maxDialWait bounds establishing the master within the caller's own context:
// ConnectTimeout covers TCP, not authentication.
const maxDialWait = 30 * time.Second

// Dial starts a master for alias and waits until it accepts commands. It
// always returns a Connection the caller must Close: when the master cannot be
// established the error carries ssh's own reason, nothing is left running, and
// the connection's commands connect directly while still being counted.
func Dial(ctx context.Context, alias string) (*Connection, error) {
	c := &Connection{alias: alias, exited: make(chan struct{})}
	dir, err := controlDir()
	if err != nil {
		close(c.exited)
		return c, fmt.Errorf("SSH %s: control directory: %w", alias, err)
	}
	c.dir, c.socket = dir, filepath.Join(dir, "s")
	args := []string{"-N", "-o", "ControlMaster=yes", "-o", "ControlPersist=no", "-o", "ControlPath=" + c.socket}
	c.cmd = exec.Command("ssh", append(args, Options(alias)...)...)
	c.cmd.Stderr = &c.stderr
	c.cmd.WaitDelay = outputWaitDelay
	if err := c.cmd.Start(); err != nil {
		close(c.exited)
		c.stop()
		return c, fmt.Errorf("SSH %s: start master: %w", alias, err)
	}
	go func() { _ = c.cmd.Wait(); close(c.exited) }()

	wait, cancel := context.WithTimeout(ctx, maxDialWait)
	defer cancel()
	tick := time.NewTicker(masterReadyPoll)
	defer tick.Stop()
	for {
		if _, err := os.Stat(c.socket); err == nil {
			return c, nil
		}
		select {
		case <-c.exited:
			c.stop()
			return c, fmt.Errorf("SSH %s: master exited (%v): %s", alias, c.cmd.ProcessState, strings.TrimSpace(c.stderr.String()))
		case <-wait.Done():
			c.stop()
			return c, fmt.Errorf("SSH %s: master not ready: %w: %s", alias, context.Cause(wait), strings.TrimSpace(c.stderr.String()))
		case <-tick.C:
		}
	}
}

// controlDir makes a private directory usable as a ControlPath: short enough
// for a Unix socket (OpenSSH binds a temporary name 17 bytes longer, and
// sun_path is 104 bytes on macOS) and free of what ssh would split or expand
// (whitespace, quotes, %tokens, ~).
func controlDir() (string, error) {
	dir, err := os.MkdirTemp("", "hvssh-")
	if err == nil && len(dir) <= 80 && !strings.ContainsAny(dir, " \t\n\"'%~") {
		return dir, nil
	}
	if err == nil {
		_ = os.RemoveAll(dir)
	}
	return os.MkdirTemp("/tmp", "hvssh-")
}

// Close stops the master and removes its directory. Commands still running on
// it are cut off, so the owner closes only once its own commands returned.
func (c *Connection) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.stop()
}

// stop asks the master to exit, letting OpenSSH stop a ProxyCommand child it
// started, and kills it if it has not exited within masterStopGrace.
func (c *Connection) stop() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.exited:
		case <-time.After(masterStopGrace):
			_ = c.cmd.Process.Kill()
		}
	}
	<-c.exited
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
}

func (c *Connection) usable() bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// Stage names the part of the operation the following commands belong to.
func (c *Connection) Stage(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if n := len(c.stages); n > 0 {
		c.stages[n-1].end = now
	}
	c.stages = append(c.stages, stage{name: name, start: now})
}

func (c *Connection) record(d time.Duration, err error, direct bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if direct {
		c.direct++
	} else {
		c.ops++
	}
	if err != nil {
		c.failed++
	}
	c.busy += d
	if d > c.slowest {
		c.slowest = d
	}
	if n := len(c.stages); n > 0 {
		c.stages[n-1].ops++
	}
}

// Stats reports the commands so far; the current stage runs to now.
func (c *Connection) Stats() ConnectionStats {
	if c == nil {
		return ConnectionStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := ConnectionStats{Operations: c.ops, Failed: c.failed, Direct: c.direct, Busy: c.busy, Slowest: c.slowest}
	for _, st := range c.stages {
		end := st.end
		if end.IsZero() {
			end = time.Now()
		}
		out.Stages = append(out.Stages, StageTiming{Name: st.name, Elapsed: end.Sub(st.start), Operations: st.ops})
		out.Stage = st.name
	}
	return out
}

// String renders the stats for logs and error context.
func (s ConnectionStats) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d SSH operations over the shared connection", s.Operations)
	if s.Direct > 0 {
		fmt.Fprintf(&b, ", %d direct", s.Direct)
	}
	fmt.Fprintf(&b, ", %d failed, %s busy, slowest %s", s.Failed, s.Busy.Round(time.Millisecond), s.Slowest.Round(time.Millisecond))
	if len(s.Stages) > 0 {
		b.WriteString("; stages:")
		for _, st := range s.Stages {
			fmt.Fprintf(&b, " %s %s/%d ops", st.Name, st.Elapsed.Round(time.Millisecond), st.Operations)
		}
	}
	return b.String()
}

type connectionKey struct{}

// WithConnection makes commands for c's alias run over c while ctx is used.
func WithConnection(ctx context.Context, c *Connection) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, connectionKey{}, c)
}

// ConnectionFrom returns the connection ctx carries, if any.
func ConnectionFrom(ctx context.Context) *Connection {
	c, _ := ctx.Value(connectionKey{}).(*Connection)
	return c
}

// connectionFor returns ctx's connection for alias and whether it is usable.
func connectionFor(ctx context.Context, alias string) (*Connection, bool) {
	c := ConnectionFrom(ctx)
	if c == nil || c.alias != alias {
		return nil, false
	}
	return c, c.usable()
}
