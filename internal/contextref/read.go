package contextref

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/remoteexec"
)

// remoteReadTimeout bounds one SSH read or listing.
const remoteReadTimeout = 30 * time.Second

// Reader reads context files and directories where they live. Remote runs
// the SSH script; tests replace it.
type Reader struct {
	Remote func(ctx context.Context, alias, script string) ([]byte, error)
}

func NewReader() *Reader {
	return &Reader{Remote: func(ctx context.Context, alias, script string) ([]byte, error) {
		return remoteexec.Run(ctx, alias, script, nil)
	}}
}

// rawFile is a whole file as read on its machine, or why it could not be.
type rawFile struct {
	status  domain.ContextReadStatus
	message string
	data    []byte
	size    int64
	mod     time.Time
}

// rawEntry is one directory entry as listed on its machine.
type rawEntry struct {
	name string
	typ  domain.ContextEntryType
	size int64
	mod  time.Time
}

type rawListing struct {
	status  domain.ContextReadStatus
	message string
	entries []rawEntry
}

func target(ref domain.ParsedReference, root Root) domain.ContextTarget {
	return domain.ContextTarget{Reference: ref.String(), Owner: root.Owner, Location: root.Location(), Machine: root.Machine}
}

// ReadFile returns one page of a text file. Invalid references and unknown
// owners are errors (bad input); every filesystem outcome is a status.
func (r *Reader) ReadFile(ctx context.Context, scope Scope, req domain.ContextReadRequest) (domain.ContextFile, error) {
	ref, root, err := scope.ParseContext(req.Reference)
	if err != nil {
		return domain.ContextFile{}, &domain.ValidationError{Field: "reference", Message: err.Error()}
	}
	offset, limit := req.Offset, req.Limit
	if offset == 0 {
		offset = 1
	}
	if limit == 0 {
		limit = domain.DefaultContextLines
	}
	if offset < 1 {
		return domain.ContextFile{}, &domain.ValidationError{Field: "offset", Message: "must be a 1-based line number"}
	}
	if limit < 1 || limit > domain.MaxContextLines {
		return domain.ContextFile{}, &domain.ValidationError{Field: "limit", Message: fmt.Sprintf("must be between 1 and %d lines", domain.MaxContextLines)}
	}
	out := domain.ContextFile{ContextTarget: target(ref, root)}
	if root.Err != nil {
		out.Status, out.Message = domain.ContextUnreachable, root.Err.Error()
		return out, nil
	}
	var raw rawFile
	if root.Machine == "" {
		raw = readLocalFile(root, ref.Target)
	} else {
		raw = r.readRemoteFile(ctx, root, ref.Target)
	}
	out.Status, out.Message = raw.status, raw.message
	if raw.status != domain.ContextOK && raw.status != domain.ContextTooLarge {
		return out, nil
	}
	out.Size = raw.size
	if !raw.mod.IsZero() {
		mod := raw.mod.UTC()
		out.ModifiedAt = &mod
	}
	if raw.status == domain.ContextTooLarge {
		return out, nil
	}
	out.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw.data))
	if req.ExpectedSHA256 != "" && req.ExpectedSHA256 != out.SHA256 {
		out.Status = domain.ContextChanged
		out.Message = "the file changed since the page with sha256 " + req.ExpectedSHA256 + " was read; read again from offset 1 (with expected_sha256 " + out.SHA256 + " for later pages)"
		return out, nil
	}
	if bytes.IndexByte(raw.data, 0) >= 0 || !utf8.Valid(raw.data) {
		out.Status = domain.ContextBinary
		out.Message = "the file is not UTF-8 text; binary and other encodings are not readable as context"
		return out, nil
	}
	paginate(&out, string(raw.data), offset, limit)
	return out, nil
}

// paginate fills lines [offset, offset+limit) within MaxContextPageBytes.
func paginate(out *domain.ContextFile, content string, offset, limit int) {
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	out.TotalLines = len(lines)
	if offset > len(lines) {
		if len(lines) == 0 && offset == 1 {
			return // an empty file: ok, no content
		}
		out.Status = domain.ContextOK
		out.Message = fmt.Sprintf("offset %d is past the end of the file (%d lines)", offset, len(lines))
		return
	}
	var page strings.Builder
	end := offset - 1
	for end < len(lines) && end-(offset-1) < limit {
		line := lines[end]
		if page.Len()+len(line) > domain.MaxContextPageBytes {
			if page.Len() == 0 {
				// One line longer than a page: cut it on a rune boundary.
				cut := domain.MaxContextPageBytes
				for cut > 0 && !utf8.RuneStart(line[cut]) {
					cut--
				}
				page.WriteString(line[:cut])
				out.Message = fmt.Sprintf("line %d is longer than %d bytes and was cut", end+1, domain.MaxContextPageBytes)
				end++
			}
			break
		}
		page.WriteString(line)
		end++
	}
	out.Content = page.String()
	out.StartLine = offset
	out.EndLine = end
	if end < len(lines) {
		out.NextOffset = end + 1
	}
}

func readLocalFile(root Root, targetPath string) rawFile {
	real, err := canonicalLocal(root.Path, root.Join(targetPath))
	if f, ok := localFailure(err); ok {
		return f
	}
	info, err := os.Stat(real)
	if f, ok := localFailure(err); ok {
		return f
	}
	if info.IsDir() {
		return rawFile{status: domain.ContextNotFile, message: "the target is a directory; list it with listContext"}
	}
	if !info.Mode().IsRegular() {
		return rawFile{status: domain.ContextNotFile, message: "the target is not a regular file"}
	}
	if info.Size() > domain.MaxContextFileBytes {
		return rawFile{status: domain.ContextTooLarge, message: tooLarge(info.Size()), size: info.Size(), mod: info.ModTime()}
	}
	f, err := os.Open(real)
	if failure, ok := localFailure(err); ok {
		return failure
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, domain.MaxContextFileBytes+1))
	if err != nil {
		return rawFile{status: domain.ContextPermissionDenied, message: "read failed: " + err.Error()}
	}
	if len(data) > domain.MaxContextFileBytes {
		return rawFile{status: domain.ContextTooLarge, message: tooLarge(int64(len(data))), size: int64(len(data)), mod: info.ModTime()}
	}
	return rawFile{status: domain.ContextOK, data: data, size: int64(len(data)), mod: info.ModTime()}
}

func tooLarge(size int64) string {
	return fmt.Sprintf("the file is %d bytes; readContext reads files up to %d bytes", size, domain.MaxContextFileBytes)
}

// localFailure maps a local filesystem error to its status.
func localFailure(err error) (rawFile, bool) {
	switch {
	case err == nil:
		return rawFile{}, false
	case errors.Is(err, ErrOutsideRoot):
		return rawFile{status: domain.ContextOutsideRoot, message: "the target " + ErrOutsideRoot.Error() + " and is not readable as context"}, true
	case os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR):
		return rawFile{status: domain.ContextMissing, message: "no such file or directory"}, true
	case os.IsPermission(err):
		return rawFile{status: domain.ContextPermissionDenied, message: err.Error()}, true
	default:
		return rawFile{status: domain.ContextPermissionDenied, message: "cannot inspect: " + err.Error()}, true
	}
}

// remoteResolve is the shared head of the remote read and list scripts: it
// resolves $p within $root and prints a one-word verdict and exits unless the
// target exists inside the root, leaving it canonical in $c.
const remoteResolve = remoteShellPrelude + `r=$(rp "$root") || { echo M; exit 0; }
if [ ! -e "$p" ]; then
  if [ -L "$p" ]; then echo M; exit 0; fi
  case $(ancestor_state "$p") in u) echo U;; *) echo M;; esac
  exit 0
fi
c=$(rp "$p") || { echo U; exit 0; }
inside "$r" "$c" || { echo O; exit 0; }
`

// statFields prints "size mtime" for $1 without following a final symlink.
const remoteStat = `st() { stat -c '%s %Y' "$1" 2>/dev/null || stat -f '%z %m' "$1" 2>/dev/null || echo '- -'; }
`

func (r *Reader) readRemoteFile(ctx context.Context, root Root, targetPath string) rawFile {
	script := "root=" + remoteexec.Quote(root.Path) + "\np=" + remoteexec.Quote(root.Join(targetPath)) + "\nmax=" + strconv.Itoa(domain.MaxContextFileBytes) + "\n" +
		remoteResolve + remoteStat + `if [ -d "$c" ]; then echo D; exit 0; fi
if [ ! -f "$c" ]; then echo X; exit 0; fi
if [ ! -r "$c" ]; then echo U; exit 0; fi
set -- $(st "$c")
if [ "$1" != - ] && [ "$1" -gt "$max" ]; then echo "L $1 $2"; exit 0; fi
echo "F $1 $2"
exec cat "$c"
`
	ctx, cancel := context.WithTimeout(ctx, remoteReadTimeout)
	defer cancel()
	out, err := r.Remote(ctx, root.SSH, script)
	if err != nil {
		return rawFile{status: domain.ContextUnreachable, message: fmt.Sprintf("cannot read on %s: %v", root.Where(), err)}
	}
	head, data, _ := bytes.Cut(out, []byte("\n"))
	fields := strings.Fields(string(head))
	if len(fields) == 0 {
		return rawFile{status: domain.ContextUnreachable, message: "the remote read returned no answer on " + root.Where()}
	}
	switch fields[0] {
	case "M":
		return rawFile{status: domain.ContextMissing, message: "no such file or directory on " + root.Where()}
	case "U":
		return rawFile{status: domain.ContextPermissionDenied, message: "permission denied on " + root.Where()}
	case "O":
		return rawFile{status: domain.ContextOutsideRoot, message: "the target " + ErrOutsideRoot.Error() + " and is not readable as context"}
	case "D":
		return rawFile{status: domain.ContextNotFile, message: "the target is a directory; list it with listContext"}
	case "X":
		return rawFile{status: domain.ContextNotFile, message: "the target is not a regular file"}
	case "L", "F":
		if len(fields) != 3 {
			break
		}
		size, _ := strconv.ParseInt(fields[1], 10, 64)
		mod := unixTime(fields[2])
		if fields[0] == "L" {
			return rawFile{status: domain.ContextTooLarge, message: tooLarge(size), size: size, mod: mod}
		}
		if int64(len(data)) != size {
			return rawFile{status: domain.ContextChanged, message: fmt.Sprintf("the file changed while it was read (%d bytes expected, %d read); read it again", size, len(data))}
		}
		return rawFile{status: domain.ContextOK, data: data, size: size, mod: mod}
	}
	return rawFile{status: domain.ContextUnreachable, message: fmt.Sprintf("unexpected remote read answer %q on %s", string(head), root.Where())}
}

func unixTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// List returns one page of a directory's immediate entries in name order.
func (r *Reader) List(ctx context.Context, scope Scope, req domain.ContextListRequest) (domain.ContextListing, error) {
	ref, root, err := scope.ParseContext(req.Reference)
	if err != nil {
		return domain.ContextListing{}, &domain.ValidationError{Field: "reference", Message: err.Error()}
	}
	limit := req.Limit
	if limit == 0 {
		limit = domain.DefaultContextEntries
	}
	if req.Offset < 0 {
		return domain.ContextListing{}, &domain.ValidationError{Field: "offset", Message: "must be a 0-based entry index"}
	}
	if limit < 1 || limit > domain.MaxContextEntries {
		return domain.ContextListing{}, &domain.ValidationError{Field: "limit", Message: fmt.Sprintf("must be between 1 and %d entries", domain.MaxContextEntries)}
	}
	out := domain.ContextListing{ContextTarget: target(ref, root)}
	if root.Err != nil {
		out.Status, out.Message = domain.ContextUnreachable, root.Err.Error()
		return out, nil
	}
	var raw rawListing
	if root.Machine == "" {
		raw = listLocal(root, ref.Target)
	} else {
		raw = r.listRemote(ctx, root, ref.Target)
	}
	out.Status, out.Message = raw.status, raw.message
	if raw.status != domain.ContextOK {
		return out, nil
	}
	sort.Slice(raw.entries, func(i, j int) bool { return raw.entries[i].name < raw.entries[j].name })
	h := sha256.New()
	for _, e := range raw.entries {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", e.name, e.typ, e.size, e.mod.Unix())
	}
	out.SHA256 = fmt.Sprintf("%x", h.Sum(nil))
	if req.ExpectedSHA256 != "" && req.ExpectedSHA256 != out.SHA256 {
		out.Status = domain.ContextChanged
		out.Message = "the directory changed since the page with sha256 " + req.ExpectedSHA256 + " was listed; list again from offset 0"
		return out, nil
	}
	out.TotalEntries = len(raw.entries)
	out.Offset = req.Offset
	out.Entries = []domain.ContextEntry{}
	end := min(req.Offset+limit, len(raw.entries))
	for i := req.Offset; i < end; i++ {
		e := raw.entries[i]
		entry := domain.ContextEntry{Name: e.name, Reference: Child(ref, e.name), Type: e.typ, Size: e.size}
		if !e.mod.IsZero() {
			mod := e.mod.UTC()
			entry.ModifiedAt = &mod
		}
		out.Entries = append(out.Entries, entry)
	}
	if end < len(raw.entries) {
		out.NextOffset = end
	}
	return out, nil
}

func listLocal(root Root, targetPath string) rawListing {
	real, err := canonicalLocal(root.Path, root.Join(targetPath))
	if f, ok := localFailure(err); ok {
		return rawListing{status: f.status, message: f.message}
	}
	info, err := os.Stat(real)
	if f, ok := localFailure(err); ok {
		return rawListing{status: f.status, message: f.message}
	}
	if !info.IsDir() {
		return rawListing{status: domain.ContextNotDirectory, message: "the target is a file; read it with readContext"}
	}
	dirEntries, err := os.ReadDir(real)
	if f, ok := localFailure(err); ok {
		return rawListing{status: f.status, message: f.message}
	}
	entries := make([]rawEntry, 0, len(dirEntries))
	for _, d := range dirEntries {
		e := rawEntry{name: d.Name(), typ: domain.ContextEntryOther}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			e.typ = domain.ContextEntrySymlink
		case d.IsDir():
			e.typ = domain.ContextEntryDirectory
		case d.Type().IsRegular():
			e.typ = domain.ContextEntryFile
		}
		if fi, err := os.Lstat(filepath.Join(real, d.Name())); err == nil {
			e.size, e.mod = fi.Size(), fi.ModTime()
			if e.typ == domain.ContextEntryDirectory {
				e.size = 0
			}
		}
		entries = append(entries, e)
	}
	return rawListing{status: domain.ContextOK, entries: entries}
}

func (r *Reader) listRemote(ctx context.Context, root Root, targetPath string) rawListing {
	script := "root=" + remoteexec.Quote(root.Path) + "\np=" + remoteexec.Quote(root.Join(targetPath)) + "\n" +
		remoteResolve + remoteStat + `if [ ! -d "$c" ]; then echo N; exit 0; fi
if [ ! -r "$c" ] || [ ! -x "$c" ]; then echo U; exit 0; fi
cd "$c" || { echo U; exit 0; }
echo D
for f in * .[!.]* ..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  if [ -L "$f" ]; then t=symlink; elif [ -d "$f" ]; then t=directory; elif [ -f "$f" ]; then t=file; else t=other; fi
  printf '%s %s %s\0' "$t" "$(st "$f")" "$f"
done
`
	ctx, cancel := context.WithTimeout(ctx, remoteReadTimeout)
	defer cancel()
	out, err := r.Remote(ctx, root.SSH, script)
	if err != nil {
		return rawListing{status: domain.ContextUnreachable, message: fmt.Sprintf("cannot list on %s: %v", root.Where(), err)}
	}
	head, rest, _ := bytes.Cut(out, []byte("\n"))
	switch string(head) {
	case "M":
		return rawListing{status: domain.ContextMissing, message: "no such file or directory on " + root.Where()}
	case "U":
		return rawListing{status: domain.ContextPermissionDenied, message: "permission denied on " + root.Where()}
	case "O":
		return rawListing{status: domain.ContextOutsideRoot, message: "the target " + ErrOutsideRoot.Error() + " and is not readable as context"}
	case "N":
		return rawListing{status: domain.ContextNotDirectory, message: "the target is a file; read it with readContext"}
	case "D":
	default:
		return rawListing{status: domain.ContextUnreachable, message: fmt.Sprintf("unexpected remote listing answer %q on %s", string(head), root.Where())}
	}
	entries := []rawEntry{}
	for record := range bytes.SplitSeq(rest, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := strings.SplitN(string(record), " ", 4)
		if len(parts) != 4 {
			return rawListing{status: domain.ContextUnreachable, message: "malformed remote listing record on " + root.Where()}
		}
		e := rawEntry{typ: domain.ContextEntryType(parts[0]), name: parts[3], mod: unixTime(parts[2])}
		e.size, _ = strconv.ParseInt(parts[1], 10, 64)
		if e.typ == domain.ContextEntryDirectory {
			e.size = 0
		}
		entries = append(entries, e)
	}
	return rawListing{status: domain.ContextOK, entries: entries}
}
