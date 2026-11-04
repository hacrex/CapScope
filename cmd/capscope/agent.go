package main

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"

	"capscope/internal/agent"
	"capscope/internal/config"
)

// runAgent runs the CapScope agent: collects from local Docker and streams to a hub over mTLS.
func runAgent(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx, cfg, log)
}
