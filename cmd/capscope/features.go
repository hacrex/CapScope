package main

import (
	"fmt"
	"log/slog"

	"capscope/internal/config"
)

// features lists the feature gates and whether each is enabled. Features ship
// behind gates and graduate to GA once stable (implementation plan Phase 0).
// A feature is enabled when its backing config is present (e.g. OTEL_LISTEN
// set turns on the OTLP receiver) or when it is unconditionally on.
func features(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	gates := []struct {
		name  string
		on    bool
		why   string
		stage string // "experimental", "beta", "ga"
	}{
		{
			name:  "otlp",
			on:    cfg.OTELListen != "",
			why:   otlpWhy(cfg),
			stage: "experimental",
		},
		{
			name:  "pipelines",
			on:    cfg.PipelinesFile != "",
			why:   pipelinesWhy(cfg),
			stage: "experimental",
		},
		{
			name:  "traces",
			on:    cfg.OTELListen != "", // trace ingestion needs the OTLP receiver
			why:   "OTLP receiver provides /v1/traces",
			stage: "experimental",
		},
		{
			name:  "archive",
			on:    cfg.ArchiveBucket != "",
			why:   archiveWhy(cfg),
			stage: "experimental",
		},
		{
			name:  "incidents",
			on:    true,
			why:   "incident model and auto-create from alert firings",
			stage: "experimental",
		},
	}

	const pad = 12
	fmt.Println("CapScope feature gates:")
	for _, g := range gates {
		status := "off"
		if g.on {
			status = "on"
		}
		fmt.Printf("  %-*.*s  %-7s  [%s]  %s\n", pad, pad, g.name, status, g.stage, g.why)
	}
	fmt.Println()
	fmt.Println("Enable a feature by setting its backing env var (see README.md).")
	fmt.Println("OTEL_LISTEN turns on the OTLP receiver; PIPELINES_FILE turns on")
	fmt.Println("logs-to-metrics pipelines; ARCHIVE_BUCKET turns on Parquet archiving.")
	return nil
}

func otlpWhy(cfg config.Config) string {
	if cfg.OTELListen == "" {
		return "set OTEL_LISTEN (host:port) to enable OTLP HTTP receiver"
	}
	return fmt.Sprintf("OTEL_LISTEN=%s, OTEL_PATH=%s", cfg.OTELListen, cfg.OTELPath)
}

func pipelinesWhy(cfg config.Config) string {
	if cfg.PipelinesFile == "" {
		return "set PIPELINES_FILE to a pipelines.yaml to enable ingest-time rollups"
	}
	return fmt.Sprintf("PIPELINES_FILE=%s", cfg.PipelinesFile)
}

func archiveWhy(cfg config.Config) string {
	if cfg.ArchiveBucket == "" {
		return "set ARCHIVE_BUCKET to enable Parquet archive tier"
	}
	return fmt.Sprintf("ARCHIVE_BUCKET=%s ARCHIVE_PREFIX=%s RETENTION_HOT_DAYS=%d",
		cfg.ArchiveBucket, cfg.ArchivePrefix, cfg.RetentionHotDays)
}
