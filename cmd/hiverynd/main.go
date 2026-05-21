package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hiveryn/daemon/internal/app"
	daemonmcp "github.com/hiveryn/daemon/internal/mcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "daemon failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "mcp":
			return runMCP(args[1:])
		case "serve":
			return runServe(args[1:])
		}
	}

	return runServe(args)
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
