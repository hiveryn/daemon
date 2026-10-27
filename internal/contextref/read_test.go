package contextref

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture: an architect workspace and a local repo on this machine, a repo
// "infra" on machine bk (served from a local directory by the fake ssh) and
// a directory outside every root that symlinks point into.
type fixture struct {
	workspace, local, remote, outside string
	scope                             Scope
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{workspace: t.TempDir(), local: t.TempDir(), remote: t.TempDir(), outside: t.TempDir()}
	write(t, filepath.Join(f.outside, "secret.env"), "TOKEN=x\n")
	for _, root := range []string{f.workspace, f.remote} {
		write(t, filepath.Join(root, "OVERVIEW.md"), "line 1\nline 2\nline 3\n")
		write(t, filepath.Join(root, "knowledge", "README.md"), "See [spec](../specs/a.md).\n")
		write(t, filepath.Join(root, "specs", "a.md"), "spec\n")
		write(t, filepath.Join(root, "bin.dat"), "a\x00b")
		if err := os.Symlink(filepath.Join(f.outside, "secret.env"), filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.outside, filepath.Join(root, "escape-dir")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("specs/a.md", filepath.Join(root, "inner")); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{
		Machines: map[string]config.MachineConfig{"bk": {SSH: "bk-alias"}},
		Architects: map[string]config.ArchitectConfig{"p": {
			Path:         f.workspace,
			Repos:        map[string]string{"core": f.local, "infra": f.remote, "lost": "/nowhere"},
			RepoMachines: map[string]string{"infra": "bk", "lost": "gone"},
		}},
	}
	scope, err := ScopeFor(cfg, "p")
	if err != nil {
		t.Fatal(err)
	}
	f.scope = scope
	return f
}

func read(t *testing.T, r *Reader, f fixture, req domain.ContextReadRequest) domain.ContextFile {
	t.Helper()
	out, err := r.ReadFile(context.Background(), f.scope, req)
	if err != nil {
		t.Fatalf("ReadFile(%+v): %v", req, err)
	}
	return out
}

func TestReadContextStatusesLocallyAndOverSSH(t *testing.T) {
	sshtest.Install(t)
	f := newFixture(t)
	r := NewReader()
	for _, owner := range []string{"architect", "infra"} {
		t.Run(owner, func(t *testing.T) {
			ref := func(p string) string { return owner + ":" + p }
			first := read(t, r, f, domain.ContextReadRequest{Reference: ref("OVERVIEW.md"), Limit: 2})
			if first.Status != domain.ContextOK || first.Content != "line 1\nline 2\n" || first.TotalLines != 3 || first.StartLine != 1 || first.EndLine != 2 || first.NextOffset != 3 || first.Size != 21 || first.ModifiedAt == nil || len(first.SHA256) != 64 {
				t.Fatalf("first page = %+v", first)
			}
			wantLocation := domain.ReferenceLocal
			if owner == "infra" {
				wantLocation = domain.ReferenceRemote
			}
			if first.Owner != owner || first.Location != wantLocation {
				t.Fatalf("target = %+v", first.ContextTarget)
			}
			second := read(t, r, f, domain.ContextReadRequest{Reference: ref("OVERVIEW.md"), Offset: 3, ExpectedSHA256: first.SHA256})
			if second.Status != domain.ContextOK || second.Content != "line 3\n" || second.NextOffset != 0 || second.SHA256 != first.SHA256 {
				t.Fatalf("second page = %+v", second)
			}
			root := f.workspace
			if owner == "infra" {
				root = f.remote
			}
			write(t, filepath.Join(root, "OVERVIEW.md"), "line 1\nchanged\nline 3\n")
			if changed := read(t, r, f, domain.ContextReadRequest{Reference: ref("OVERVIEW.md"), Offset: 3, ExpectedSHA256: first.SHA256}); changed.Status != domain.ContextChanged || changed.Content != "" || changed.SHA256 == first.SHA256 {
				t.Fatalf("changed = %+v", changed)
			}
			for path, want := range map[string]domain.ContextReadStatus{
				"knowledge/README.md":   domain.ContextOK,
				"inner":                 domain.ContextOK,
				"missing.md":            domain.ContextMissing,
				"missing/deeper.md":     domain.ContextMissing,
				"specs":                 domain.ContextNotFile,
				"bin.dat":               domain.ContextBinary,
				"escape":                domain.ContextOutsideRoot,
				"escape-dir/secret.env": domain.ContextOutsideRoot,
			} {
				got := read(t, r, f, domain.ContextReadRequest{Reference: ref(path)})
				if got.Status != want {
					t.Fatalf("%s = %+v, want %s", path, got, want)
				}
				if want != domain.ContextOK && got.Message == "" {
					t.Fatalf("%s has no message", path)
				}
				if strings.Contains(got.Content, "TOKEN") || strings.Contains(got.Message, "TOKEN") {
					t.Fatalf("escape leaked content: %+v", got)
				}
			}
		})
	}
}

func TestReadContextBoundsAndInputs(t *testing.T) {
	f := newFixture(t)
	r := NewReader()
	big := strings.Repeat("x", domain.MaxContextFileBytes+1)
	write(t, filepath.Join(f.workspace, "big.txt"), big)
	if got := read(t, r, f, domain.ContextReadRequest{Reference: "architect:big.txt"}); got.Status != domain.ContextTooLarge || got.Size != int64(len(big)) || got.Content != "" {
		t.Fatalf("too large = %+v", got)
	}
	// A page stops at the byte budget on a line boundary.
	line := strings.Repeat("y", 1000) + "\n"
	write(t, filepath.Join(f.workspace, "wide.txt"), strings.Repeat(line, 600))
	page := read(t, r, f, domain.ContextReadRequest{Reference: "architect:wide.txt", Limit: domain.MaxContextLines})
	if len(page.Content) > domain.MaxContextPageBytes || page.EndLine != domain.MaxContextPageBytes/len(line) || page.NextOffset != page.EndLine+1 {
		t.Fatalf("budget page = lines %d-%d next %d (%d bytes)", page.StartLine, page.EndLine, page.NextOffset, len(page.Content))
	}
	write(t, filepath.Join(f.workspace, "empty.md"), "")
	if got := read(t, r, f, domain.ContextReadRequest{Reference: "architect:empty.md"}); got.Status != domain.ContextOK || got.TotalLines != 0 || got.Content != "" {
		t.Fatalf("empty = %+v", got)
	}
	if got := read(t, r, f, domain.ContextReadRequest{Reference: "lost:x"}); got.Status != domain.ContextUnreachable || got.Location != domain.ReferenceUnknown || !strings.Contains(got.Message, `machine "gone"`) {
		t.Fatalf("unlocatable repo = %+v", got)
	}

	for _, bad := range []domain.ContextReadRequest{
		{Reference: f.workspace + "/OVERVIEW.md"},
		{Reference: "OVERVIEW.md"},
		{Reference: "architect:../x"},
		{Reference: "other:x"},
		{Reference: "2026-10-27-0534-a-ticket"},
		{Reference: "architect:OVERVIEW.md", Offset: -1},
		{Reference: "architect:OVERVIEW.md", Limit: domain.MaxContextLines + 1},
	} {
		_, err := r.ReadFile(context.Background(), f.scope, bad)
		var validation *domain.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("ReadFile(%+v) err = %v, want validation", bad, err)
		}
	}
}

func TestListContextLocallyAndOverSSH(t *testing.T) {
	sshtest.Install(t)
	f := newFixture(t)
	r := NewReader()
	for _, owner := range []string{"architect", "infra"} {
		t.Run(owner, func(t *testing.T) {
			page, err := r.List(context.Background(), f.scope, domain.ContextListRequest{Reference: owner + ":.", Limit: 4})
			if err != nil {
				t.Fatal(err)
			}
			if page.Status != domain.ContextOK || page.TotalEntries != 7 || page.NextOffset != 4 || len(page.SHA256) != 64 {
				t.Fatalf("page = %+v", page)
			}
			var names []string
			for _, e := range page.Entries {
				names = append(names, e.Name+"="+string(e.Type)+"="+e.Reference)
			}
			want := []string{"OVERVIEW.md=file=" + owner + ":OVERVIEW.md", "bin.dat=file=" + owner + ":bin.dat", "escape=symlink=" + owner + ":escape", "escape-dir=symlink=" + owner + ":escape-dir"}
			if !slices.Equal(names, want) {
				t.Fatalf("entries = %v, want %v", names, want)
			}
			rest, err := r.List(context.Background(), f.scope, domain.ContextListRequest{Reference: owner + ":.", Offset: 4, ExpectedSHA256: page.SHA256})
			if err != nil || rest.Status != domain.ContextOK || len(rest.Entries) != 3 || rest.NextOffset != 0 || rest.Entries[0].Name != "inner" || rest.Entries[1].Type != domain.ContextEntryDirectory {
				t.Fatalf("rest = %+v %v", rest, err)
			}
			root := f.workspace
			if owner == "infra" {
				root = f.remote
			}
			write(t, filepath.Join(root, "new.md"), "x")
			if changed, _ := r.List(context.Background(), f.scope, domain.ContextListRequest{Reference: owner + ":.", Offset: 4, ExpectedSHA256: page.SHA256}); changed.Status != domain.ContextChanged {
				t.Fatalf("changed = %+v", changed)
			}
			for ref, want := range map[string]domain.ContextReadStatus{
				owner + ":knowledge":   domain.ContextOK,
				owner + ":OVERVIEW.md": domain.ContextNotDirectory,
				owner + ":missing":     domain.ContextMissing,
				owner + ":escape-dir":  domain.ContextOutsideRoot,
			} {
				got, err := r.List(context.Background(), f.scope, domain.ContextListRequest{Reference: ref})
				if err != nil || got.Status != want {
					t.Fatalf("%s = %+v %v, want %s", ref, got, err, want)
				}
			}
		})
	}
}

func TestRemoteContextUnreachableIsNeverMissing(t *testing.T) {
	fake := sshtest.Install(t)
	f := newFixture(t)
	fake.SetDown(t, "bk-alias", true)
	r := NewReader()
	got := read(t, r, f, domain.ContextReadRequest{Reference: "infra:OVERVIEW.md"})
	if got.Status != domain.ContextUnreachable || got.Location != domain.ReferenceRemote || got.Machine != "bk" || !strings.Contains(got.Message, `machine "bk"`) {
		t.Fatalf("down read = %+v", got)
	}
	listing, err := r.List(context.Background(), f.scope, domain.ContextListRequest{Reference: "infra:."})
	if err != nil || listing.Status != domain.ContextUnreachable {
		t.Fatalf("down list = %+v %v", listing, err)
	}
	// The same paths exist under the architect workspace on this machine and
	// are never consulted instead.
	if got.Content != "" {
		t.Fatalf("fallback content = %q", got.Content)
	}
}
