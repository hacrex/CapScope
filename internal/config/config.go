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

	StatsInterval time.Duration
	InitialTail   int
	MaxLineBytes  int
	Redact        bool

	SnapshotKinds         map[string]bool
	SnapshotLogLines      int
	SnapshotMetricSamples int

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

	for _, k := range strings.Split(env("SNAPSHOT_KINDS", "crash,oom"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.SnapshotKinds[k] = true
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
	if c.StatsInterval < time.Second && c.StatsInterval != 0 {
		// Docker emits roughly one stats frame per second, so lower values add nothing.
		c.StatsInterval = time.Second
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
