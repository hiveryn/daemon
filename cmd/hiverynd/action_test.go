package main

import (
	"bytes"
	"encoding/json"
	"os"
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

func TestActionValidateWarningsPassUnlessStrict(t *testing.T) {
	dir := writeActionRepo(t, "demo", "name: demo\ndescription: d\nartifacts: a\n", validKickoff)

	code, out, errOut := runCLI(t, "action", "validate", dir)
	if code != exitOK || errOut != "" || !strings.Contains(out, "valid — 0 errors, 1 warning") || !strings.Contains(out, "warning SUGGESTIONS_MISSING  action.yaml:") {
		t.Fatalf("code %d stdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	code, out, _ = runCLI(t, "action", "validate", dir, "--strict", "--json")
	var report validateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if code != exitFailed || !report.Valid || report.Passed || !report.Strict || report.Warnings != 1 || report.Errors != 0 {
		t.Fatalf("code %d report %+v", code, report)
	}
	d := report.Diagnostics[0]
	if d.Code != actionfs.CodeSuggestionsMissing || d.Severity != actionfs.SeverityWarning || d.Path != filepath.Join(dir, "action.yaml") {
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
