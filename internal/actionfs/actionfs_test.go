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

func TestSuggestionsAreOptionalTrimmedAndBounded(t *testing.T) {
	root := t.TempDir()
	kickoff := "Do {{prompt}} into {{output_dir}}."
	writeAction(t, root, "none", true, map[string]string{
		"action.yaml": "name: none\ndescription: d\nartifacts: a\n",
		"KICKOFF.md":  kickoff,
	})
	writeAction(t, root, "some", true, map[string]string{
		"action.yaml": "name: some\ndescription: d\nartifacts: a\nsuggestions:\n  - \"  Compare AMS and LDN  \"\n  - |\n    Two lines\n    of prompt\n",
		"KICKOFF.md":  kickoff,
	})
	writeAction(t, root, "broken", true, map[string]string{
		"action.yaml": "name: broken\ndescription: d\nartifacts: a\nsuggestions:\n  - one\n  - \"  \"\n  - one\n  - " + strings.Repeat("x", 1001) + "\n",
		"KICKOFF.md":  kickoff,
	})
	writeAction(t, root, "mapping", true, map[string]string{
		"action.yaml": "name: mapping\ndescription: d\nartifacts: a\nsuggestions:\n  - label: x\n    prompt: y\n",
		"KICKOFF.md":  kickoff,
	})
	var many strings.Builder
	many.WriteString("name: many\ndescription: d\nartifacts: a\nsuggestions:\n")
	for i := range 11 {
		many.WriteString("  - prompt " + string(rune('a'+i)) + "\n")
	}
	writeAction(t, root, "many", true, map[string]string{"action.yaml": many.String(), "KICKOFF.md": kickoff})

	get := func(name string) Definition {
		t.Helper()
		def, err := Get(root, name)
		if err != nil {
			t.Fatal(err)
		}
		return def
	}

	if def := get("none"); !def.Valid || def.Suggestions != nil {
		t.Fatalf("none = %+v problems:\n%s", def.ActionDefinition, problemText(def))
	}
	some := get("some")
	if !some.Valid || strings.Join(some.Suggestions, "|") != "Compare AMS and LDN|Two lines\nof prompt" {
		t.Fatalf("some = %q problems:\n%s", some.Suggestions, problemText(some))
	}
	broken := get("broken")
	text := problemText(broken)
	for _, want := range []string{"suggestions[1] is blank", "suggestions[2] repeats suggestions[0]", "suggestions[3] is 1001 characters; the limit is 1000"} {
		if broken.Valid || !strings.Contains(text, want) {
			t.Fatalf("broken problems missing %q:\n%s", want, text)
		}
	}
	if mapping := get("mapping"); mapping.Valid || !strings.Contains(problemText(mapping), "invalid YAML") {
		t.Fatalf("mapping problems:\n%s", problemText(mapping))
	}
	if many := get("many"); many.Valid || !strings.Contains(problemText(many), "suggestions has 11 entries; the limit is 10") {
		t.Fatalf("many problems:\n%s", problemText(many))
	}
}
