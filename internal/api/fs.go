package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/hiveryn/daemon/internal/gitdiff"
)

// maxTreeEntries caps how many entries a single /api/fs/tree call returns,
// so pathological directories (node_modules, etc.) don't blow up the
// response. It's a var (not const) so tests can lower it.
var maxTreeEntries = 2000

// maxFileBytes caps how many bytes /api/fs/file reads from a file. It's a
// var (not const) so tests can lower it.
var maxFileBytes int64 = 2 * 1024 * 1024

type fsHandler struct {
	logger *slog.Logger
}

type fsEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // file | dir | symlink | other
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
	Ignored bool   `json:"ignored,omitempty"`
}

type fsTreeResponse struct {
	Path      string    `json:"path"`
	Entries   []fsEntry `json:"entries"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated,omitempty"`
}

func (h *fsHandler) tree(w http.ResponseWriter, r *http.Request) {
	path, ok := parseAbsPathParam(w, r)
	if !ok {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		writeFsOSError(w, r, path, err)
		return
	}
	if !info.IsDir() {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "path is not a directory: "+path, map[string]string{"field": "path"})
		return
	}

	dirEntries, err := os.ReadDir(path)
	if err != nil {
		writeFsOSError(w, r, path, err)
		return
	}

	total := len(dirEntries)
	truncated := false
	if total > maxTreeEntries {
		dirEntries = dirEntries[:maxTreeEntries]
		truncated = true
	}

	entries := make([]fsEntry, 0, len(dirEntries))
	names := make([]string, 0, len(dirEntries))
	for _, de := range dirEntries {
		entryInfo, err := de.Info()
		if err != nil {
			writeFsOSError(w, r, filepath.Join(path, de.Name()), err)
			return
		}
		entries = append(entries, fsEntry{
			Name:    de.Name(),
			Kind:    entryKind(entryInfo.Mode()),
			Size:    entryInfo.Size(),
			ModTime: entryInfo.ModTime().UTC().Format(time.RFC3339),
		})
		names = append(names, de.Name())
	}

	ignored := gitdiff.CheckIgnore(r.Context(), path, names)
	for i := range entries {
		if ignored[entries[i].Name] {
			entries[i].Ignored = true
		}
	}

	writeJSON(w, r, http.StatusOK, fsTreeResponse{
		Path:      path,
		Entries:   entries,
		Total:     total,
		Truncated: truncated,
	})
}

func entryKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "dir"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

type fsWriteRequest struct {
	Content string `json:"content"`
}

type fsWriteResponse struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
}

// writeFile overwrites an existing file's contents. It is deliberately
// overwrite-only: the target must already exist (missing → 404), so it can't be
// used to create arbitrary new files. Like the read endpoints, it accepts any
// absolute path with no root containment.
func (h *fsHandler) writeFile(w http.ResponseWriter, r *http.Request) {
	path, ok := parseAbsPathParam(w, r)
	if !ok {
		return
	}

	var req fsWriteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), err.Error(), map[string]string{"field": "body"})
		return
	}
	if int64(len(req.Content)) > maxFileBytes {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "content exceeds max file size of "+strconv.FormatInt(maxFileBytes, 10)+" bytes", map[string]string{"field": "content"})
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		writeFsOSError(w, r, path, err)
		return
	}
	if info.IsDir() {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "path is a directory: "+path, map[string]string{"field": "path"})
		return
	}

	if err := atomicWriteFile(path, []byte(req.Content), info.Mode().Perm()); err != nil {
		writeFsOSError(w, r, path, err)
		return
	}

	info, err = os.Stat(path)
	if err != nil {
		writeFsOSError(w, r, path, err)
		return
	}

	writeJSON(w, r, http.StatusOK, fsWriteResponse{
		Path:    path,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339),
	})
}

func (h *fsHandler) file(w http.ResponseWriter, r *http.Request) {
	path, ok := parseAbsPathParam(w, r)
	if !ok {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		writeFsOSError(w, r, path, err)
		return
	}
	if info.IsDir() {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "path is a directory: "+path, map[string]string{"field": "path"})
		return
	}

	f, err := os.Open(path)
	if err != nil {
		writeFsOSError(w, r, path, err)
		return
	}
	defer func() { _ = f.Close() }()

	trueSize := info.Size()
	readLimit := min(trueSize, maxFileBytes)
	truncated := trueSize > maxFileBytes

	buf := make([]byte, readLimit)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		writeFsOSError(w, r, path, err)
		return
	}
	buf = buf[:n]

	w.Header().Set("Content-Type", http.DetectContentType(buf))
	w.Header().Set("X-File-Size", strconv.FormatInt(trueSize, 10))
	w.Header().Set("X-File-Truncated", strconv.FormatBool(truncated))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

func parseAbsPathParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.URL.Query().Get("path")
	if raw == "" {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "missing required query parameter: path", map[string]string{"field": "path"})
		return "", false
	}
	cleaned := filepath.Clean(raw)
	if !filepath.IsAbs(cleaned) {
		writeError(w, r, http.StatusBadRequest, string(domain.ErrCodeValidation), "path must be absolute: "+raw, map[string]string{"field": "path"})
		return "", false
	}
	return cleaned, true
}

func writeFsOSError(w http.ResponseWriter, r *http.Request, path string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", err.Error(), map[string]string{"resource": "path", "id": path})
		return
	}
	if errors.Is(err, fs.ErrPermission) {
		writeError(w, r, http.StatusForbidden, "PERMISSION_DENIED", err.Error(), map[string]string{"path": path})
		return
	}
	slog.Error("fs operation failed", "path", path, "error", err)
	writeError(w, r, http.StatusInternalServerError, string(domain.ErrCodeInternal), err.Error(), map[string]string{"path": path})
}

// atomicWriteFile writes data to path by writing a sibling temp file and
// renaming it into place, so a crash mid-write can't leave a partially written
// file. Mirrors internal/config.atomicWriteFile, reimplemented here because that
// copy is unexported and hardcodes a .yaml.tmp suffix.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hiveryn-fswrite-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %q: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %q to %q: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}
