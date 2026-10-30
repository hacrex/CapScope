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

	Include *regexp.Regexp // only track containers whose name or app matches
	Exclude *regexp.Regexp // never track containers whose name or app matches
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	var errs []error
	c := Config{
		Listen:                env("LISTEN", ":8080"),
		DBPath:                env("DB_PATH", "/data/capscope.db"),
		DockerHost:            env("DOCKER_HOST", "unix:///var/run/docker.sock"),
		AuthUser:              env("AUTH_USER", "admin"),
		AuthPass:              os.Getenv("AUTH_PASS"),
		AllowNoAuth:           envBool("ALLOW_NO_AUTH", false),
		MaxDBMB:               int64(envInt("MAX_DB_MB", 1024)),
		InitialTail:           envInt("INITIAL_TAIL", 200),
		MaxLineBytes:          envInt("MAX_LINE_BYTES", 16384),
		Redact:                envBool("REDACT", true),
		SnapshotLogLines:      envInt("SNAPSHOT_LOG_LINES", 200),
		SnapshotMetricSamples: envInt("SNAPSHOT_METRIC_SAMPLES", 60),
		SnapshotKinds:         map[string]bool{},
		RestartLoopCount:      envInt("RESTART_LOOP_COUNT", 3),
		AccessLog:             env("ACCESS_LOG", "auto"),
		AnonIPS:               envBool("ANON_IPS", true),
		AlertsFile:            env("ALERTS_FILE", "/data/alerts.json"),
		AlertWebhookURL:       os.Getenv("ALERT_WEBHOOK_URL"),
		AlertTelegramTok:      os.Getenv("ALERT_TELEGRAM_TOKEN"),
		AlertTelegramChat:     os.Getenv("ALERT_TELEGRAM_CHAT_ID"),
		AlertSlackURL:         os.Getenv("ALERT_SLACK_URL"),
		AlertDiscordURL:       os.Getenv("ALERT_DISCORD_URL"),
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

	if c.AuthPass == "" && !c.AllowNoAuth {
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
	} {
		if ch.url != "" && !strings.HasPrefix(ch.url, "http://") && !strings.HasPrefix(ch.url, "https://") {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL", ch.key))
		}
	}
	if (c.AlertTelegramTok == "") != (c.AlertTelegramChat == "") {
		errs = append(errs, errors.New("ALERT_TELEGRAM_TOKEN and ALERT_TELEGRAM_CHAT_ID must be set together"))
	}
	return c, errors.Join(errs...)
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

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
