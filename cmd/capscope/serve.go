package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"capscope/internal/alerts"
	"capscope/internal/api"
	"capscope/internal/auth"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/hubagent"
	"capscope/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// levelFromEnv reads LOG_LEVEL for the slog logger.
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

// serve runs the CapScope web API and UI (default command).
// Docker connection is non-blocking: the HTTP server starts immediately so the
// UI is responsive even when Docker is slow or unreachable. Docker-dependent
// features (log tailing, snapshot collection) retry in the background.
func serve(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AuthPass == "" {
		if cfg.AuthProvider == "proxy" {
			log.Warn("AUTH_PROVIDER=proxy: trusting the " + cfg.ProxyUserHeader + " header from anyone who reaches this port; publish only the reverse proxy and keep CapScope on an internal network")
		} else {
			log.Warn("authentication is disabled (ALLOW_NO_AUTH=true); anyone who can reach this port can read all container logs")
		}
	}
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := hub.New()

	// Alerts (A1 to A7): an engine over the store, fed by hot-reloaded rules
	// and notification channels. Runs independently of Docker connectivity.
	chans := alerts.BuildChannels(cfg.AlertWebhookURL, cfg.AlertTelegramTok, cfg.AlertTelegramChat, cfg.AlertSlackURL, cfg.AlertDiscordURL, cfg.AlertNtfyURL, cfg.AlertNtfyToken)
	eng := alerts.New(st, cfg.AlertsFile, cfg.AlertsInterval, chans, log)
	go eng.Run(ctx)

	mgr := collector.NewManager(cfg, dk, st, h, log)
	mgr.SetEventSink(eng.FeedEvent) // event rules (A6) fire on live Docker events
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()

	if cfg.UpdateCheck {
		up := alerts.NewUpdater(mgr, func(ctx context.Context, image string) ([]string, error) {
			ii, err := dk.ImageInspect(ctx, image)
			if err != nil {
				return nil, err
			}
			return ii.RepoDigests, nil
		}, log, cfg.UpdateCheckInterval)
		go up.Run(ctx, eng.FeedEvent)
	} else {
		log.Info("update check disabled", "env", "UPDATE_CHECK=false")
	}

	// Local password auth needs a session store; proxy header auth is sessionless.
	opts := []func(*api.Server){api.WithAlerts(eng), api.WithUserStore(st)}
	if cfg.AuthProvider != "proxy" {
		// Sessions persist HMAC-signed next to the DB, so a restart does not
		// log the browser out (Phase 3.1). The file never receives raw tokens.
		opts = append(opts, api.WithAuth(auth.NewPersistentSessionManager(cfg.DBPath+".sessions", log)))
	} else if cfg.AuthPass != "" {
		log.Warn("AUTH_PASS set but ignored: AUTH_PROVIDER=proxy authenticates via " + cfg.ProxyUserHeader)
	}
	// Seed the first user from AUTH_USER/AUTH_PASS if no users exist yet
	// (Phase 3.3). For local auth, hash the password with bcrypt.
	if cfg.AuthProvider == "local" && cfg.AuthPass != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AuthPass), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("bcrypt: %w", err)
		}
		created, err := st.SeedFirstUser(cfg.AuthUser, string(hash))
		if err != nil {
			return fmt.Errorf("seed first user: %w", err)
		}
		if created {
			log.Info("seeded first user", "user", cfg.AuthUser, "role", "admin")
		}
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(cfg, st, mgr, h, log, opts...).Handler(),
		ReadHeaderTimeout: 10 * time.Second, // no WriteTimeout: SSE streams are long-lived
	}
	// Agent ingestion (Phase 4.4): accept agent connections over mTLS and
	// ingest their logs/metrics/events tagged by host_id.
	if cfg.AgentHubListen != "" {
		var hubCert, hubKey, caCert []byte
		if cfg.AgentHubCert != "" {
			hubCert, err = os.ReadFile(cfg.AgentHubCert)
			if err != nil {
				return fmt.Errorf("read AGENT_HUB_CERT: %w", err)
			}
		}
		if cfg.AgentHubKey != "" {
			hubKey, err = os.ReadFile(cfg.AgentHubKey)
			if err != nil {
				return fmt.Errorf("read AGENT_HUB_KEY: %w", err)
			}
		}
		if cfg.AgentHubCACert != "" {
			caCert, err = os.ReadFile(cfg.AgentHubCACert)
			if err != nil {
				return fmt.Errorf("read AGENT_HUB_CA_CERT: %w", err)
			}
		}
		pins := strings.Split(cfg.AgentHubPinnedFingerprints, ",")
		hs := hubagent.New(st, h, eng, log, pins...)
		ln, err := net.Listen("tcp", cfg.AgentHubListen)
		if err != nil {
			return fmt.Errorf("listen AGENT_HUB_LISTEN: %w", err)
		}
		// Close the listener on shutdown so Run's accept loop exits instead of
		// blocking forever (and accepting against a closed store).
		go func() {
			<-ctx.Done()
			_ = ln.Close()
		}()
		go func() {
			_ = hs.Run(ctx, ln, string(hubCert), string(hubKey), string(caCert))
		}()
		log.Info("hubagent: agent ingestion enabled", "listen", cfg.AgentHubListen)
	}

	// Background Docker health: retry ping until Docker is reachable.
	// The UI shows "connecting" until the first successful ping, then live-tail
	// and snapshot collection start automatically.
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if ctx.Err() != nil {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := dk.Ping(pctx)
			cancel()
			if err == nil {
				log.Info("docker connected", "host", cfg.DockerHost)
				return
			}
			log.Debug("docker not yet reachable", "host", cfg.DockerHost, "err", err)
		}
	}()

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
