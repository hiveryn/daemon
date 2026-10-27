// Package remoteexec executes operations in a POSIX SSH account. It never
// inherits laptop cwd, expands remote paths locally, or prints command inputs.
package remoteexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func Args(args ...string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = Quote(a)
	}
	return strings.Join(out, " ")
}
func Options(alias string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "SendEnv=-*", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "--", alias}
}

// Command leaves stdin available for private scripts or file content. The SSH
// alias is configuration, not a hostname assembled from request arguments.
// When ctx carries a usable Connection for alias, the command runs over it.
func Command(ctx context.Context, alias, script string) *exec.Cmd {
	cmd, _, _ := command(ctx, alias, script)
	return cmd
}

// command also returns ctx's connection for alias and whether cmd uses it.
func command(ctx context.Context, alias, script string) (*exec.Cmd, *Connection, bool) {
	var args []string
	c, shared := connectionFor(ctx, alias)
	if shared {
		args = []string{"-o", "ControlMaster=no", "-o", "ControlPath=" + c.socket}
	}
	args = append(append(args, Options(alias)...), "sh -c "+Quote(script))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	// A cancelled ssh is killed, but a child it started (a ProxyCommand, a
	// connection multiplexer) can keep its output pipes open; stop waiting for
	// them so a deadline actually bounds the call.
	cmd.WaitDelay = outputWaitDelay
	return cmd, c, shared
}

// outputWaitDelay bounds how long a killed ssh's leftover pipes are drained.
const outputWaitDelay = 2 * time.Second

// Run executes script on alias. A command for the alias of ctx's Connection is
// recorded in its stats, whether or not its master was still up.
func Run(ctx context.Context, alias, script string, input io.Reader) (_ []byte, err error) {
	cmd, c, shared := command(ctx, alias, script)
	if c != nil {
		started := time.Now()
		defer func() { c.record(time.Since(started), err, !shared) }()
	}
	cmd.Stdin = input
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A killed ssh says only "signal: killed"; name the cancellation that
		// killed it so a deadline is not mistaken for a remote failure.
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return nil, fmt.Errorf("SSH %s: %w (ssh %v): %s", alias, ctxErr, err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("SSH %s: %w: %s", alias, err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
