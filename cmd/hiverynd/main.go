package main

import (
	"flag"
	"io"
	"os"
	"strings"

	"github.com/hiveryn/daemon/internal/app"
	daemonmcp "github.com/hiveryn/daemon/internal/mcp"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the exit status. Bare flags (or no
// arguments) mean serve; an unknown command is a usage error, never a server.
func run(args []string, stdout, stderr io.Writer) int {
	command, rest := "serve", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, rest = args[0], args[1:]
	}

	var err error
	switch command {
	case "action":
		return runAction(rest, stdout, stderr)
	case "mcp":
		err = runMCP(rest)
	case "serve":
		err = runServe(rest)
	default:
		say(stderr, "hiverynd: unknown command %q\nusage: hiverynd [serve] [flags] | hiverynd mcp [flags] | hiverynd action validate [path]\n", command)
		return exitUsageErr
	}
	if err != nil {
		say(stderr, "daemon failed: %v\n", err)
		return 1
	}
	return 0
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to daemon bootstrap config")
	databasePath := fs.String("db", "", "Path to local SQLite state database")
	port := fs.Int("port", 0, "Override listen port (takes precedence over config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Run(*configPath, *databasePath, *port)
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	daemonURL := fs.String("daemon-url", os.Getenv("HIVERYN_DAEMON_URL"), "Daemon HTTP base URL")
	architectKey := fs.String("architect-key", os.Getenv("HIVERYN_ARCHITECT_KEY"), "Architect key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	server, err := daemonmcp.NewServer(daemonmcp.Config{
		DaemonURL:    *daemonURL,
		ArchitectKey: *architectKey,
		SessionType:  daemonmcp.SessionType(os.Getenv("HIVERYN_SESSION_TYPE")),
		SessionID:    os.Getenv("HIVERYN_SESSION_ID"),
	})
	if err != nil {
		return err
	}

	return server.Run()
}
