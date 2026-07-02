package api

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/config"
)

func TestReposDiffEndpoint(t *testing.T) {
	t.Parallel()

	handler := newGitDiffTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/daemon/diff", nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload diffResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.Repo != "daemon" || payload.RepoPath == "" {
		t.Fatalf("unexpected diff payload: %#v", payload)
	}
	if payload.Summary.Files == 0 {
		t.Fatalf("expected at least one changed file, got %#v", payload.Summary)
	}
}

func TestReposDiffEndpoint_ArchitectNotFound(t *testing.T) {
	t.Parallel()

	handler := newGitDiffTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/missing/repos/daemon/diff", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, string(body))
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND, got %q", errBody.Code)
	}
}

func TestReposDiffEndpoint_RepoNotFound(t *testing.T) {
	t.Parallel()

	handler := newGitDiffTestHandler(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/missing/diff", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, string(body))
	}
}

func TestReposCommitDiffEndpoint(t *testing.T) {
	t.Parallel()

	handler, sha := newGitDiffTestHandlerWithCommit(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/daemon/commits/"+sha+"/diff", nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var payload commitDiffResponse
	decodeEnvelopeData(t, body, &payload)
	if payload.SHA != sha || payload.Summary.Files == 0 {
		t.Fatalf("unexpected commit diff payload: %#v", payload)
	}
}

func TestReposCommitDiffEndpoint_UnknownSHA(t *testing.T) {
	t.Parallel()

	handler, _ := newGitDiffTestHandlerWithCommit(t)

	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/daemon/commits/"+strings.Repeat("a", 40)+"/diff", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, string(body))
	}
}

// newGitDiffTestHandler builds a handler whose "hiveryn"/"daemon" repo
// points at a real temp git repo with mixed staged/unstaged/untracked
// changes, for exercising the git-shelling diff endpoints end to end.
func newGitDiffTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _ := newGitDiffTestHandlerWithCommit(t)
	return handler
}

func newGitDiffTestHandlerWithCommit(t *testing.T) (http.Handler, string) {
	t.Helper()

	repoPath := t.TempDir()
	runGitDiffTestCmd(t, repoPath, "init", "-q")
	runGitDiffTestCmd(t, repoPath, "config", "user.name", "Test User")
	runGitDiffTestCmd(t, repoPath, "config", "user.email", "test@example.com")

	writeGitDiffTestFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo\nthree\n")
	runGitDiffTestCmd(t, repoPath, "add", "file.txt")
	runGitDiffTestCmd(t, repoPath, "commit", "-q", "-m", "init")
	sha := runGitDiffTestCmd(t, repoPath, "rev-parse", "HEAD")

	writeGitDiffTestFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo staged\nthree\n")
	runGitDiffTestCmd(t, repoPath, "add", "file.txt")
	writeGitDiffTestFile(t, filepath.Join(repoPath, "untracked.txt"), "new\n")

	cfg := testConfig()
	architect := cfg.Architects["hiveryn"]
	architect.Repos = map[string]string{"daemon": repoPath}
	cfg.Architects["hiveryn"] = architect

	return NewHandler(Dependencies{
		Config:  cfg,
		Runtime: newTestRuntime(t),
		BaseURL: "http://127.0.0.1:4200",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), sha
}

func runGitDiffTestCmd(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeGitDiffTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestRuntime(t *testing.T) config.Runtime {
	t.Helper()
	runtime, err := config.ResolveRuntime("", "")
	if err != nil {
		t.Fatalf("resolve runtime: %v", err)
	}
	return runtime
}
