package actionpkg

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

var roomy = Limits{MaxBytes: 1 << 20, MaxFiles: 100}

// writeTree creates files (name -> content) under dir; a name ending in "/"
// is an empty directory, a name ending in "*" an executable file.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "*") {
			name, mode = strings.TrimSuffix(name, "*"), 0o755
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree returns every file's content and every empty directory under dir.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			entries, _ := os.ReadDir(p)
			if len(entries) == 0 {
				out[rel+"/"] = ""
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		if info.Mode()&0o100 != 0 {
			rel += "*"
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var sample = map[string]string{
	"report.md":           "# Report\n",
	"evidence/a.json":     `{"a":1}`,
	"evidence/deep/b.txt": "b",
	"bin/run.sh*":         "#!/bin/sh\necho hi\n",
	"empty/":              "",
}

func requireContentError(t *testing.T, err error, fragment string) {
	t.Helper()
	var content *Error
	if !errors.As(err, &content) {
		t.Fatalf("want a content error containing %q, got %v", fragment, err)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not mention %q", err, fragment)
	}
}

func TestLocalCopyFreezesTheTreeAndLeavesNoPartial(t *testing.T) {
	src := filepath.Join(t.TempDir(), "out")
	writeTree(t, src, sample)
	dst := filepath.Join(t.TempDir(), "packages", "exec-1")

	stats, err := Copy(context.Background(), Location{Path: src}, Location{Path: dst}, roomy)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 4 || stats.Dirs != 4 || stats.Bytes != int64(len("# Report\n")+len(`{"a":1}`)+1+len("#!/bin/sh\necho hi\n")) {
		t.Fatalf("stats %+v", stats)
	}
	if got := readTree(t, dst); len(got) != len(sample) || got["bin/run.sh*"] != sample["bin/run.sh*"] || got["empty/"] != "" {
		t.Fatalf("copy %v", got)
	}
	if _, err := os.Stat(dst + PartialSuffix); !os.IsNotExist(err) {
		t.Fatalf("partial left behind: %v", err)
	}

	// The package is a snapshot: later edits or deletion of the source never
	// reach it.
	writeTree(t, src, map[string]string{"report.md": "changed", "new.txt": "new"})
	if err := os.RemoveAll(filepath.Join(src, "evidence")); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, dst); got["report.md"] != "# Report\n" || got["evidence/a.json"] != `{"a":1}` || got["new.txt"] != "" {
		t.Fatalf("package changed with its source: %v", got)
	}
}

func TestLocalPackagesRejectLinksAndSpecialFiles(t *testing.T) {
	for name, prepare := range map[string]func(dir string) error{
		"symbolic link": func(dir string) error { return os.Symlink("/etc/hosts", filepath.Join(dir, "link")) },
		"named pipe":    func(dir string) error { return syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			writeTree(t, src, map[string]string{"ok.txt": "ok"})
			if err := prepare(src); err != nil {
				t.Fatal(err)
			}
			_, err := Preflight(context.Background(), Location{Path: src}, roomy)
			requireContentError(t, err, name)
			dst := filepath.Join(t.TempDir(), "pkg")
			_, err = Copy(context.Background(), Location{Path: src}, Location{Path: dst}, roomy)
			requireContentError(t, err, name)
			for _, p := range []string{dst, dst + PartialSuffix} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("%s exists after a refused copy: %v", p, err)
				}
			}
		})
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	_, err := Preflight(context.Background(), Location{Path: link}, roomy)
	requireContentError(t, err, "symbolic link")
	_, err = Preflight(context.Background(), Location{Path: filepath.Join(t.TempDir(), "absent")}, roomy)
	requireContentError(t, err, "does not exist")
}

func TestLimitsAreEnforcedExactly(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a": "12345", "b": "12345"})
	exact := Limits{MaxBytes: 10, MaxFiles: 2}
	if _, err := Preflight(context.Background(), Location{Path: src}, exact); err != nil {
		t.Fatalf("a package exactly at the limits: %v", err)
	}
	for name, limits := range map[string]Limits{
		"files": {MaxBytes: 10, MaxFiles: 1},
		"bytes": {MaxBytes: 9, MaxFiles: 2},
	} {
		_, err := Preflight(context.Background(), Location{Path: src}, limits)
		requireContentError(t, err, "limit")
		dst := filepath.Join(t.TempDir(), "pkg")
		_, err = Copy(context.Background(), Location{Path: src}, Location{Path: dst}, limits)
		requireContentError(t, err, "limit")
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Fatalf("%s: an over-limit copy appeared: %v", name, statErr)
		}
	}
}

func TestRemoteCopiesInEveryDirection(t *testing.T) {
	fake := sshtest.Install(t)
	ctx := context.Background()

	local := filepath.Join(t.TempDir(), "out")
	writeTree(t, local, sample)

	// local -> remote A
	onA := Location{Alias: "box-a", Path: filepath.Join(fake.Home("box-a"), "deliveries", "x")}
	stats, err := Copy(ctx, Location{Path: local}, onA, roomy)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, onA.Path); len(got) != len(sample) || got["bin/run.sh*"] != sample["bin/run.sh*"] {
		t.Fatalf("local -> remote: %v", got)
	}
	// remote A preflight counts what was copied.
	pre, err := Preflight(ctx, onA, roomy)
	if err != nil || pre != stats {
		t.Fatalf("remote preflight %+v (%v), copy %+v", pre, err, stats)
	}
	// remote A -> remote B, through this process.
	onB := Location{Alias: "box-b", Path: filepath.Join(fake.Home("box-b"), "packages", "x")}
	if _, err := Copy(ctx, onA, onB, roomy); err != nil {
		t.Fatal(err)
	}
	// remote B -> local
	back := filepath.Join(t.TempDir(), "x")
	if _, err := Copy(ctx, onB, Location{Path: back}, roomy); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, back); len(got) != len(sample) || got["evidence/deep/b.txt"] != "b" || got["empty/"] != "" {
		t.Fatalf("round trip: %v", got)
	}
	for _, p := range []string{onA.Path, onB.Path, back} {
		if _, err := os.Stat(p + PartialSuffix); !os.IsNotExist(err) {
			t.Fatalf("partial left at %s", p)
		}
	}
}

func TestRemoteSourcesRejectUnsafeEntriesAndLimits(t *testing.T) {
	fake := sshtest.Install(t)
	ctx := context.Background()
	home := fake.Home("box")
	cases := map[string]func(dir string) error{
		"symbolic link": func(dir string) error { return os.Symlink("/etc/hosts", filepath.Join(dir, "link")) },
		"named pipe":    func(dir string) error { return syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644) },
		"hard link":     func(dir string) error { return os.Link(filepath.Join(dir, "ok.txt"), filepath.Join(dir, "twin.txt")) },
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(home, strings.ReplaceAll(name, " ", "-"))
			writeTree(t, src, map[string]string{"ok.txt": "ok"})
			if err := prepare(src); err != nil {
				t.Fatal(err)
			}
			if name != "hard link" {
				_, err := Preflight(ctx, Location{Alias: "box", Path: src}, roomy)
				requireContentError(t, err, "only regular files and directories")
			}
			dst := filepath.Join(t.TempDir(), "pkg")
			_, err := Copy(ctx, Location{Alias: "box", Path: src}, Location{Path: dst}, roomy)
			if name == "hard link" {
				requireContentError(t, err, "hard link")
			} else {
				requireContentError(t, err, "only regular files and directories")
			}
			if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
				t.Fatalf("refused copy appeared: %v", statErr)
			}
		})
	}

	src := filepath.Join(home, "big")
	writeTree(t, src, map[string]string{"a": "12345", "b": "12345"})
	_, err := Preflight(ctx, Location{Alias: "box", Path: src}, Limits{MaxBytes: 9, MaxFiles: 5})
	requireContentError(t, err, "limit")
	_, err = Copy(ctx, Location{Alias: "box", Path: src}, Location{Path: filepath.Join(t.TempDir(), "p")}, Limits{MaxBytes: 10, MaxFiles: 1})
	requireContentError(t, err, "limit")

	_, err = Preflight(ctx, Location{Alias: "box", Path: filepath.Join(home, "absent")}, roomy)
	requireContentError(t, err, "not a directory")
}

func TestInterruptedRemoteTransferNeverAppearsComplete(t *testing.T) {
	fake := sshtest.Install(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "out")
	writeTree(t, src, sample)
	dst := Location{Alias: "box", Path: filepath.Join(fake.Home("box"), "deliveries", "x")}

	// The destination is unreachable: nothing appears there.
	fake.SetDown(t, "box", true)
	if _, err := Copy(ctx, Location{Path: src}, dst, roomy); err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("copy to an offline machine: %v", err)
	}
	fake.SetDown(t, "box", false)
	if _, err := os.Stat(dst.Path); !os.IsNotExist(err) {
		t.Fatalf("destination appeared: %v", err)
	}

	// A stale partial copy from an interrupted attempt is replaced by the
	// retry, which completes.
	writeTree(t, dst.Path+PartialSuffix, map[string]string{"stale.txt": "half"})
	if _, err := Copy(ctx, Location{Path: src}, dst, roomy); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, dst.Path); got["stale.txt"] != "" || got["report.md"] != sample["report.md"] {
		t.Fatalf("retry result %v", got)
	}

	// A cancelled copy leaves the completed destination untouched.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Copy(cancelled, Location{Path: src}, Location{Alias: "box", Path: filepath.Join(fake.Home("box"), "deliveries", "y")}, roomy); err == nil {
		t.Fatal("cancelled copy succeeded")
	}
	if _, err := os.Stat(filepath.Join(fake.Home("box"), "deliveries", "y")); !os.IsNotExist(err) {
		t.Fatalf("cancelled copy appeared: %v", err)
	}
}

func TestRemoveStaysInsideTheManagedRoot(t *testing.T) {
	fake := sshtest.Install(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "managed")
	victim := filepath.Join(t.TempDir(), "user-folder")
	writeTree(t, victim, map[string]string{"keep.txt": "keep"})
	for _, target := range []string{victim, root, filepath.Join(root, "..", "elsewhere")} {
		if err := Remove(ctx, Location{Path: target}, root); err == nil {
			t.Fatalf("Remove(%s) outside %s succeeded", target, root)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatal(err)
	}

	pkg := filepath.Join(root, "packages", "x")
	writeTree(t, pkg, map[string]string{"a": "a"})
	writeTree(t, pkg+PartialSuffix, map[string]string{"a": "a"})
	if err := Remove(ctx, Location{Path: pkg}, root); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, Location{Path: pkg}, root); err != nil {
		t.Fatalf("removing an absent package: %v", err)
	}

	remoteRoot := filepath.Join(fake.Home("box"), ".local/state/hiveryn-actions")
	remotePkg := filepath.Join(remoteRoot, "packages", "x")
	writeTree(t, remotePkg, map[string]string{"a": "a"})
	fake.SetDown(t, "box", true)
	if err := Remove(ctx, Location{Alias: "box", Path: remotePkg}, remoteRoot); err == nil {
		t.Fatal("removal on an offline machine reported success")
	}
	fake.SetDown(t, "box", false)
	if err := Remove(ctx, Location{Alias: "box", Path: remotePkg}, remoteRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(remotePkg); !os.IsNotExist(err) {
		t.Fatalf("remote package remains: %v", err)
	}
}

func TestRemoteHomeAndMkdirEmpty(t *testing.T) {
	fake := sshtest.Install(t)
	ctx := context.Background()
	home, err := RemoteHome(ctx, "box")
	if err != nil || home != fake.Home("box") {
		t.Fatalf("home %q, %v", home, err)
	}
	dir := Location{Alias: "box", Path: filepath.Join(home, "runs", "a", "1")}
	if err := MkdirEmpty(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := MkdirEmpty(ctx, dir); err == nil {
		t.Fatal("an existing directory was accepted as new")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatal(err)
	}
}
