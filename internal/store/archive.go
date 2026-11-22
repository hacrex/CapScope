// Package store extends the SQLite schema with storage tiering (Phase 4).
//
// A background job rolls logs/metrics/spans older than RETENTION_HOT_DAYS
// from SQLite to archive files (newline-delimited JSON, partitioned by
// day + app/host_id) on the local disk. An archive_index table tracks what
// was rolled off so queries can fall back to the archive tier for older
// ranges.
//
// The archive tier is local-first: files live under ARCHIVE_PREFIX (a
// directory path). MAX_DB_MB enforcement only applies to the hot SQLite DB;
// archived data is exempt.

package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager rolls hot SQLite data off to archive files and serves tiered
// queries (SQLite first, archive fallback).
type Manager struct {
	st      *Store
	log     *slog.Logger
	bucket  string // archive destination: directory path
	prefix  string // key prefix inside the bucket
	hotDays int    // days of data kept hot in SQLite

	mu    sync.Mutex
	runng bool // a rollup is in flight
}

// NewArchiveManager creates an archive manager. When bucket is empty,
// archiving is disabled and the manager is a no-op.
func NewArchiveManager(st *Store, bucket, prefix string, hotDays int, log *slog.Logger) *Manager {
	if prefix == "" {
		prefix = "capscope/"
	}
	if hotDays <= 0 {
		hotDays = 30
	}
	return &Manager{st: st, bucket: bucket, prefix: strings.TrimRight(prefix, "/"), hotDays: hotDays, log: log}
}

// Enabled returns true when an archive bucket/directory is configured.
func (m *Manager) Enabled() bool {
	return m.bucket != ""
}

// Rollup rolls logs/metrics/spans older than hotDays into archive files.
// Returns the number of rows archived. Safe to call concurrently; only one
// rollup runs at a time.
func (m *Manager) Rollup(ctx context.Context) (int64, error) {
	if !m.Enabled() {
		return 0, nil
	}
	m.mu.Lock()
	if m.runng {
		m.mu.Unlock()
		return 0, nil
	}
	m.runng = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.runng = false
		m.mu.Unlock()
	}()

	cutoff := time.Now().Add(-time.Duration(m.hotDays) * 24 * time.Hour).UnixMilli()
	var total int64

	n, err := m.archiveLogs(ctx, cutoff)
	if err != nil {
		m.log.Warn("archive: logs rollup failed", "err", err)
	} else {
		total += n
	}
	n, err = m.archiveMetrics(ctx, cutoff)
	if err != nil {
		m.log.Warn("archive: metrics rollup failed", "err", err)
	} else {
		total += n
	}
	n, err = m.archiveSpans(ctx, cutoff)
	if err != nil {
		m.log.Warn("archive: spans rollup failed", "err", err)
	} else {
		total += n
	}
	return total, nil
}

// archiveLogs rolls log rows older than cutoff into per-day/app files and
// deletes them from SQLite. Returns rows archived.
func (m *Manager) archiveLogs(ctx context.Context, cutoff int64) (int64, error) {
	rows, err := m.st.QueryLogs(ctx, LogQuery{
		UntilMs: cutoff,
		Limit:   10000,
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	groups := map[string][]LogRow{}
	for _, r := range rows {
		day := time.Unix(0, r.TS).UTC().Format("2006-01-02")
		key := day + "|" + r.App + "|" + r.HostID
		groups[key] = append(groups[key], r)
	}
	var total int64
	for key, group := range groups {
		parts := strings.SplitN(key, "|", 3)
		day, app, hostID := parts[0], parts[1], parts[2]
		path, err := m.writeJSONL("logs", day, app, hostID, group)
		if err != nil {
			return total, err
		}
		if err := m.st.RecordArchive(ctx, "logs", day, app, hostID, path, int64(len(group)), group[0].TSMs, group[len(group)-1].TSMs); err != nil {
			return total, err
		}
		total += int64(len(group))
	}
	if err := m.st.DeleteLogsBefore(ctx, cutoff); err != nil {
		return total, err
	}
	return total, nil
}

// archiveMetrics rolls metric samples older than cutoff into archive files.
func (m *Manager) archiveMetrics(ctx context.Context, cutoff int64) (int64, error) {
	rows, err := m.st.QueryMetrics(ctx, "", "", "", 0, cutoff, 0)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	groups := map[string][]MetricPoint{}
	for _, p := range rows {
		day := time.UnixMilli(p.TS).UTC().Format("2006-01-02")
		key := day + "|" + p.App + "|" + p.HostID
		groups[key] = append(groups[key], p)
	}
	var total int64
	for key, group := range groups {
		parts := strings.SplitN(key, "|", 3)
		day, app, hostID := parts[0], parts[1], parts[2]
		path, err := m.writeJSONL("metrics", day, app, hostID, group)
		if err != nil {
			return total, err
		}
		if err := m.st.RecordArchive(ctx, "metrics", day, app, hostID, path, int64(len(group)), group[0].TS, group[len(group)-1].TS); err != nil {
			return total, err
		}
		total += int64(len(group))
	}
	if err := m.st.DeleteMetricsBefore(ctx, cutoff); err != nil {
		return total, err
	}
	return total, nil
}

// archiveSpans rolls trace spans older than cutoff into archive files.
func (m *Manager) archiveSpans(ctx context.Context, cutoff int64) (int64, error) {
	rows, err := m.st.QuerySpansBefore(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	groups := map[string][]Span{}
	for _, s := range rows {
		day := time.UnixMilli(s.StartTs / 1_000_000).UTC().Format("2006-01-02")
		key := day + "|" + s.Service + "|" + s.HostID
		groups[key] = append(groups[key], s)
	}
	var total int64
	for key, group := range groups {
		parts := strings.SplitN(key, "|", 3)
		day, service, hostID := parts[0], parts[1], parts[2]
		path, err := m.writeJSONL("spans", day, service, hostID, group)
		if err != nil {
			return total, err
		}
		if err := m.st.RecordArchive(ctx, "spans", day, service, hostID, path, int64(len(group)), group[0].StartTs/1_000_000, group[len(group)-1].StartTs/1_000_000); err != nil {
			return total, err
		}
		total += int64(len(group))
	}
	if err := m.st.DeleteSpansBefore(ctx, cutoff); err != nil {
		return total, err
	}
	return total, nil
}

// writeJSONL writes rows as newline-delimited JSON to a partitioned file:
// <prefix>/<kind>/<day>/<app>/<host_id>-<seq>.jsonl
func (m *Manager) writeJSONL(kind, day, app, hostID string, rows any) (string, error) {
	dir := filepath.Join(m.prefix, kind, day, app, hostID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := filepath.Join(dir, fmt.Sprintf("%d.jsonl", time.Now().UnixMilli()))
	f, err := os.Create(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	enc := json.NewEncoder(w)
	switch v := rows.(type) {
	case []LogRow:
		for _, r := range v {
			if err := enc.Encode(r); err != nil {
				return "", err
			}
		}
	case []MetricPoint:
		for _, r := range v {
			if err := enc.Encode(r); err != nil {
				return "", err
			}
		}
	case []Span:
		for _, r := range v {
			if err := enc.Encode(r); err != nil {
				return "", err
			}
		}
	}
	return name, nil
}
