package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func fsSearchURL(path, q string, limit int) string {
	values := url.Values{"path": {path}, "q": {q}}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	return "/api/fs/search?" + values.Encode()
}

func decodeSearchResponse(t *testing.T, body []byte) fsSearchResponse {
	t.Helper()
	var resp fsSearchResponse
	decodeEnvelopeData(t, body, &resp)
	return resp
}

func matchPaths(resp fsSearchResponse) []string {
	paths := make([]string, len(resp.Matches))
	for i, m := range resp.Matches {
		paths[i] = m.Path
	}
	return paths
}

func TestFsSearchNonGitRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "components"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFsTestFile(t, filepath.Join(dir, "readme.md"), "root")
	writeFsTestFile(t, filepath.Join(dir, "src", "main.ts"), "code")
	writeFsTestFile(t, filepath.Join(dir, "src", "components", "button.tsx"), "code")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchURL(dir, "btn", 0), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	resp := decodeSearchResponse(t, body)
	if len(resp.Matches) != 1 || resp.Matches[0].Path != "src/components/button.tsx" {
		t.Fatalf("expected subsequence match on button.tsx, got %v", matchPaths(resp))
	}
	if resp.Total != 1 || resp.Truncated {
		t.Fatalf("expected total=1 truncated=false, got total=%d truncated=%v", resp.Total, resp.Truncated)
	}
}

func TestFsSearchRankingPrefersBasenameMatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "button"), 0o755); err != nil {
		t.Fatal(err)
	}
	// "button" appears only in the path for one, in the basename for the other.
	writeFsTestFile(t, filepath.Join(dir, "button", "index.ts"), "code")
	writeFsTestFile(t, filepath.Join(dir, "z-button.tsx"), "code")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchURL(dir, "button", 0), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	resp := decodeSearchResponse(t, body)
	paths := matchPaths(resp)
	if len(paths) != 2 || paths[0] != "z-button.tsx" || paths[1] != "button/index.ts" {
		t.Fatalf("expected basename match ranked first, got %v", paths)
	}
}

func TestFsSearchGitRootRespectsGitignore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runFsTestGit(t, dir, "init", "-q")
	writeFsTestFile(t, filepath.Join(dir, ".gitignore"), "ignored.log\n")
	writeFsTestFile(t, filepath.Join(dir, "tracked.ts"), "code")
	writeFsTestFile(t, filepath.Join(dir, "untracked.ts"), "code")
	writeFsTestFile(t, filepath.Join(dir, "ignored.log"), "log")
	runFsTestGit(t, dir, "add", "tracked.ts")
	runFsTestGit(t, dir, "commit", "-q", "-m", "init")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchURL(dir, "ed", 0), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	paths := matchPaths(decodeSearchResponse(t, body))
	if len(paths) != 2 {
		t.Fatalf("expected tracked+untracked only, got %v", paths)
	}
	for _, p := range paths {
		if p == "ignored.log" {
			t.Fatalf("gitignored file leaked into results: %v", paths)
		}
	}
}

func TestFsSearchNestedRepoUnderPlainRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFsTestFile(t, filepath.Join(dir, "notes.md"), "plain file")
	repo := filepath.Join(dir, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runFsTestGit(t, repo, "init", "-q")
	writeFsTestFile(t, filepath.Join(repo, ".gitignore"), "secret.env\n")
	writeFsTestFile(t, filepath.Join(repo, "app.go"), "code")
	writeFsTestFile(t, filepath.Join(repo, "secret.env"), "x")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchURL(dir, "e", 0), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	paths := matchPaths(decodeSearchResponse(t, body))
	want := map[string]bool{"notes.md": true, "myrepo/.gitignore": true}
	for _, p := range paths {
		if p == "myrepo/secret.env" {
			t.Fatalf("nested repo's gitignored file leaked: %v", paths)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected matches %v in %v", want, paths)
	}
}

func TestFsSearchLimitAndTotal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFsTestFile(t, filepath.Join(dir, "aaa.txt"), "1")
	writeFsTestFile(t, filepath.Join(dir, "aab.txt"), "2")
	writeFsTestFile(t, filepath.Join(dir, "aac.txt"), "3")

	handler := newFsTestHandler(t)
	status, body := request(t, handler, http.MethodGet, fsSearchURL(dir, "aa", 1), nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}
	resp := decodeSearchResponse(t, body)
	if len(resp.Matches) != 1 || resp.Total != 3 {
		t.Fatalf("expected 1 match of total 3, got %d of %d", len(resp.Matches), resp.Total)
	}
}

func TestFsSearchValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFsTestFile(t, filepath.Join(dir, "file.txt"), "x")
	handler := newFsTestHandler(t)

	// Missing q.
	status, _ := request(t, handler, http.MethodGet, "/api/fs/search?"+url.Values{"path": {dir}}.Encode(), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("missing q: expected 400, got %d", status)
	}

	// Out-of-range limit.
	status, _ = request(t, handler, http.MethodGet, fsSearchURL(dir, "x", maxSearchResults+1), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("oversized limit: expected 400, got %d", status)
	}

	// Root is a file.
	status, _ = request(t, handler, http.MethodGet, fsSearchURL(filepath.Join(dir, "file.txt"), "x", 0), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("file root: expected 400, got %d", status)
	}

	// Nonexistent root.
	status, _ = request(t, handler, http.MethodGet, fsSearchURL(filepath.Join(dir, "missing"), "x", 0), nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing root: expected 404, got %d", status)
	}
}
