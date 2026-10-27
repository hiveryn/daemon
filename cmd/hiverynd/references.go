package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/config"
)

const referencesUsage = `usage:
  hiverynd references-migrate plan [-daemon-url URL] [-architect KEY]... [-rules rules.json] > plan.json
  hiverynd references-migrate apply [-daemon-url URL] plan.json > result.json

plan inventories every registered architect (or those named) and changes
nothing. Review plan.json, decide ambiguous/unmapped/cross_architect/invalid
entries by setting "to" or "drop": true, then apply it. Tickets whose file
changed since the plan are reported as conflicts and left untouched; rerun
plan and apply until nothing is listed. The running daemon performs both, so
runtime-owned records change through its workspace protection.

rules.json is a reviewed list of explicit path mappings applied before
inference, e.g. a repo that moved:
  [{"architect": "radius", "from_prefix": "/Users/me/one/repos/radius-engine", "to_prefix": "radius-engine:"}]
`

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// runReferences talks to the running daemon; the JSON report goes to stdout
// and a one-line summary to stderr.
func runReferences(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "plan" && args[0] != "apply") {
		say(stderr, "%s", referencesUsage)
		return exitUsage
	}
	mode := args[0]
	fs := flag.NewFlagSet("references-migrate "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultURL := os.Getenv("HIVERYN_DAEMON_URL")
	if defaultURL == "" {
		defaultURL = "http://127.0.0.1:" + strconv.Itoa(config.DefaultPort)
	}
	daemonURL := fs.String("daemon-url", defaultURL, "Daemon HTTP base URL")
	var architects stringList
	var rulesPath *string
	if mode == "plan" {
		fs.Var(&architects, "architect", "Architect key to plan (repeatable; default every registered architect)")
		rulesPath = fs.String("rules", "", "JSON file of reviewed path-prefix rules")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	var body []byte
	switch mode {
	case "plan":
		if fs.NArg() != 0 {
			say(stderr, "%s", referencesUsage)
			return exitUsage
		}
		request := map[string]any{"architects": []string(architects)}
		if *rulesPath != "" {
			raw, err := os.ReadFile(*rulesPath)
			if err != nil {
				say(stderr, "hiverynd: read rules: %v\n", err)
				return 1
			}
			var rules []map[string]string
			if err := json.Unmarshal(raw, &rules); err != nil {
				say(stderr, "hiverynd: rules must be a JSON list of {architect?, from_prefix, to_prefix}: %v\n", err)
				return 1
			}
			request["rules"] = rules
		}
		body, _ = json.Marshal(request)
	case "apply":
		if fs.NArg() != 1 {
			say(stderr, "%s", referencesUsage)
			return exitUsage
		}
		var err error
		body, err = os.ReadFile(fs.Arg(0))
		if err != nil {
			say(stderr, "hiverynd: read plan: %v\n", err)
			return 1
		}
	}
	report, err := postMigration(strings.TrimRight(*daemonURL, "/")+"/api/reference-migration/"+mode, body)
	if err != nil {
		say(stderr, "hiverynd: references-migrate %s: %v\n", mode, err)
		return 1
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, report, "", "  "); err != nil {
		say(stderr, "hiverynd: format report: %v\n", err)
		return 1
	}
	pretty.WriteByte('\n')
	if _, err := stdout.Write(pretty.Bytes()); err != nil {
		return 1
	}
	var summary struct {
		Totals map[string]int `json:"totals"`
	}
	_ = json.Unmarshal(report, &summary)
	keys := make([]string, 0, len(summary.Totals))
	for k := range summary.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, summary.Totals[k]))
	}
	say(stderr, "references-migrate %s: %s\n", mode, strings.Join(parts, " "))
	return 0
}

func postMigration(url string, body []byte) (json.RawMessage, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode daemon response (HTTP %d): %w", resp.StatusCode, err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
	}
	if len(env.Data) == 0 {
		return nil, errors.New("daemon response has no data")
	}
	return env.Data, nil
}
