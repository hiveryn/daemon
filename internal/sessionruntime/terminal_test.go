package sessionruntime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

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

func TestTerminalProcessClosesSlowSubscriberWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	process := newTestTerminalProcess(t)
	attachment, err := process.attach()
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	for i := 0; i < outputQueueSize+1; i++ {
		process.broadcast([]byte("x"))
	}

	count := 0
	for range attachment.Output() {
		count++
	}
	if count != outputQueueSize {
		t.Fatalf("expected %d queued chunks before close, got %d", outputQueueSize, count)
	}
}

func TestPTYTerminalManagerReplaysStartupOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}

	manager := newPTYTerminalManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if err := manager.Start(ctx, terminalStartSpec{
		ID:      "session-1",
		Command: "/bin/sh",
		Args:    []string{"-c", "printf boot; sleep 1"},
		Size:    terminalSize{Cols: 80, Rows: 24},
	}); err != nil {
		t.Fatalf("start terminal: %v", err)
	}
	defer func() {
		killCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Kill(killCtx, "session-1")
	}()

	time.Sleep(100 * time.Millisecond)

	attachment, err := manager.Attach(ctx, "session-1")
	if err != nil {
		t.Fatalf("attach terminal: %v", err)
	}
	defer func() { _ = attachment.Close() }()

	assertOutputChunk(t, attachment.Output(), "boot")
}

func newTestTerminalProcess(t *testing.T) *terminalProcess {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "terminal-process-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	return &terminalProcess{
		id:         "session-1",
		pty:        file,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		outputSubs: map[uint64]chan []byte{},
		done:       make(chan struct{}),
	}
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
