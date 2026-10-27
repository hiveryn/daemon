package remoteexec_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/remoteexec"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

func TestConnectionSharesOneHandshakeWithinItsOwnerOnly(t *testing.T) {
	fake := sshtest.Install(t)
	tmp := privateTemp(t)
	c, err := remoteexec.Dial(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 1 {
		t.Fatalf("control directory: %v", entries)
	}
	ctx := remoteexec.WithConnection(context.Background(), c)
	c.Stage("probe")
	for i := 0; i < 3; i++ {
		if out, err := remoteexec.Run(ctx, "box", "printf ok", nil); err != nil || string(out) != "ok" {
			t.Fatalf("shared run: %q %v", out, err)
		}
	}
	// Another alias, and a context without the connection, connect on their own.
	if _, err := remoteexec.Run(ctx, "other", "true", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteexec.Run(context.Background(), "box", "true", nil); err != nil {
		t.Fatal(err)
	}
	c.Stage("fail")
	if _, err := remoteexec.Run(ctx, "box", "exit 3", nil); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("remote failure: %v", err)
	}
	fresh, masters, shared := fake.Connections("box")
	if fresh != 1 || masters != 1 || shared != 4 {
		t.Fatalf("box connections fresh=%d masters=%d shared=%d", fresh, masters, shared)
	}
	stats := c.Stats()
	if stats.Operations != 4 || stats.Failed != 1 || stats.Direct != 0 || stats.Stage != "fail" || len(stats.Stages) != 2 || stats.Stages[0].Operations != 3 {
		t.Fatalf("stats: %+v", stats)
	}
	if s := stats.String(); strings.Contains(s, "printf") || !strings.Contains(s, "probe") {
		t.Fatalf("stats must name stages, never scripts: %s", s)
	}

	c.Close()
	c.Close()
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("control directory left after close: %v", entries)
	}
	// After Close the connection's commands connect directly again, still counted.
	if _, err := remoteexec.Run(ctx, "box", "true", nil); err != nil {
		t.Fatal(err)
	}
	if fresh, _, _ := fake.Connections("box"); fresh != 2 || c.Stats().Direct != 1 {
		t.Fatalf("after close fresh=%d stats=%+v", fresh, c.Stats())
	}
}

func TestDialFailureNamesSSHReasonAndLeavesNothing(t *testing.T) {
	fake := sshtest.Install(t)
	fake.SetDown(t, "box", true)
	tmp := privateTemp(t)
	c, err := remoteexec.Dial(context.Background(), "box")
	t.Cleanup(c.Close) // Dial returns a connection to close even on error.
	if err == nil || !strings.Contains(err.Error(), "SSH box: master exited") || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("dial: %v", err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("control directory left behind: %v", entries)
	}
	// Its commands connect directly, keep their own errors and are counted.
	fake.SetDown(t, "box", false)
	ctx := remoteexec.WithConnection(context.Background(), c)
	if _, err := remoteexec.Run(ctx, "box", "true", nil); err != nil {
		t.Fatal(err)
	}
	if stats := c.Stats(); stats.Direct != 1 || stats.Operations != 0 {
		t.Fatalf("fallback stats: %+v", stats)
	}
}

func TestDialIsBoundedByItsContext(t *testing.T) {
	fake := sshtest.Install(t)
	fake.SetLatency(t, 5*time.Second, 0)
	tmp := privateTemp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	c, err := remoteexec.Dial(ctx, "box")
	t.Cleanup(c.Close)
	if err == nil || !strings.Contains(err.Error(), "master not ready: context deadline exceeded") {
		t.Fatalf("dial: %v", err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("dial not bounded: %s", time.Since(started))
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("control directory left behind: %v", entries)
	}
}

// privateTemp points os.TempDir at a short directory of the test's own, so
// control directories can be inspected without racing other tests.
func privateTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hvt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	return dir
}
