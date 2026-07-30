package gitdiff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	repoPath := t.TempDir()
	runGitTest(t, repoPath, "init", "-q")
	runGitTest(t, repoPath, "config", "user.name", "Test User")
	runGitTest(t, repoPath, "config", "user.email", "test@example.com")
	return repoPath
}

func createRepoWithMixedChanges(t *testing.T) string {
	t.Helper()
	repoPath := initRepo(t)
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo\nthree\n")
	runGitTest(t, repoPath, "add", "file.txt")
	runGitTest(t, repoPath, "commit", "-q", "-m", "init")

	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo staged\nthree\n")
	runGitTest(t, repoPath, "add", "file.txt")
	writeFile(t, filepath.Join(repoPath, "file.txt"), "one\ntwo unstaged\nthree\n")
	writeFile(t, filepath.Join(repoPath, "untracked.txt"), "new file\nline\n")

	return repoPath
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBinaryContent(path string, contents []byte) error {
	return os.WriteFile(path, contents, 0o644)
}

func runGitTest(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func commitSHA(t *testing.T, repoPath string) string {
	t.Helper()
	return runGitTest(t, repoPath, "rev-parse", "HEAD")
}

func findFile(t *testing.T, files []File, path string) File {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("file %q not found in %#v", path, files)
	return File{}
}
