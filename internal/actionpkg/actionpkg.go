// Package actionpkg freezes and moves Action artifact packages: directory
// trees of regular files and directories only, bounded in file count and
// bytes. A package lives on one location — the local filesystem or an SSH
// machine — and every copy, whether a snapshot on the execution machine or a
// delivery to a caller on another machine, is the same pipeline:
//
//	source   a local walk, or `tar -cf -` streamed over SSH
//	check    every entry validated here: regular files and directories only,
//	         safe relative names, exact file/directory/byte limits enforced
//	         before any content is written
//	sink     a local writer, or `tar -xf -` over SSH, into <dest>.partial
//	commit   the partial copy is recounted on its machine and renamed to
//	         <dest> only when it matches what was sent
//
// A partial copy is therefore never at the destination path, and a copy
// between two remote machines passes through this process without either
// machine needing access to the other. Remote paths are never interpreted
// locally. Content problems (unsafe entries, limits, a missing directory) are
// *Error values the caller can act on; transport failures keep their cause.
package actionpkg

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hiveryn/daemon/internal/remoteexec"
)

// Location is a directory on the local filesystem (Alias empty) or on the SSH
// machine reached through Alias.
type Location struct {
	Alias string
	Path  string
}

func (l Location) remote() bool { return l.Alias != "" }

func (l Location) String() string {
	if l.remote() {
		return l.Alias + ":" + l.Path
	}
	return l.Path
}

// Limits bound one package. Directories are bounded by MaxFiles too, so a
// tree of empty directories cannot evade the file limit.
type Limits struct {
	MaxBytes int64
	MaxFiles int
}

// Stats counts a package: regular files, directories below the root, and the
// bytes of file content.
type Stats struct {
	Files int
	Dirs  int
	Bytes int64
}

// Error is a problem with the package content itself — an unsafe entry, an
// exceeded limit, a source that is not a directory — that its author can fix.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func contentError(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// PartialSuffix names the in-progress copy beside a destination.
const PartialSuffix = ".partial"

// abortTimeout bounds removing a failed remote partial copy.
const abortTimeout = 30 * time.Second

type entry struct {
	name string // slash-separated, relative, cleaned
	dir  bool
	exec bool
	size int64
	body io.Reader
}

// counter enforces the limits as entries arrive, before their content moves.
type counter struct {
	limits Limits
	stats  Stats
}

func (c *counter) admit(e entry) error {
	if e.dir {
		c.stats.Dirs++
		if c.stats.Dirs > c.limits.MaxFiles {
			return contentError("the package has more than %d directories; the limit is %d", c.limits.MaxFiles, c.limits.MaxFiles)
		}
		return nil
	}
	c.stats.Files++
	if c.stats.Files > c.limits.MaxFiles {
		return contentError("the package has more than %d files; the limit is %d — deliver fewer files (for example an archive of them)", c.limits.MaxFiles, c.limits.MaxFiles)
	}
	c.stats.Bytes += e.size
	if c.stats.Bytes > c.limits.MaxBytes {
		return contentError("the package exceeds %d bytes (reached at %s); the limit is %d bytes — deliver less data", c.limits.MaxBytes, e.name, c.limits.MaxBytes)
	}
	return nil
}

// Preflight checks that src is a directory holding a valid package within
// limits, without copying it.
func Preflight(ctx context.Context, src Location, limits Limits) (Stats, error) {
	if src.remote() {
		return remotePreflight(ctx, src, limits)
	}
	c := &counter{limits: limits}
	err := walkLocal(src.Path, func(e entry) error {
		if err := c.admit(e); err != nil {
			return err
		}
		if e.body != nil {
			if closer, ok := e.body.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		return nil
	}, false)
	return c.stats, err
}

// Copy copies the package at src to dst.Path through dst.Path+".partial",
// enforcing limits on every entry, and renames the verified copy into place.
// Any earlier partial copy at dst is replaced; an existing dst.Path is
// replaced only by a complete, verified copy. On failure the partial copy is
// removed (best effort; Remove retries it) and dst.Path is untouched.
func Copy(ctx context.Context, src, dst Location, limits Limits) (Stats, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sink, err := openSink(ctx, dst)
	if err != nil {
		return Stats{}, err
	}
	c := &counter{limits: limits}
	add := func(e entry) error {
		if err := c.admit(e); err != nil {
			return err
		}
		return sink.add(e)
	}
	if src.remote() {
		err = walkRemote(ctx, src, add)
	} else {
		err = walkLocal(src.Path, add, true)
	}
	if err != nil {
		cancel()
		sink.abort()
		return Stats{}, err
	}
	if err := sink.commit(c.stats); err != nil {
		cancel()
		sink.abort()
		return Stats{}, err
	}
	return c.stats, nil
}

// Remove deletes the directory at loc and any partial copy beside it. It
// refuses any path outside managedRoot, so only Hiveryn-managed directories
// can ever be removed. A missing directory is not an error.
func Remove(ctx context.Context, loc Location, managedRoot string) error {
	clean := filepath.Clean
	if loc.remote() {
		clean = path.Clean
	}
	root := clean(managedRoot)
	target := clean(loc.Path)
	if managedRoot == "" || !strings.HasPrefix(target, root+"/") {
		return fmt.Errorf("refusing to remove %s: not inside the managed directory %s", loc, managedRoot)
	}
	if loc.remote() {
		script := "rm -rf -- " + remoteexec.Args(target+PartialSuffix, target)
		if _, err := remoteexec.Run(ctx, loc.Alias, script, nil); err != nil {
			return fmt.Errorf("remove %s: %w", loc, err)
		}
		return nil
	}
	for _, p := range []string{target + PartialSuffix, target} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// RemoteHome returns the SSH account's home directory, which roots the
// managed directories there.
func RemoteHome(ctx context.Context, alias string) (string, error) {
	out, err := remoteexec.Run(ctx, alias, `printf '%s' "$HOME"`, nil)
	if err != nil {
		return "", err
	}
	home := string(out)
	if !path.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") {
		return "", fmt.Errorf("SSH %s reported an unusable HOME %q", alias, home)
	}
	return path.Clean(home), nil
}

// MkdirEmpty creates dir (and its parents) as a new, empty directory; an
// existing dir is an error, so an agent's folder is always its own.
func MkdirEmpty(ctx context.Context, loc Location) error {
	if loc.remote() {
		script := "p=" + remoteexec.Quote(loc.Path) + `; mkdir -p -- "$(dirname -- "$p")" && mkdir -- "$p"`
		if _, err := remoteexec.Run(ctx, loc.Alias, script, nil); err != nil {
			return fmt.Errorf("create directory %s: %w", loc, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(loc.Path), 0o755); err != nil {
		return fmt.Errorf("create directory parent %s: %w", filepath.Dir(loc.Path), err)
	}
	if err := os.Mkdir(loc.Path, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", loc.Path, err)
	}
	return nil
}

// --- local source ----------------------------------------------------------

// walkLocal visits root's tree in lexical order. open controls whether file
// bodies are opened (a preflight only counts).
func walkLocal(root string, visit func(entry) error, open bool) error {
	info, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return contentError("%s does not exist", root)
		}
		return fmt.Errorf("inspect %s: %w", root, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return contentError("%s is a symbolic link; name the directory itself", root)
	}
	if !info.IsDir() {
		return contentError("%s is not a directory", root)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		switch t := d.Type(); {
		case t.IsDir():
			return visit(entry{name: name, dir: true})
		case t.IsRegular():
		default:
			return contentError("%s is %s; packages hold only regular files and directories — copy the content instead", name, describeMode(t))
		}
		if !open {
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("inspect %s: %w", p, err)
			}
			return visit(entry{name: name, size: info.Size(), exec: info.Mode()&0o111 != 0})
		}
		// O_NOFOLLOW: an entry swapped for a link after the walk saw it is
		// refused rather than followed.
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return contentError("%s changed while it was being packaged", name)
		}
		return visit(entry{name: name, size: info.Size(), exec: info.Mode()&0o111 != 0, body: &exactReader{r: f, name: name, remaining: info.Size()}})
	})
}

// exactReader yields exactly the declared size and fails if the file is
// shorter or longer than it was when its entry was admitted.
type exactReader struct {
	r         io.Reader
	name      string
	remaining int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.remaining == 0 {
		var probe [1]byte
		if n, _ := e.r.Read(probe[:]); n > 0 {
			return 0, contentError("%s grew while it was being packaged", e.name)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.r.Read(p)
	e.remaining -= int64(n)
	if err == io.EOF && e.remaining > 0 {
		return n, contentError("%s shrank while it was being packaged", e.name)
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}

func describeMode(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeDevice != 0:
		return "a device"
	default:
		return "not a regular file or directory"
	}
}

// --- remote source ---------------------------------------------------------

// remoteSourceCheck refuses a source that is not a directory and lists the
// first entries that are neither regular files nor directories; tar would
// otherwise archive links and skip sockets with only a warning.
func remoteSourceCheck(dir string) string {
	return "d=" + remoteexec.Quote(dir) + `
if test -L "$d" || ! test -d "$d"; then printf '%s\n' 'HIVERYN-NOT-A-DIRECTORY' >&2; exit 3; fi
cd -- "$d" || exit 3
bad=$(find . ! -type f ! -type d -print | head -n 5)
if test -n "$bad"; then printf '%s\n' "$bad" | sed 's/^/HIVERYN-UNSAFE-ENTRY /' >&2; exit 4; fi
`
}

func remoteContentError(loc Location, stderr string) error {
	switch {
	case strings.Contains(stderr, "HIVERYN-NOT-A-DIRECTORY"):
		return contentError("%s is not a directory on that machine (a symbolic link must name the directory itself)", loc.Path)
	case strings.Contains(stderr, "HIVERYN-UNSAFE-ENTRY"):
		var names []string
		// The first line may carry the transport's own prefix.
		for _, line := range strings.Split(stderr, "\n") {
			if _, rest, ok := strings.Cut(line, "HIVERYN-UNSAFE-ENTRY "); ok {
				names = append(names, strings.TrimPrefix(rest, "./"))
			}
		}
		return contentError("packages hold only regular files and directories, but %s holds: %s — remove them or copy their content instead", loc.Path, strings.Join(names, ", "))
	}
	return nil
}

func remotePreflight(ctx context.Context, src Location, limits Limits) (Stats, error) {
	script := remoteSourceCheck(src.Path) + `
files=$(find . -type f -exec sh -c 'echo $#' sh {} + | awk '{s+=$1} END{print s+0}')
dirs=$(find . -type d ! -name . -exec sh -c 'echo $#' sh {} + | awk '{s+=$1} END{print s+0}')
bytes=$(find . -type f -exec wc -c {} + | awk '$2!="total"{s+=$1} END{print s+0}')
printf '%s %s %s\n' "$files" "$dirs" "$bytes"
`
	out, err := remoteexec.Run(ctx, src.Alias, script, nil)
	if err != nil {
		if cerr := remoteContentError(src, err.Error()); cerr != nil {
			return Stats{}, cerr
		}
		return Stats{}, fmt.Errorf("check %s: %w", src, err)
	}
	stats, err := parseStats(out)
	if err != nil {
		return Stats{}, fmt.Errorf("check %s: %w", src, err)
	}
	switch {
	case stats.Files > limits.MaxFiles:
		return stats, contentError("the package has %d files; the limit is %d — deliver fewer files (for example an archive of them)", stats.Files, limits.MaxFiles)
	case stats.Dirs > limits.MaxFiles:
		return stats, contentError("the package has %d directories; the limit is %d", stats.Dirs, limits.MaxFiles)
	case stats.Bytes > limits.MaxBytes:
		return stats, contentError("the package is %d bytes; the limit is %d bytes — deliver less data", stats.Bytes, limits.MaxBytes)
	}
	return stats, nil
}

func parseStats(out []byte) (Stats, error) {
	fields := strings.Fields(string(out))
	if len(fields) != 3 {
		return Stats{}, fmt.Errorf("unexpected count response %q", strings.TrimSpace(string(out)))
	}
	files, err1 := strconv.Atoi(fields[0])
	dirs, err2 := strconv.Atoi(fields[1])
	size, err3 := strconv.ParseInt(fields[2], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return Stats{}, fmt.Errorf("unexpected count response %q: %w", strings.TrimSpace(string(out)), err)
	}
	return Stats{Files: files, Dirs: dirs, Bytes: size}, nil
}

// walkRemote streams the remote tree as a tar archive and visits each entry
// after validating it. Any warning from the remote tar fails the copy: an
// entry it skipped would otherwise be a silent omission.
func walkRemote(ctx context.Context, src Location, visit func(entry) error) error {
	script := remoteSourceCheck(src.Path) + "COPYFILE_DISABLE=1 exec tar -cf - .\n"
	cmd := remoteexec.Command(ctx, src.Alias, script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	walkErr := readTar(stdout, visit)
	if walkErr != nil {
		// Stop the remote tar; its exit is no longer interesting.
		_ = cmd.Process.Kill()
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		if cerr := remoteContentError(src, stderr.String()); cerr != nil {
			return cerr
		}
		return walkErr
	}
	waitErr := cmd.Wait()
	if cerr := remoteContentError(src, stderr.String()); cerr != nil {
		return cerr
	}
	if waitErr != nil {
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return fmt.Errorf("read %s: %w (ssh %v): %s", src, ctxErr, waitErr, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("read %s: %w: %s", src, waitErr, strings.TrimSpace(stderr.String()))
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return contentError("reading %s reported problems, so the package would be incomplete: %s", src.Path, msg)
	}
	return nil
}

func readTar(r io.Reader, visit func(entry) error) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read package stream: %w", err)
		}
		name, ok := cleanName(hdr.Name)
		if !ok {
			return contentError("unsafe entry name %q", hdr.Name)
		}
		if name == "" {
			continue // the root itself
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := visit(entry{name: name, dir: true}); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what old tars write for regular files.
			if hdr.Size < 0 {
				return contentError("%s has an invalid size", name)
			}
			if err := visit(entry{name: name, size: hdr.Size, exec: hdr.Mode&0o111 != 0, body: tr}); err != nil {
				return err
			}
		case tar.TypeSymlink:
			return contentError("%s is a symbolic link; packages hold only regular files and directories — copy the content instead", name)
		case tar.TypeLink:
			return contentError("%s is a hard link to %s; packages hold only independent regular files — copy the file instead", name, hdr.Linkname)
		default:
			return contentError("%s is not a regular file or directory (tar type %q); packages hold only regular files and directories", name, hdr.Typeflag)
		}
	}
}

// cleanName turns an archive name into a safe relative name; "" is the root.
func cleanName(raw string) (string, bool) {
	if raw == "" || strings.HasPrefix(raw, "/") || strings.ContainsRune(raw, 0) {
		return "", false
	}
	name := path.Clean(strings.TrimSuffix(raw, "/"))
	if name == "." {
		return "", true
	}
	if name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

// --- sinks -----------------------------------------------------------------

type sink interface {
	add(entry) error
	commit(Stats) error
	abort()
}

func openSink(ctx context.Context, dst Location) (sink, error) {
	if dst.remote() {
		return openRemoteSink(ctx, dst)
	}
	return openLocalSink(dst.Path)
}

type localSink struct {
	final, partial string
}

func openLocalSink(final string) (*localSink, error) {
	s := &localSink{final: final, partial: final + PartialSuffix}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(final), err)
	}
	if err := os.RemoveAll(s.partial); err != nil {
		return nil, fmt.Errorf("remove earlier partial copy %s: %w", s.partial, err)
	}
	if err := os.Mkdir(s.partial, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", s.partial, err)
	}
	return s, nil
}

func (s *localSink) add(e entry) error {
	target := filepath.Join(s.partial, filepath.FromSlash(e.name))
	if e.dir {
		if err := os.MkdirAll(target, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	mode := os.FileMode(0o600)
	if e.exec {
		mode = 0o700
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	n, err := io.Copy(f, e.body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		var content *Error
		if errors.As(err, &content) {
			return err
		}
		return fmt.Errorf("write %s: %w", target, err)
	}
	if n != e.size {
		return contentError("%s ended after %d of %d bytes", e.name, n, e.size)
	}
	return nil
}

func (s *localSink) commit(want Stats) error {
	got := &counter{limits: Limits{MaxBytes: want.Bytes, MaxFiles: max(want.Files, want.Dirs)}}
	if err := walkLocal(s.partial, got.admit, false); err != nil {
		return fmt.Errorf("verify %s: %w", s.partial, err)
	}
	if got.stats != want {
		return fmt.Errorf("verify %s: holds %+v, sent %+v", s.partial, got.stats, want)
	}
	if err := os.RemoveAll(s.final); err != nil {
		return fmt.Errorf("replace %s: %w", s.final, err)
	}
	if err := os.Rename(s.partial, s.final); err != nil {
		return fmt.Errorf("complete %s: %w", s.final, err)
	}
	return nil
}

func (s *localSink) abort() { _ = os.RemoveAll(s.partial) }

type remoteSink struct {
	ctx            context.Context
	dst            Location
	partial        string
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	tw             *tar.Writer
	stderr         *bytes.Buffer
	finished, done bool
}

func openRemoteSink(ctx context.Context, dst Location) (*remoteSink, error) {
	partial := dst.Path + PartialSuffix
	script := "umask 077; p=" + remoteexec.Quote(partial) + `
mkdir -p -- "$(dirname -- "$p")" || exit 1
rm -rf -- "$p" || exit 1
mkdir -- "$p" || exit 1
cd -- "$p" || exit 1
exec tar -xf -
`
	cmd := remoteexec.Command(ctx, dst.Alias, script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("write %s: %w", dst, err)
	}
	return &remoteSink{
		ctx: ctx, dst: dst, partial: partial, cmd: cmd, stdin: stdin, tw: tar.NewWriter(stdin), stderr: &stderr,
	}, nil
}

func (s *remoteSink) add(e entry) error {
	hdr := &tar.Header{Name: e.name, Format: tar.FormatPAX}
	if e.dir {
		hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, e.name+"/", 0o700
		if err := s.tw.WriteHeader(hdr); err != nil {
			return s.transportError(err)
		}
		return nil
	}
	hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, e.size, 0o600
	if e.exec {
		hdr.Mode = 0o700
	}
	if err := s.tw.WriteHeader(hdr); err != nil {
		return s.transportError(err)
	}
	n, err := io.Copy(s.tw, e.body)
	if err != nil {
		var content *Error
		if errors.As(err, &content) {
			return err
		}
		return s.transportError(err)
	}
	if n != e.size {
		return contentError("%s ended after %d of %d bytes", e.name, n, e.size)
	}
	return nil
}

// transportError explains a broken stream with what the remote side said.
func (s *remoteSink) transportError(err error) error {
	_ = s.finish()
	return fmt.Errorf("write %s: %w: %s", s.dst, err, strings.TrimSpace(s.stderr.String()))
}

// finish closes the stream and waits for the remote tar.
func (s *remoteSink) finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	closeErr := s.tw.Close()
	_ = s.stdin.Close()
	if err := s.cmd.Wait(); err != nil {
		return fmt.Errorf("write %s: %w: %s", s.dst, err, strings.TrimSpace(s.stderr.String()))
	}
	if closeErr != nil {
		return fmt.Errorf("write %s: %w", s.dst, closeErr)
	}
	return nil
}

func (s *remoteSink) commit(want Stats) error {
	if err := s.finish(); err != nil {
		return err
	}
	// Recount on the destination machine, then rename only a matching copy.
	script := "p=" + remoteexec.Quote(s.partial) + "; f=" + remoteexec.Quote(s.dst.Path) + `
cd -- "$p" || exit 1
bad=$(find . ! -type f ! -type d -print | head -n 1)
if test -n "$bad"; then printf 'unexpected entry %s\n' "$bad" >&2; exit 4; fi
files=$(find . -type f -exec sh -c 'echo $#' sh {} + | awk '{s+=$1} END{print s+0}')
dirs=$(find . -type d ! -name . -exec sh -c 'echo $#' sh {} + | awk '{s+=$1} END{print s+0}')
bytes=$(find . -type f -exec cat {} + | wc -c | tr -d ' ')
if test "$files $dirs $bytes" != ` + remoteexec.Quote(fmt.Sprintf("%d %d %d", want.Files, want.Dirs, want.Bytes)) + `; then
  printf 'holds %s files, %s directories, %s bytes\n' "$files" "$dirs" "$bytes" >&2; exit 4
fi
cd / && rm -rf -- "$f" && mv -- "$p" "$f"
`
	if _, err := remoteexec.Run(s.ctx, s.dst.Alias, script, nil); err != nil {
		return fmt.Errorf("verify %s (sent %d files, %d directories, %d bytes): %w", s.dst, want.Files, want.Dirs, want.Bytes, err)
	}
	s.done = true
	return nil
}

func (s *remoteSink) abort() {
	if s.done {
		return
	}
	if !s.finished {
		_ = s.cmd.Process.Kill()
		_ = s.finish()
	}
	// Best effort on a fresh context: the copy's own may be exhausted. A
	// partial copy left behind is removed by the next Remove of dst.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), abortTimeout)
	defer cancel()
	_, _ = remoteexec.Run(ctx, s.dst.Alias, "rm -rf -- "+remoteexec.Quote(s.partial), nil)
}
