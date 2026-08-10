package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func fsSearchContentURL(path, q string, limit int) string {
	values := url.Values{"path": {path}, "q": {q}}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	return "/api/fs/search-content?" + values.Encode()
}

func decodeContentSearchResponse(t *testing.T, body []byte) fsContentSearchResponse {
	t.Helper()
	var resp fsContentSearchResponse
	decodeEnvelopeData(t, body, &resp)
	return resp
}

func TestFsSearchContentNonGitRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFsTestFile(t, filepath.Join(dir, "readme.md"), "intro\nthe Needle line\n")
	writeFsTestFile(t, filepath.Join(dir, "src", "main.ts"), "let needle = 1;\nconsole.log(needle);\n")
	// A NUL byte marks the file binary — must be skipped, not matched.
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("needle\x00needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchContentURL(dir, "needle", 0), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	resp := decodeContentSearchResponse(t, body)
	if len(resp.Matches) != 3 {
		t.Fatalf("expected 3 text matches (binary skipped), got %#v", resp.Matches)
	}
	for _, m := range resp.Matches {
		if m.Path == "blob.bin" {
			t.Fatalf("binary file leaked into matches: %#v", resp.Matches)
		}
		if m.Line < 1 || m.Text == "" {
			t.Fatalf("match missing line/text: %#v", m)
		}
	}
}

func TestFsSearchContentLimitTruncates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFsTestFile(t, filepath.Join(dir, "a.txt"), "hit\nhit\nhit\n")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchContentURL(dir, "hit", 2), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	resp := decodeContentSearchResponse(t, body)
	if len(resp.Matches) != 2 || !resp.Truncated {
		t.Fatalf("expected 2 matches truncated=true, got %d truncated=%v", len(resp.Matches), resp.Truncated)
	}
}

func TestFsSearchContentMissingQuery(t *testing.T) {
	t.Parallel()

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, "/api/fs/search-content?path=/tmp", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, string(body))
	}
}
