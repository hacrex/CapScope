package main

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"

	"capscope/internal/auth"
	"capscope/internal/config"
	"capscope/internal/mcp"
	"capscope/internal/store"
)

// runMCP runs the MCP (Model Context Protocol) server over stdio.
// It exposes read-only tools over the CapScope SQLite store:
//   query_logs, list_snapshots, get_snapshot, run_sql.
// Auth is handled via the session token passed in the MCP initialize request.
func runMCP(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sessions := auth.NewPersistentSessionManager(cfg.DBPath+".sessions", log)
	srv := mcp.New(st, sessions, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("capscope mcp server starting over stdio")
	if err := srv.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	log.Info("capscope mcp server stopped")
	return nil
}
