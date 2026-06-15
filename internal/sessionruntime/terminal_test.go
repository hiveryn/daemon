package sessionruntime

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestPTYTerminalManagerLogsExecutionSpec(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	manager := newPTYTerminalManager(slog.New(slog.NewJSONHandler(&logs, nil)))

	err := manager.Start(context.Background(), terminalStartSpec{
		SessionID:  "session-1",
		TerminalID: "term-main-1",
		Name:       mainTerminalName,
		Command:    "hiveryn-command-that-does-not-exist",
		Args:       []string{"--flag", "value"},
		Env:        map[string]string{"HIVERYN_TEST_EXEC_LOG": "visible"},
		Workdir:    t.TempDir(),
		Size:       terminalSize{Cols: 80, Rows: 24},
	})
	if err == nil {
		t.Fatal("expected missing command to fail")
	}

	logOutput := logs.String()
	for _, want := range []string{
		`"msg":"[pty] exec"`,
		`"terminal_key":"session-1:term-main-1"`,
		`"terminal_id":"term-main-1"`,
		`"command":"hiveryn-command-that-does-not-exist"`,
		`"args":["--flag","value"]`,
		`"argv":["hiveryn-command-that-does-not-exist","--flag","value"]`,
		`HIVERYN_TEST_EXEC_LOG=visible`,
		`"workdir":`,
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("expected log output to contain %s, got:\n%s", want, logOutput)
		}
	}
}

func TestTerminalProcessReplaysBufferedOutputToLateAttach(t *testing.T) {
	t.Parallel()

	process := newTestTerminalProcess(t)
	process.broadcast([]byte("boot"))

	attachment, err := process.attach()
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	assertOutputChunk(t, attachment.Output(), "boot")

	process.broadcast([]byte("next"))
	assertOutputChunk(t, attachment.Output(), "next")
}

func TestTerminalProcessReplaysBeforeResizeOutput(t *testing.T) {
	t.Parallel()

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	defer func() { _ = master.Close() }()
	defer func() { _ = slave.Close() }()

	process := &terminalProcess{
		key:        terminalKey("session-1", "term-1"),
		terminalID: "term-1",
		pty:        master,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		outputSubs: map[uint64]chan []byte{},
		done:       make(chan struct{}),
	}
	process.broadcast([]byte("replay"))

	attachment, err := process.attach()
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	resizeDone := make(chan error, 1)
	go func() {
		if err := attachment.Resize(120, 40); err != nil {
			resizeDone <- err
			return
		}
		process.broadcast([]byte("redraw"))
		resizeDone <- nil
	}()

	if err := <-resizeDone; err != nil {
		t.Fatalf("resize terminal: %v", err)
	}
	assertOutputChunk(t, attachment.Output(), "replay")
	assertOutputChunk(t, attachment.Output(), "redraw")
}

func TestTerminalProcessCoalescesBacklogWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	process := newTestTerminalProcess(t)
	attachment, err := process.attach()
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	// Overflow the channel without draining it. The total stays well under the
	// replay-buffer backstop, so nothing is evicted: the backlog is coalesced
	// into a single ordered payload instead of dropping the subscriber.
	const chunks = outputQueueSize + 64
	for range chunks {
		process.broadcast([]byte("x"))
	}

	// Drain everything the subscriber received and confirm zero bytes were lost
	// and the channel was never closed (no eviction).
	var got []byte
	deadline := time.After(time.Second)
	for len(got) < chunks {
		select {
		case b, ok := <-attachment.Output():
			if !ok {
				t.Fatalf("subscriber was evicted; got %d of %d bytes", len(got), chunks)
			}
			got = append(got, b...)
		case <-deadline:
			t.Fatalf("timed out after receiving %d of %d bytes", len(got), chunks)
		}
	}
	if string(got) != strings.Repeat("x", chunks) {
		t.Fatalf("coalesced stream lost or reordered bytes: got %d bytes", len(got))
	}
}

func TestTerminalProcessEvictsSubscriberBeyondReplayBuffer(t *testing.T) {
	t.Parallel()

	process := newTestTerminalProcess(t)
	attachment, err := process.attach()
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	// A consumer that falls further behind than the replay buffer is genuinely
	// stuck. Eviction only triggers once the channel is full (coalescing can no
	// longer make progress), so overflow the queue with chunks whose coalesced
	// total exceeds the replay buffer. Each 4 KB chunk × (outputQueueSize+1)
	// far exceeds the 64 KB backstop, so the first full-channel broadcast
	// drains a >64 KB backlog and evicts rather than re-enqueueing it.
	chunk := bytes.Repeat([]byte("x"), 4096)
	for range outputQueueSize + 1 {
		process.broadcast(chunk)
	}

	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-attachment.Output():
			if !ok {
				return // channel closed → subscriber evicted, as expected
			}
		case <-deadline:
			t.Fatal("expected subscriber channel to be closed after exceeding replay buffer")
		}
	}
}

func TestPTYTerminalManagerReplaysStartupOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}

	manager := newPTYTerminalManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := manager.Start(ctx, terminalStartSpec{
		SessionID:  "session-1",
		TerminalID: "term-main-1",
		Name:       mainTerminalName,
		Command:    "/bin/sh",
		Args:       []string{"-c", "printf boot; sleep 1"},
		Size:       terminalSize{Cols: 80, Rows: 24},
	}); err != nil {
		t.Fatalf("start terminal: %v", err)
	}
	defer func() {
		killCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Kill(killCtx, "session-1", "term-main-1")
	}()

	time.Sleep(100 * time.Millisecond)

	attachment, err := manager.Attach(ctx, "session-1", "term-main-1")
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	assertOutputChunk(t, attachment.Output(), "boot")
}

func TestMergeProcessEnvSetsPWDToWorkdir(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	env := mergeProcessEnv(map[string]string{
		"PWD":              "/wrong/workdir",
		"HIVERYN_TEST_ENV": "visible",
	}, workdir)

	got := envMap(env)
	if got["PWD"] != workdir {
		t.Fatalf("expected PWD %q, got %q", workdir, got["PWD"])
	}
	if got["HIVERYN_TEST_ENV"] != "visible" {
		t.Fatalf("expected extra env to be preserved, got %q", got["HIVERYN_TEST_ENV"])
	}
}

func newTestTerminalProcess(t *testing.T) *terminalProcess {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "terminal-process-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	return &terminalProcess{
		key:        terminalKey("session-1", "term-1"),
		terminalID: "term-1",
		pty:        file,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		outputSubs: map[uint64]chan []byte{},
		done:       make(chan struct{}),
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func assertOutputChunk(t *testing.T, output <-chan []byte, want string) {
	t.Helper()

	select {
	case got, ok := <-output:
		if !ok {
			t.Fatalf("output channel closed before receiving %q", want)
		}
		if string(got) != want {
			t.Fatalf("expected output %q, got %q", want, string(got))
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for output %q", want)
	}
}
