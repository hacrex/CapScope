// Package config loads CapScope settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen     string
	DBPath     string
	DockerHost string

	AuthUser    string
	AuthPass    string
	AllowNoAuth bool

	// Z5-prep: how callers prove who they are. "local" (default) is the shared
	// AUTH_USER/AUTH_PASS credential; "proxy" trusts an identity header set by
	// a reverse proxy in front of CapScope (Authelia, Authentik, Cloudflare
	// Access, ...). Proxy mode never challenges passwords itself.
	AuthProvider string
	// ProxyUserHeader is the header the reverse proxy sets to the username
	// (Authelia/Authentik and the Django/Servlet worlds use "Remote-User").
	ProxyUserHeader string

	LogRetention      time.Duration
	MetricsRetention  time.Duration
	SnapshotRetention time.Duration
	MaxDBMB           int64

	// S7: per-app overrides. AppRetention shortens log retention for a noisy
	// app (the global retention still wins as an upper bound); AppMaxMB caps
	// the stored log bytes of one app so it cannot monopolize the DB.
	AppRetention map[string]time.Duration
	AppMaxMB     map[string]int64

	StatsInterval time.Duration
	InitialTail   int
	MaxLineBytes  int
	Redact        bool

	SnapshotKinds         map[string]bool
	SnapshotLogLines      int
	RestartLoopCount      int // deaths of one container inside the window that flag a restart loop
	RestartLoopWindow     time.Duration
	SnapshotMetricSamples int

	AccessLog string // access-log detection: auto | all | off
	AnonIPS   bool   // store access-log client IPs anonymized (last octet/IPv6 hextets zeroed)

	AlertsFile     string        // JSON alert rules file (hot-reloaded; empty disables)
	AlertsInterval time.Duration // how often rules are evaluated
	// Notification channels (A5); empty entries are simply not configured.
	AlertWebhookURL   string
	AlertTelegramTok  string
	AlertTelegramChat string
	AlertSlackURL     string
	AlertDiscordURL   string
	AlertNtfyURL      string // ntfy topic URL, e.g. https://ntfy.sh/capscope
	AlertNtfyToken    string // optional ntfy access token

	// A7: daily jittered registry check comparing each container's image
	// manifest digest; fires "update_available" pseudo-alert events. Spin-free
	// installs can turn it off here.
	UpdateCheck         bool
	UpdateCheckInterval time.Duration

	Include *regexp.Regexp // only track containers whose name or app matches
	Exclude *regexp.Regexp // never track containers whose name or app matches

	// Agent mode (Phase 4): when AgentHub is set, this binary runs as an agent
	// that streams collected rows to a hub over mutual TLS instead of storing
	// them locally. host_id is stable across restarts.
	AgentHostID string // stable host identity; defaults to CAPSCOPE_HOST_ID then hostname
	AgentHub    string // hub address (host:port); empty = serve mode
	AgentCACert string // path to the hub CA cert PEM (trust anchor)
	AgentCert   string // path to the agent certificate PEM
	AgentKey    string // path to the agent private key PEM

	// DockerFilter is a label selector (name=value) limiting which containers
	// the agent collects, like Dozzle's agent filter.
	DockerFilter string

	// Agent hub (Phase 4.4): when AgentHubListen is set, CapScope listens for
	// agent connections and ingests their logs/metrics/events tagged by
	// host_id, merging many nodes into one store and UI.
	AgentHubListen string // host:port to listen for agents; empty disables agent ingestion
	// AgentHubPinnedFingerprints is a comma-separated list of SHA-256
	// fingerprints of agent client certificates allowed to connect. Empty
	// means any certificate signed by the trusted CA is accepted.
	AgentHubPinnedFingerprints string
	// AgentHubCert / AgentHubKey are the hub's server certificate presented to
	// agents; AgentHubCACert is the CA used to verify agent client certs.
	AgentHubCert   string
	AgentHubKey    string
	AgentHubCACert string

	// OIDC (Phase 3.2): when AuthProvider=oidc, CapScope delegates
	// authentication to an OpenID Connect provider.
	OIDCIssuer       string // OIDC issuer URL, used for Discovery
	OIDCClientID     string // OAuth 2.0 client ID
	OIDCClientSecret string // OAuth 2.0 client secret
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	var errs []error
	c := Config{
		Listen:                     env("LISTEN", ":8080"),
		DBPath:                     env("DB_PATH", "/data/capscope.db"),
		DockerHost:                 env("DOCKER_HOST", "unix:///var/run/docker.sock"),
		AuthUser:                   env("AUTH_USER", "admin"),
		AuthPass:                   os.Getenv("AUTH_PASS"),
		AllowNoAuth:                envBool("ALLOW_NO_AUTH", false, &errs),
		AuthProvider:               env("AUTH_PROVIDER", "local"),
		ProxyUserHeader:            env("PROXY_USER_HEADER", "Remote-User"),
		MaxDBMB:                    int64(envInt("MAX_DB_MB", 1024, &errs)),
		InitialTail:                envInt("INITIAL_TAIL", 200, &errs),
		MaxLineBytes:               envInt("MAX_LINE_BYTES", 16384, &errs),
		Redact:                     envBool("REDACT", true, &errs),
		SnapshotLogLines:           envInt("SNAPSHOT_LOG_LINES", 200, &errs),
		SnapshotMetricSamples:      envInt("SNAPSHOT_METRIC_SAMPLES", 60, &errs),
		SnapshotKinds:              map[string]bool{},
		RestartLoopCount:           envInt("RESTART_LOOP_COUNT", 3, &errs),
		AccessLog:                  env("ACCESS_LOG", "auto"),
		AnonIPS:                    envBool("ANON_IPS", true, &errs),
		AlertsFile:                 env("ALERTS_FILE", "/data/alerts.json"),
		AlertWebhookURL:            os.Getenv("ALERT_WEBHOOK_URL"),
		AlertTelegramTok:           os.Getenv("ALERT_TELEGRAM_TOKEN"),
		AlertTelegramChat:          os.Getenv("ALERT_TELEGRAM_CHAT_ID"),
		AlertSlackURL:              os.Getenv("ALERT_SLACK_URL"),
		AlertDiscordURL:            os.Getenv("ALERT_DISCORD_URL"),
		AlertNtfyURL:               os.Getenv("ALERT_NTFY_URL"),
		AlertNtfyToken:             os.Getenv("ALERT_NTFY_TOKEN"),
		UpdateCheck:                envBool("UPDATE_CHECK", true, &errs),
		AgentHostID:                env("AGENT_HOST_ID", os.Getenv("CAPSCOPE_HOST_ID")),
		AgentHub:                   env("AGENT_HUB", ""),
		AgentCACert:                env("AGENT_CA_CERT", ""),
		AgentCert:                  env("AGENT_CERT", ""),
		AgentKey:                   env("AGENT_KEY", ""),
		DockerFilter:               env("DOCKER_FILTER", ""),
		AgentHubListen:             env("AGENT_HUB_LISTEN", ""),
		AgentHubPinnedFingerprints: env("AGENT_HUB_PINNED_FINGERPRINTS", ""),
		AgentHubCert:               env("AGENT_HUB_CERT", ""),
		AgentHubKey:                env("AGENT_HUB_KEY", ""),
		AgentHubCACert:             env("AGENT_HUB_CA_CERT", ""),
		OIDCIssuer:                 env("OIDC_ISSUER", ""),
		OIDCClientID:               env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:           env("OIDC_CLIENT_SECRET", ""),
	}

	dur := func(key, def string) time.Duration {
		d, err := ParseDuration(env(key, def))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return d
	}
	c.LogRetention = dur("LOG_RETENTION", "7d")
	c.MetricsRetention = dur("METRICS_RETENTION", "7d")
	c.SnapshotRetention = dur("SNAPSHOT_RETENTION", "30d")
	c.StatsInterval = dur("STATS_INTERVAL", "10s")
	c.RestartLoopWindow = dur("RESTART_LOOP_WINDOW", "5m")
	c.AlertsInterval = dur("ALERTS_INTERVAL", "1m")
	c.UpdateCheckInterval = dur("UPDATE_CHECK_INTERVAL", "24h")

	for _, k := range strings.Split(env("SNAPSHOT_KINDS", "crash,oom"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.SnapshotKinds[k] = true
		}
	}

	// S7: per-app budgets, written as comma-separated app=value pairs.
	//   APP_RETENTION="web=3d,api=1d"   APP_MAX_MB="web=500,api=200"
	c.AppRetention = map[string]time.Duration{}
	c.AppMaxMB = map[string]int64{}
	for _, pair := range strings.Split(os.Getenv("APP_RETENTION"), ",") {
		app, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(app) == "" {
			continue
		}
		d, err := ParseDuration(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("APP_RETENTION %s: %w", app, err))
			continue
		}
		if d > 0 {
			c.AppRetention[strings.TrimSpace(app)] = d
		}
	}
	for _, pair := range strings.Split(os.Getenv("APP_MAX_MB"), ",") {
		app, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(app) == "" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("APP_MAX_MB %s: expected a non-negative number of MB", app))
			continue
		}
		if n > 0 {
			c.AppMaxMB[strings.TrimSpace(app)] = n
		}
	}

	rx := func(key string) *regexp.Regexp {
		v := os.Getenv(key)
		if v == "" {
			return nil
		}
		r, err := regexp.Compile(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return r
	}
	c.Include = rx("INCLUDE")
	c.Exclude = rx("EXCLUDE")

	switch c.AuthProvider {
	case "local":
	case "proxy":
		// Proxy header auth: no password required because the reverse proxy
		// owns authentication. The name must be a well-formed RFC 7230 field
		// name — reject junk early rather than trust it later.
		if !validHeaderName(c.ProxyUserHeader) {
			errs = append(errs, fmt.Errorf("PROXY_USER_HEADER must be a valid header name (got %q; e.g. Remote-User)", c.ProxyUserHeader))
		}
	case "oidc":
		// OIDC: Discovery, PKCE, redirect /api/oidc/callback.
		if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" {
			errs = append(errs, errors.New("AUTH_PROVIDER=oidc requires OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET"))
		}
	default:
		errs = append(errs, fmt.Errorf("AUTH_PROVIDER must be local, proxy or oidc (got %q)", c.AuthProvider))
	}
	// A password is only needed for local (password) auth.
	if c.AuthProvider == "local" && c.AuthPass == "" && !c.AllowNoAuth {
		errs = append(errs, errors.New("AUTH_PASS is required (set ALLOW_NO_AUTH=true only for local testing)"))
	}
	switch c.AccessLog {
	case "auto", "all", "off":
	default:
		errs = append(errs, fmt.Errorf("ACCESS_LOG must be one of auto, all, off (got %q)", c.AccessLog))
	}
	if c.StatsInterval < time.Second && c.StatsInterval != 0 {
		// Docker emits roughly one stats frame per second, so lower values add nothing.
		c.StatsInterval = time.Second
	}
	if c.RestartLoopCount < 2 {
		c.RestartLoopCount = 2 // one death is not a loop
	}
	if c.RestartLoopWindow < time.Minute {
		c.RestartLoopWindow = time.Minute
	}
	if c.AlertsInterval < 10*time.Second {
		c.AlertsInterval = 10 * time.Second // evaluation is cheap but not free
	}
	for _, ch := range []struct{ key, url string }{
		{"ALERT_WEBHOOK_URL", c.AlertWebhookURL},
		{"ALERT_SLACK_URL", c.AlertSlackURL},
		{"ALERT_DISCORD_URL", c.AlertDiscordURL},
		{"ALERT_NTFY_URL", c.AlertNtfyURL},
	} {
		if ch.url != "" && !strings.HasPrefix(ch.url, "http://") && !strings.HasPrefix(ch.url, "https://") {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL", ch.key))
		}
	}
	if c.AlertNtfyToken != "" && c.AlertNtfyURL == "" {
		errs = append(errs, errors.New("ALERT_NTFY_TOKEN is set but ALERT_NTFY_URL is empty"))
	}
	if c.UpdateCheckInterval < time.Hour {
		c.UpdateCheckInterval = time.Hour // a registry is checked from far away, not often
	}
	if (c.AlertTelegramTok == "") != (c.AlertTelegramChat == "") {
		errs = append(errs, errors.New("ALERT_TELEGRAM_TOKEN and ALERT_TELEGRAM_CHAT_ID must be set together"))
	}
	// MAX_DB_MB=0 (or negative) would make EnforceSize delete everything on
	// every maintenance cycle. Require a positive cap; the caller must treat
	// Load's error as fatal at startup.
	if c.MaxDBMB <= 0 {
		errs = append(errs, errors.New("MAX_DB_MB must be a positive number of megabytes"))
	}

	return c, errors.Join(errs...)
}

// validHeaderName checks RFC 7230 field-name characters: printable ASCII with
// no separator characters. The exact case does not matter — Header.Get
// canonicalizes the lookup — so "X-WebAuth-User" stays usable as written.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '!' || ch > '~' || ch == ':' {
			return false
		}
	}
	return true
}

// ParseDuration is time.ParseDuration plus a "d" (days) suffix.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int, errs *[]error) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		*errs = append(*errs, fmt.Errorf("%s: not a valid integer: %q", k, v))
	}
	return def
}

func envBool(k string, def bool, errs *[]error) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		*errs = append(*errs, fmt.Errorf("%s: not a valid boolean: %q", k, v))
	}
	return def
}
