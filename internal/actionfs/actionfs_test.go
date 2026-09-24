package actionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAction(t *testing.T, root, name string, git bool, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if git {
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func problemText(def Definition) string {
	var parts []string
	for _, p := range def.Problems {
		parts = append(parts, filepath.Base(p.Path)+": "+p.Message)
	}
	return strings.Join(parts, "\n")
}

func TestListReportsValidAndInvalidDefinitions(t *testing.T) {
	root := t.TempDir()
	writeAction(t, root, "demo", true, map[string]string{
		"action.yaml": "name: demo\ndescription: |\n  Compare AMS and LDN; say how many runs.\nartifacts: summary.md, results.json\n",
		"KICKOFF.md":  "Do {{prompt}} into {{output_dir}}.",
	})
	writeAction(t, root, "nogit", false, map[string]string{
		"action.yaml": "name: nogit\ndescription: d\nartifacts: a\nextra: nope\n",
	})
	writeAction(t, root, "Bad Name", true, nil)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("not an action"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	defs, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 3 {
		t.Fatalf("listed %d definitions, want 3 directories: %+v", len(defs), defs)
	}
	byName := map[string]Definition{}
	for _, def := range defs {
		byName[def.Name] = def
	}

	demo := byName["demo"]
	if !demo.Valid || demo.Description != "Compare AMS and LDN; say how many runs." || demo.Artifacts != "summary.md, results.json" {
		t.Fatalf("demo = %+v problems:\n%s", demo.ActionDefinition, problemText(demo))
	}

	nogit := byName["nogit"]
	text := problemText(nogit)
	for _, want := range []string{"not a Git repository", "invalid YAML", "KICKOFF.md: file is missing"} {
		if nogit.Valid || !strings.Contains(text, want) {
			t.Fatalf("nogit problems missing %q:\n%s", want, text)
		}
	}

	bad := byName["Bad Name"]
	if bad.Valid || !strings.Contains(problemText(bad), "not a valid action name") || !strings.Contains(problemText(bad), "action.yaml: file is missing") {
		t.Fatalf("bad name problems:\n%s", problemText(bad))
	}
}

func TestListMissingRootIsEmpty(t *testing.T) {
	defs, err := List(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(defs) != 0 {
		t.Fatalf("defs = %+v, err = %v", defs, err)
	}
}

func TestGetUnknownActionIsNotFound(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"missing", "../escape", ""} {
		if _, err := Get(root, name); err == nil {
			t.Fatalf("Get(%q) succeeded", name)
		}
	}
}

func TestKickoffPlaceholderRules(t *testing.T) {
	cases := map[string]string{
		"":                                      "file is empty",
		"only {{output_dir}}":                   "missing the {{prompt}} placeholder",
		"only {{prompt}}":                       "missing the {{output_dir}} placeholder",
		"{{prompt}} {{output_dir}} {{ prompt}}": "unknown placeholder {{ prompt}}",
	}
	for kickoff, want := range cases {
		problems := strings.Join(kickoffProblems(kickoff), "\n")
		if !strings.Contains(problems, want) {
			t.Errorf("kickoff %q problems %q missing %q", kickoff, problems, want)
		}
	}
	if problems := kickoffProblems("{{prompt}} then {{output_dir}} and {{prompt}} again"); len(problems) != 0 {
		t.Errorf("valid kickoff reported %v", problems)
	}
}

func TestRenderKickoffIsSinglePass(t *testing.T) {
	def := Definition{Kickoff: "Request:\n{{prompt}}\nOut: {{output_dir}}\n"}
	def.Valid = true
	got, err := RenderKickoff(def, "  write to {{output_dir}} please  ", "/out/1")
	if err != nil {
		t.Fatal(err)
	}
	want := "Request:\nwrite to {{output_dir}} please\nOut: /out/1"
	if got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
	def.Valid = false
	if _, err := RenderKickoff(def, "x", "/out"); err == nil {
		t.Fatal("rendered an invalid definition")
	}
}
