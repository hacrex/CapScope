package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// version is set at build time with -ldflags "-X capscope/cmd version=...".
// It defaults to "dev".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelFromEnv()}))
	if err := run(log, os.Args[1:]); err != nil && !errors.Is(err, errHelp) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

var errHelp = errors.New("help")

func run(log *slog.Logger, args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve", "":
		return serve(log)
	case "agent":
		return runAgent(log)
	case "healthcheck", "--healthcheck": // "--healthcheck" matches the Dockerfile HEALTHCHECK line
		return runHealthcheck(log)
	case "version":
		fmt.Printf("capscope %s\n", version)
		return nil
	case "generate-certs":
		return runGenerateCerts(log)
	case "mcp":
		return runMCP(log)
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return errHelp
	default:
		return fmt.Errorf("unknown command %q (try 'capscope serve', 'capscope agent', 'capscope healthcheck', 'capscope version', 'capscope generate-certs', 'capscope mcp')", cmd)
	}
}

const usageText = `CapScope — read-only Docker log viewer with SQLite persistence

Usage: capscope <command> [arguments]

Commands:
  serve            Run the CapScope web API and UI (default).
  agent            Stream local Docker logs/metrics/events to a hub over mTLS.
  healthcheck      Probe /healthz; exit 0 when healthy, 1 otherwise.
  version          Print the build version.
  generate-certs   Generate a CA + agent certificate pair for mTLS.
  mcp              Run the MCP (Model Context Protocol) server over stdio.

Environment is read from 12-factor variables; see README.md for the full table.
`
