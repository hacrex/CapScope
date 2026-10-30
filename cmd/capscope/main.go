package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"capscope/internal/alerts"
	"capscope/internal/api"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelFromEnv()}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthcheck probes our own /healthz without a shell in the image (scratch).
func healthcheck() int {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	addr = strings.Replace(addr, "0.0.0.0", "127.0.0.1", 1)
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AuthPass == "" {
		log.Warn("authentication is disabled (ALLOW_NO_AUTH=true); anyone who can reach this port can read all container logs")
	}
	if i := strings.LastIndex(cfg.DBPath, "/"); i > 0 {
		if err := os.MkdirAll(cfg.DBPath[:i], 0o755); err != nil {
			return err
		}
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	dk, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = dk.Ping(pctx)
	cancel()
	if err != nil {
		return errors.New("cannot reach Docker at " + cfg.DockerHost + ": " + err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := hub.New()
	mgr := collector.NewManager(cfg, dk, st, h, log)
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()

	// Alerts (A1 to A5): an engine over the store, fed by a hot-reloaded JSON
	// rules file and a set of notification channels from the environment.
	chans := alerts.BuildChannels(cfg.AlertWebhookURL, cfg.AlertTelegramTok, cfg.AlertTelegramChat, cfg.AlertSlackURL, cfg.AlertDiscordURL)
	eng := alerts.New(st, cfg.AlertsFile, cfg.AlertsInterval, chans, log)
	go eng.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(cfg, st, mgr, h, log, api.WithAlerts(eng)).Handler(),
		ReadHeaderTimeout: 10 * time.Second, // no WriteTimeout: SSE streams are long-lived
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("capscope started", "listen", cfg.Listen, "db", cfg.DBPath, "docker", cfg.DockerHost)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done // let the writer flush buffered rows
	log.Info("stopped")
	return nil
}

func levelFromEnv() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
