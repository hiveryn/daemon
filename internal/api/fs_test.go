package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newFsTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return NewHandler(Dependencies{
		Config:  testConfig(),
		Runtime: newTestRuntime(t),
		BaseURL: "http://127.0.0.1:4200",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// rawRequest is like request() but also exposes response headers, needed to
// assert on /api/fs/file's raw-byte responses.
func rawRequest(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func fsTreeURL(path string) string {
	return "/api/fs/tree?" + url.Values{"path": {path}}.Encode()
}

func fsFileURL(path string) string {
	return "/api/fs/file?" + url.Values{"path": {path}}.Encode()
}

func TestFsTreeEndpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFsTestFile(t, filepath.Join(dir, "file.txt"), "hello")
	writeFsTestFile(t, filepath.Join(dir, ".hidden"), "dotfile")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(dir), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload fsTreeResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.Path != dir || payload.Total != 3 || payload.Truncated {
		t.Fatalf("unexpected payload: %#v", payload)
	}

	byName := map[string]fsEntry{}
	for _, e := range payload.Entries {
		byName[e.Name] = e
	}
	if byName["file.txt"].Kind != "file" {
		t.Fatalf("expected file.txt to be kind=file, got %#v", byName["file.txt"])
	}
	if byName["subdir"].Kind != "dir" {
		t.Fatalf("expected subdir to be kind=dir, got %#v", byName["subdir"])
	}
	if _, ok := byName[".hidden"]; !ok {
		t.Fatalf("expected dotfile to be included, got %#v", payload.Entries)
	}
}

func TestFsTreeEndpoint_SymlinkEntryNotFollowed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	writeFsTestFile(t, target, "target contents")
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(dir), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload fsTreeResponse
	decodeEnvelopeData(t, body, &payload)
	byName := map[string]fsEntry{}
	for _, e := range payload.Entries {
		byName[e.Name] = e
	}
	if byName["link.txt"].Kind != "symlink" {
		t.Fatalf("expected link.txt to be kind=symlink, got %#v", byName["link.txt"])
	}
}

func TestFsTreeEndpoint_RootSymlinkFollowed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFsTestFile(t, filepath.Join(realDir, "inside.txt"), "hi")
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(linkDir), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload fsTreeResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.Total != 1 || payload.Entries[0].Name != "inside.txt" {
		t.Fatalf("expected root symlink to be followed and list target contents, got %#v", payload)
	}
}

func TestFsTreeEndpoint_Truncated(t *testing.T) {
	original := maxTreeEntries
	maxTreeEntries = 3
	t.Cleanup(func() { maxTreeEntries = original })

	dir := t.TempDir()
	for i := range 5 {
		writeFsTestFile(t, filepath.Join(dir, fmt.Sprintf("file-%d.txt", i)), "x")
	}

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(dir), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload fsTreeResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.Total != 5 || !payload.Truncated || len(payload.Entries) != 3 {
		t.Fatalf("expected truncation at cap 3 of 5 total, got %#v", payload)
	}
}

func TestFsTreeEndpoint_NotFound(t *testing.T) {
	t.Parallel()

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(filepath.Join(t.TempDir(), "does-not-exist")), nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, string(body))
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND, got %q", errBody.Code)
	}
}

func TestFsTreeEndpoint_RelativePathRejected(t *testing.T) {
	t.Parallel()

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, "/api/fs/tree?path=relative/path", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, string(body))
	}
}

func TestFsTreeEndpoint_NotADirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "file.txt")
	writeFsTestFile(t, filePath, "hello")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(filePath), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, string(body))
	}
}

func TestFsTreeEndpoint_Ignored(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	runFsTestGit(t, repoPath, "init", "-q")
	writeFsTestFile(t, filepath.Join(repoPath, ".gitignore"), "ignored.txt\n")
	writeFsTestFile(t, filepath.Join(repoPath, "tracked.txt"), "keep")
	writeFsTestFile(t, filepath.Join(repoPath, "ignored.txt"), "skip")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsTreeURL(repoPath), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload fsTreeResponse
	decodeEnvelopeData(t, body, &payload)
	byName := map[string]fsEntry{}
	for _, e := range payload.Entries {
		byName[e.Name] = e
	}
	if !byName["ignored.txt"].Ignored {
		t.Fatalf("expected ignored.txt to be Ignored=true, got %#v", byName["ignored.txt"])
	}
	if byName["tracked.txt"].Ignored {
		t.Fatalf("expected tracked.txt to be Ignored=false, got %#v", byName["tracked.txt"])
	}
}

func TestFsFileEndpoint_Text(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "file.txt")
	contents := "hello world\n"
	writeFsTestFile(t, filePath, contents)

	handler := newFsTestHandler(t)
	rec := rawRequest(t, handler, http.MethodGet, fsFileURL(filePath))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-File-Size") != strconv.Itoa(len(contents)) {
		t.Fatalf("expected X-File-Size %d, got %q", len(contents), rec.Header().Get("X-File-Size"))
	}
	if rec.Header().Get("X-File-Truncated") != "false" {
		t.Fatalf("expected X-File-Truncated=false, got %q", rec.Header().Get("X-File-Truncated"))
	}
	if rec.Body.String() != contents {
		t.Fatalf("expected body %q, got %q", contents, rec.Body.String())
	}
}

func TestFsFileEndpoint_Binary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "file.bin")
	contents := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x00, 0x00}
	if err := os.WriteFile(filePath, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newFsTestHandler(t)
	rec := rawRequest(t, handler, http.MethodGet, fsFileURL(filePath))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/") {
		t.Fatalf("expected non-text content type, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestFsFileEndpoint_Truncated(t *testing.T) {
	original := maxFileBytes
	maxFileBytes = 4
	t.Cleanup(func() { maxFileBytes = original })

	dir := t.TempDir()
	filePath := filepath.Join(dir, "big.txt")
	contents := "0123456789"
	writeFsTestFile(t, filePath, contents)

	handler := newFsTestHandler(t)
	rec := rawRequest(t, handler, http.MethodGet, fsFileURL(filePath))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-File-Truncated") != "true" {
		t.Fatalf("expected X-File-Truncated=true, got %q", rec.Header().Get("X-File-Truncated"))
	}
	if rec.Header().Get("X-File-Size") != strconv.Itoa(len(contents)) {
		t.Fatalf("expected X-File-Size %d, got %q", len(contents), rec.Header().Get("X-File-Size"))
	}
	if rec.Body.Len() != 4 {
		t.Fatalf("expected truncated body of 4 bytes, got %d", rec.Body.Len())
	}
}

func TestFsFileEndpoint_SymlinkFollowed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	contents := "target contents"
	writeFsTestFile(t, target, contents)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	handler := newFsTestHandler(t)
	rec := rawRequest(t, handler, http.MethodGet, fsFileURL(link))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != contents {
		t.Fatalf("expected symlink target contents %q, got %q", contents, rec.Body.String())
	}
}

func TestFsFileEndpoint_NotFound(t *testing.T) {
	t.Parallel()

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsFileURL(filepath.Join(t.TempDir(), "missing.txt")), nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, string(body))
	}
}

func TestFsFileEndpoint_Directory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsFileURL(dir), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, string(body))
	}
}

func TestFsFileEndpoint_RelativePathRejected(t *testing.T) {
	t.Parallel()

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, "/api/fs/file?path=relative/path", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, string(body))
	}
}

func writeFsTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runFsTestGit(t *testing.T, repoPath string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	if _, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v", strings.Join(args, " "), err)
	}
	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = repoPath
	_ = cmd.Run()
	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = repoPath
	_ = cmd.Run()
}
