package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/hiveryn/daemon/internal/app"
)

func main() {
	configPath := flag.String("config", "", "Path to daemon bootstrap config")
	databasePath := flag.String("db", "", "Path to local SQLite state database")
	flag.Parse()

	if err := app.Run(*configPath, *databasePath); err != nil {
		slog.Error("daemon failed", "error", err)
		os.Exit(1)
	}
}
