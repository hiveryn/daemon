package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/hiveryn/daemon/internal/actionfs"
)

// Exit codes of `hiverynd action ...`.
const (
	exitOK       = 0
	exitFailed   = 1 // validation failed: errors, or warnings under --strict
	exitUsageErr = 2 // bad usage, or the path could not be validated at all
)

const actionUsage = `usage: hiverynd action validate [--json] [--strict] [path]

Validate an Action repository (action.yaml, KICKOFF.md, .git) offline with the
daemon's own definition rules. path defaults to the current directory.

  --json    print a JSON report with coded diagnostics
  --strict  fail on warnings as well as errors (for CI)

Exit status: 0 valid, 1 validation failed, 2 usage error or unreadable path.
`

// validateReport is the --json output. Diagnostic paths are absolute.
type validateReport struct {
	Path        string                `json:"path"`
	Name        string                `json:"name"`
	Valid       bool                  `json:"valid"`
	Strict      bool                  `json:"strict"`
	Passed      bool                  `json:"passed"`
	Errors      int                   `json:"errors"`
	Warnings    int                   `json:"warnings"`
	Diagnostics []actionfs.Diagnostic `json:"diagnostics"`
}

// runAction runs an `action` subcommand and returns its exit status. It never
// touches the daemon's config, database or network.
func runAction(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "%s", actionUsage)
		return exitUsageErr
	}
	switch args[0] {
	case "validate":
		return runActionValidate(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		say(stdout, "%s", actionUsage)
		return exitOK
	default:
		say(stderr, "hiverynd action: unknown subcommand %q\n\n%s", args[0], actionUsage)
		return exitUsageErr
	}
}

func runActionValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("action validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print a JSON report")
	strict := fs.Bool("strict", false, "fail on warnings")

	// Flags may come before or after the path.
	var paths []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				say(stdout, "%s", actionUsage)
				return exitOK
			}
			say(stderr, "hiverynd action validate: %v\n\n%s", err, actionUsage)
			return exitUsageErr
		}
		if fs.NArg() == 0 {
			break
		}
		paths = append(paths, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(paths) > 1 {
		say(stderr, "hiverynd action validate: expected at most one path, got %d\n\n%s", len(paths), actionUsage)
		return exitUsageErr
	}
	path := "."
	if len(paths) == 1 {
		path = paths[0]
	}

	def, err := actionfs.Inspect(path)
	if err != nil {
		say(stderr, "hiverynd action validate: %v\n", err)
		return exitUsageErr
	}

	report := validateReport{Path: def.Path, Name: def.Name, Valid: def.Valid, Strict: *strict, Diagnostics: def.Diagnostics}
	for _, d := range def.Diagnostics {
		if d.Severity == actionfs.SeverityError {
			report.Errors++
		} else {
			report.Warnings++
		}
	}
	report.Passed = report.Errors == 0 && (!*strict || report.Warnings == 0)

	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			say(stderr, "hiverynd action validate: write report: %v\n", err)
			return exitUsageErr
		}
	} else {
		writeValidateText(stdout, report)
	}
	if !report.Passed {
		return exitFailed
	}
	return exitOK
}

func writeValidateText(w io.Writer, report validateReport) {
	verdict := "valid"
	switch {
	case !report.Valid:
		verdict = "invalid"
	case !report.Passed:
		verdict = "valid, but warnings fail --strict"
	}
	say(w, "action %s (%s): %s — %s, %s\n", report.Name, report.Path, verdict, plural(report.Errors, "error"), plural(report.Warnings, "warning"))
	for _, d := range report.Diagnostics {
		where, err := filepath.Rel(report.Path, d.Path)
		if err != nil {
			where = d.Path
		}
		say(w, "  %-7s %s  %s: %s\n", d.Severity, d.Code, where, d.Message)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// say writes CLI output; a failed write to the terminal has nowhere better to
// be reported.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
