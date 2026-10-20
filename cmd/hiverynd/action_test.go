package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/actionfs"
)

func writeActionRepo(t *testing.T, name, manifest, kickoff string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{"action.yaml": manifest, "KICKOFF.md": kickoff} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const validKickoff = "Do {{prompt}} into {{output_dir}}."

func TestActionValidateMissingExamplesFailWithOrWithoutStrict(t *testing.T) {
	dir := writeActionRepo(t, "demo", "name: demo\ndescription: d\nartifacts: a\n", validKickoff)

	code, out, errOut := runCLI(t, "action", "validate", dir)
	if code != exitFailed || errOut != "" || !strings.Contains(out, "invalid — 1 error, 0 warnings") || !strings.Contains(out, "error   SUGGESTIONS_MISSING  action.yaml:") {
		t.Fatalf("code %d stdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	code, out, _ = runCLI(t, "action", "validate", dir, "--strict", "--json")
	var report validateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if code != exitFailed || report.Valid || report.Passed || !report.Strict || report.Warnings != 0 || report.Errors != 1 {
		t.Fatalf("code %d report %+v", code, report)
	}
	d := report.Diagnostics[0]
	if d.Code != actionfs.CodeSuggestionsMissing || d.Severity != actionfs.SeverityError || d.Path != filepath.Join(dir, "action.yaml") {
		t.Fatalf("diagnostic %+v", d)
	}
}

func TestActionValidateDefaultsToCurrentDirectory(t *testing.T) {
	dir := writeActionRepo(t, "demo", "name: demo\ndescription: d\nartifacts: a\nsuggestions: [one, two]\n", validKickoff)
	t.Chdir(dir)
	code, out, errOut := runCLI(t, "action", "validate", "--strict")
	if code != exitOK || !strings.Contains(out, "action demo (") || !strings.Contains(out, "valid — 0 errors, 0 warnings") {
		t.Fatalf("code %d stdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

func TestActionValidateErrorsFail(t *testing.T) {
	dir := writeActionRepo(t, "demo", "name: demo\ndescription: d\nartifacts: a\nsuggestions: [x]\nextra: 1\n", "{{prompt}}")
	code, out, _ := runCLI(t, "action", "validate", "--json", dir)
	var report validateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	var codes []string
	for _, d := range report.Diagnostics {
		codes = append(codes, d.Code)
	}
	if code != exitFailed || report.Valid || report.Passed || strings.Join(codes, ",") != "DEFINITION_INVALID_YAML,KICKOFF_MISSING_OUTPUT_DIR" {
		t.Fatalf("code %d report %+v", code, report)
	}
	if !strings.Contains(report.Diagnostics[0].Message, "field extra not found") {
		t.Fatalf("parse context lost: %s", report.Diagnostics[0].Message)
	}
}

func TestActionUsageErrorsNeverServe(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	cases := map[string][]string{
		"no subcommand":      {"action"},
		"unknown subcommand": {"action", "lint"},
		"unknown flag":       {"action", "validate", "--fix"},
		"two paths":          {"action", "validate", "a", "b"},
		"missing path":       {"action", "validate", missing},
		"unknown command":    {"frobnicate"},
	}
	for name, args := range cases {
		code, out, errOut := runCLI(t, args...)
		if code != exitUsageErr || out != "" || errOut == "" || strings.Contains(errOut, "daemon failed") {
			t.Errorf("%s: code %d stdout %q stderr %q", name, code, out, errOut)
		}
	}
	if _, _, errOut := runCLI(t, "action", "validate", missing); !strings.Contains(errOut, "no such file or directory") {
		t.Errorf("missing path lost filesystem context: %q", errOut)
	}
}

func TestActionInitScaffoldAndRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello-action")
	code, _, errOut := runCLI(t, "action", "init", dir)
	if code != exitOK {
		t.Fatal(code, errOut)
	}
	if code, out, errOut := runCLI(t, "action", "validate", "--strict", dir); code != exitOK {
		t.Fatal(out, errOut)
	}
	before, err := os.ReadFile(filepath.Join(dir, "action.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "action", "init", dir); code != exitUsageErr {
		t.Fatal(code)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "action.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("overwritten")
	}
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "?? action.yaml") {
		t.Fatal(string(out), err)
	}
	if err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD").Run(); err == nil {
		t.Fatal("init created a commit")
	}
}

func TestActionInitExistingRepositoryAndInvalidPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "existing")
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "keep.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	before, _ := exec.Command("git", "-C", dir, "ls-files", "--stage").Output()
	t.Chdir(dir)
	if code, _, errOut := runCLI(t, "action", "init"); code != exitOK {
		t.Fatal(errOut)
	}
	after, _ := exec.Command("git", "-C", dir, "ls-files", "--stage").Output()
	if !bytes.Equal(before, after) {
		t.Fatal("index changed")
	}
	invalid := filepath.Join(t.TempDir(), "Bad Name")
	if code, _, errOut := runCLI(t, "action", "init", invalid); code != exitUsageErr || !strings.Contains(errOut, "invalid action name") {
		t.Fatal(code, errOut)
	}
	if _, err := os.Stat(invalid); !os.IsNotExist(err) {
		t.Fatal("invalid target created")
	}
	for _, file := range []string{"action.yaml", "KICKOFF.md"} {
		target := filepath.Join(t.TempDir(), "refuse")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("missing", filepath.Join(target, file)); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runCLI(t, "action", "init", target); code != exitUsageErr || !strings.Contains(errOut, "refusing to overwrite") {
			t.Fatal(code, errOut)
		}
		if _, err := os.Stat(filepath.Join(target, ".git")); !os.IsNotExist(err) {
			t.Fatal("refusal initialized Git")
		}
	}
}

func TestActionInitPreservesWorktree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	git := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return out
	}
	git("init", repo)
	git("-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	target := filepath.Join(root, "linked-action")
	git("-C", repo, "worktree", "add", "--detach", target)
	before, err := os.ReadFile(filepath.Join(target, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	head := git("-C", target, "rev-parse", "HEAD")
	if code, _, errOut := runCLI(t, "action", "init", target); code != exitOK {
		t.Fatal(code, errOut)
	}
	after, err := os.ReadFile(filepath.Join(target, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !bytes.Equal(head, git("-C", target, "rev-parse", "HEAD")) {
		t.Fatal("worktree changed")
	}
	if code, out, errOut := runCLI(t, "action", "validate", target); code != exitOK {
		t.Fatal(out, errOut)
	}
}

func TestActionInitQuotesYAMLNames(t *testing.T) {
	for _, name := range []string{"null", "true", "123"} {
		dir := filepath.Join(t.TempDir(), name)
		if code, _, errOut := runCLI(t, "action", "init", dir); code != exitOK {
			t.Fatal(errOut)
		}
		if def, err := actionfs.Inspect(dir); err != nil || !def.Valid || def.Name != name {
			t.Fatal(def, err)
		}
	}
}
