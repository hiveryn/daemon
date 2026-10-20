package actionfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/remoteexec/sshtest"
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

func TestInspectReportsValidAndInvalidCheckouts(t *testing.T) {
	root := t.TempDir()
	writeAction(t, root, "demo", true, map[string]string{
		"action.yaml": "name: demo\ndescription: |\n  Compare AMS and LDN; say how many runs.\nartifacts: summary.md, results.json\nsuggestions: [Compare three runs]\n",
		"KICKOFF.md":  "Do {{prompt}} into {{output_dir}}.",
	})
	writeAction(t, root, "nogit", false, map[string]string{
		"action.yaml": "name: nogit\ndescription: d\nartifacts: a\nextra: nope\n",
	})
	writeAction(t, root, "Bad Name", true, nil)

	demo := InspectLocal(filepath.Join(root, "demo"), "demo")
	if !demo.Valid || demo.Description != "Compare AMS and LDN; say how many runs." || demo.Artifacts != "summary.md, results.json" {
		t.Fatalf("demo = %+v problems:\n%s", demo.ActionDefinition, problemText(demo))
	}

	nogit := InspectLocal(filepath.Join(root, "nogit"), "nogit")
	text := problemText(nogit)
	for _, want := range []string{"not a Git repository", "invalid YAML", "KICKOFF.md: file is missing"} {
		if nogit.Valid || !strings.Contains(text, want) {
			t.Fatalf("nogit problems missing %q:\n%s", want, text)
		}
	}

	bad := InspectLocal(filepath.Join(root, "Bad Name"), "Bad Name")
	if bad.Valid || !strings.Contains(problemText(bad), "not a valid action name") || !strings.Contains(problemText(bad), "action.yaml: file is missing") {
		t.Fatalf("bad name problems:\n%s", problemText(bad))
	}
}

func TestTheRegisteredNameNotTheDirectoryNameMustMatch(t *testing.T) {
	root := t.TempDir()
	dir := writeAction(t, root, "checkout-dir", true, map[string]string{
		"action.yaml": "name: demo\ndescription: d\nartifacts: a\nsuggestions: [x]\n",
		"KICKOFF.md":  "Do {{prompt}} into {{output_dir}}.",
	})
	if def := InspectLocal(dir, "demo"); !def.Valid {
		t.Fatalf("registered as demo: %s", problemText(def))
	}
	if def := InspectLocal(dir, "other"); def.Valid || !strings.Contains(problemText(def), `must equal the registered name "other"`) {
		t.Fatalf("registered as other: %s", problemText(def))
	}
}

func TestMissingCheckoutIsAnInvalidDefinition(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	def := InspectLocal(missing, "absent")
	if def.Valid || def.Name != "absent" || def.Path != missing || len(def.Diagnostics) != 1 || def.Diagnostics[0].Code != CodeCheckoutMissing {
		t.Fatalf("missing checkout = %+v", def)
	}
}

func TestRemoteInspectionAppliesTheSameRules(t *testing.T) {
	fake := sshtest.Install(t)
	home := fake.Home("box")
	files := map[string]string{
		"action.yaml": "name: demo\ndescription: d\nartifacts: a\nsuggestions: [\"x\"]\n",
		"KICKOFF.md":  "Do {{prompt}} into {{output_dir}}.\n\n\n",
	}
	dir := writeAction(t, home, "demo", true, files)
	local := InspectLocal(dir, "demo")
	remote := InspectRemote(context.Background(), "box", "buildbox", dir, "demo")
	if !remote.Valid || remote.Machine != "buildbox" || remote.Kickoff != local.Kickoff || remote.Description != "d" || strings.Join(remote.Suggestions, ",") != "x" {
		t.Fatalf("remote = %+v (%s)", remote.ActionDefinition, problemText(remote))
	}

	broken := writeAction(t, home, "broken", false, map[string]string{"action.yaml": "name: broken\n"})
	if err := os.Mkdir(filepath.Join(broken, "KICKOFF.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	def := InspectRemote(context.Background(), "box", "buildbox", broken, "broken")
	text := problemText(def)
	for _, want := range []string{"not a Git repository", "description is required", "KICKOFF.md: not a regular file"} {
		if def.Valid || !strings.Contains(text, want) {
			t.Fatalf("remote problems missing %q:\n%s", want, text)
		}
	}

	if def := InspectRemote(context.Background(), "box", "buildbox", filepath.Join(home, "absent"), "absent"); def.Valid || def.Diagnostics[0].Code != CodeCheckoutMissing {
		t.Fatalf("absent remote checkout = %+v", def.Diagnostics)
	}
	fake.SetDown(t, "box", true)
	def = InspectRemote(context.Background(), "box", "buildbox", dir, "demo")
	if def.Valid || def.Diagnostics[0].Code != CodeCheckoutUnreachable || !strings.Contains(problemText(def), "machine buildbox") {
		t.Fatalf("unreachable = %+v", def.Diagnostics)
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
		var messages []string
		for _, f := range kickoffProblems(kickoff) {
			messages = append(messages, f.message)
		}
		problems := strings.Join(messages, "\n")
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

func TestSuggestionsAreRequiredTrimmedAndBounded(t *testing.T) {
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
		"action.yaml": "name: broken\ndescription: d\nartifacts: a\nsuggestions:\n  - one\n  - \"  \"\n  - one\n  - " + strings.Repeat("x ", 61) + "\n",
		"KICKOFF.md":  kickoff,
	})
	writeAction(t, root, "mapping", true, map[string]string{
		"action.yaml": "name: mapping\ndescription: d\nartifacts: a\nsuggestions:\n  - label: x\n    prompt: y\n",
		"KICKOFF.md":  kickoff,
	})
	var many strings.Builder
	many.WriteString("name: many\ndescription: d\nartifacts: a\nsuggestions:\n")
	for i := range 4 {
		many.WriteString("  - prompt " + string(rune('a'+i)) + "\n")
	}
	writeAction(t, root, "many", true, map[string]string{"action.yaml": many.String(), "KICKOFF.md": kickoff})

	get := func(name string) Definition {
		t.Helper()
		return InspectLocal(filepath.Join(root, name), name)
	}

	if def := get("none"); def.Valid || def.Suggestions != nil {
		t.Fatalf("none = %+v problems:\n%s", def.ActionDefinition, problemText(def))
	}
	some := get("some")
	if !some.Valid || strings.Join(some.Suggestions, "|") != "Compare AMS and LDN|Two lines\nof prompt" {
		t.Fatalf("some = %q problems:\n%s", some.Suggestions, problemText(some))
	}
	broken := get("broken")
	text := problemText(broken)
	for _, want := range []string{"suggestions[1] is blank", "suggestions[2] repeats suggestions[0]", "suggestions[3] is 61 words; the limit is 60"} {
		if broken.Valid || !strings.Contains(text, want) {
			t.Fatalf("broken problems missing %q:\n%s", want, text)
		}
	}
	if mapping := get("mapping"); mapping.Valid || !strings.Contains(problemText(mapping), "invalid YAML") {
		t.Fatalf("mapping problems:\n%s", problemText(mapping))
	}
	if many := get("many"); many.Valid || !strings.Contains(problemText(many), "suggestions has 4 entries; the limit is 3") {
		t.Fatalf("many problems:\n%s", problemText(many))
	}
}

func diagnosticCodes(def Definition, severity Severity) []string {
	var codes []string
	for _, d := range def.Diagnostics {
		if d.Severity == severity {
			codes = append(codes, d.Code)
		}
	}
	return codes
}

func TestMissingSuggestionsInvalidate(t *testing.T) {
	root := t.TempDir()
	kickoff := "Do {{prompt}} into {{output_dir}}."
	cases := map[string]string{
		"absent": "name: absent\ndescription: d\nartifacts: a\n",
		"blank":  "name: blank\ndescription: d\nartifacts: a\nsuggestions:\n",
		"empty":  "name: empty\ndescription: d\nartifacts: a\nsuggestions: []\n",
	}
	for name, manifest := range cases {
		dir := writeAction(t, root, name, true, map[string]string{"action.yaml": manifest, "KICKOFF.md": kickoff})
		def, err := Inspect(dir)
		if err != nil {
			t.Fatal(err)
		}
		if def.Valid || len(def.Problems) != 1 {
			t.Fatalf("%s invalid: %s", name, problemText(def))
		}
		warnings := diagnosticCodes(def, SeverityError)
		if len(warnings) != 1 || warnings[0] != CodeSuggestionsMissing {
			t.Fatalf("%s warnings = %v", name, warnings)
		}
		if def.Diagnostics[0].Path != filepath.Join(dir, DefinitionFileName) {
			t.Fatalf("%s warning path = %s", name, def.Diagnostics[0].Path)
		}
	}
	dir := writeAction(t, root, "suggested", true, map[string]string{
		"action.yaml": "name: suggested\ndescription: d\nartifacts: a\nsuggestions: [one, two]\n",
		"KICKOFF.md":  kickoff,
	})
	if def, err := Inspect(dir); err != nil || !def.Valid || len(def.Diagnostics) != 0 {
		t.Fatalf("suggested = %+v, err = %v", def.Diagnostics, err)
	}
}

func TestInspectReportsCodedErrorsForAnyPath(t *testing.T) {
	dir := writeAction(t, t.TempDir(), "checkout", false, map[string]string{
		"action.yaml": "name: other\ndescription: d\nartifacts: \"\"\nsuggestions: [x, x]\n",
		"KICKOFF.md":  "{{prompt}} {{extra}}",
	})
	def, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(diagnosticCodes(def, SeverityError), ",")
	want := strings.Join([]string{CodeNotGitRepository, CodeDefinitionNameMismatch, CodeArtifactsRequired, CodeSuggestionDuplicate, CodeKickoffMissingOutputDir, CodeKickoffUnknownPlaceholder}, ",")
	if def.Valid || got != want || len(def.Problems) != len(def.Diagnostics) {
		t.Fatalf("codes = %s, want %s; problems %d diagnostics %d", got, want, len(def.Problems), len(def.Diagnostics))
	}

	if _, err := Inspect(filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("missing path err = %v", err)
	}
	if _, err := Inspect(filepath.Join(dir, "KICKOFF.md")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file path err = %v", err)
	}
}

func TestSuggestionWordBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prompts []string
		code    string
	}{
		{"sixty", []string{strings.Repeat("word\t", 60)}, ""},
		{"sixty-one", []string{strings.Repeat("word\n", 61)}, CodeSuggestionTooLong},
		{"total-120", []string{strings.Repeat("a ", 60), strings.Repeat("b\u2003", 60)}, ""},
		{"total-121", []string{strings.Repeat("a ", 60), strings.Repeat("b ", 60), "c"}, CodeSuggestionsTotalTooLong},
		{"unicode", []string{strings.Repeat("字", 2000)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, findings := suggestions(tc.prompts)
			if tc.code == "" {
				if len(findings) != 0 {
					t.Fatal(findings)
				}
				return
			}
			if len(findings) != 1 || findings[0].code != tc.code {
				t.Fatal(findings)
			}
		})
	}
}

func TestRemoteExampleErrorsMatchLocal(t *testing.T) {
	fake := sshtest.Install(t)
	for _, tc := range []struct{ name, examples string }{
		{"missing", ""},
		{"many", "suggestions: [one, two, three, four]\n"},
		{"long", "suggestions: [\"" + strings.Repeat("word ", 61) + "\"]\n"},
		{"total", "suggestions: [\"" + strings.Repeat("a ", 60) + "\", \"" + strings.Repeat("b ", 60) + "\", c]\n"},
	} {
		dir := writeAction(t, fake.Home("box"), tc.name, true, map[string]string{"action.yaml": "name: " + tc.name + "\ndescription: d\nartifacts: a\n" + tc.examples, "KICKOFF.md": "{{prompt}} {{output_dir}}"})
		local := InspectLocal(dir, tc.name)
		remote := InspectRemote(context.Background(), "box", "box", dir, tc.name)
		if local.Valid || remote.Valid || strings.Join(diagnosticCodes(local, SeverityError), ",") != strings.Join(diagnosticCodes(remote, SeverityError), ",") {
			t.Fatalf("local=%+v remote=%+v", local.Diagnostics, remote.Diagnostics)
		}
	}
}
