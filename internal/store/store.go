// Package store persists logs, metrics, container metadata and crash snapshots in SQLite.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"capscope/internal/logproc"
)

type Store struct {
	w *sql.DB // single writer connection
	r *sql.DB // read-only pool

	prunesTotal atomic.Int64 // rows pruned by retention / size enforcement (prometheus: capscope_retention_prunes_total)
	normalized  atomic.Bool  // true after NormalizeSchema has removed container/app columns from logs
}

var migrations = []string{
	`CREATE TABLE logs(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,              -- unix nanoseconds
	   container_id TEXT NOT NULL,
	   container TEXT NOT NULL,
	   app TEXT NOT NULL,
	   stream TEXT NOT NULL,             -- 'o' stdout, 'e' stderr
	   level TEXT NOT NULL DEFAULT '',
	   line TEXT NOT NULL
	 );
	 CREATE INDEX idx_logs_app_id ON logs(app, id);
	 CREATE INDEX idx_logs_cid_id ON logs(container_id, id);
	 CREATE INDEX idx_logs_ts ON logs(ts);

	 CREATE TABLE metrics(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,              -- unix milliseconds
	   container_id TEXT NOT NULL,
	   app TEXT NOT NULL,
	   cpu REAL NOT NULL, mem INTEGER NOT NULL, mem_limit INTEGER NOT NULL,
	   net_rx INTEGER NOT NULL, net_tx INTEGER NOT NULL,
	   blk_r INTEGER NOT NULL, blk_w INTEGER NOT NULL, pids INTEGER NOT NULL
	 );
	 CREATE INDEX idx_metrics_app_ts ON metrics(app, ts);
	 CREATE INDEX idx_metrics_cid_ts ON metrics(container_id, ts);

	 CREATE TABLE containers(
	   id TEXT PRIMARY KEY,
	   name TEXT NOT NULL, app TEXT NOT NULL, image TEXT NOT NULL, replica TEXT NOT NULL,
	   state TEXT NOT NULL,
	   first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL   -- unix ms
	 );
	 CREATE INDEX idx_containers_app ON containers(app);

	 CREATE TABLE snapshots(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,              -- unix ms
	   kind TEXT NOT NULL,               -- crash | oom | killed | clean_exit
	   container_id TEXT NOT NULL, container TEXT NOT NULL, app TEXT NOT NULL, image TEXT NOT NULL,
	   exit_code INTEGER NOT NULL, oom_killed INTEGER NOT NULL,
	   error TEXT NOT NULL, restart_count INTEGER NOT NULL,
	   mem_limit INTEGER NOT NULL, uptime_sec INTEGER NOT NULL,
	   summary TEXT NOT NULL,
	   logs_json TEXT NOT NULL, metrics_json TEXT NOT NULL, inspect_json TEXT NOT NULL
	 );
	 CREATE INDEX idx_snapshots_app_ts ON snapshots(app, ts);
	 CREATE INDEX idx_snapshots_ts ON snapshots(ts);`,

	// FTS5 index over logs.line. External content: the text stays in logs,
	// triggers keep the index in sync with inserts and the retention deletes.
	`CREATE VIRTUAL TABLE logs_fts USING fts5(line, content='logs', content_rowid='id');
	 CREATE TRIGGER logs_ai AFTER INSERT ON logs BEGIN
	   INSERT INTO logs_fts(rowid, line) VALUES (new.id, new.line);
	 END;
	 CREATE TRIGGER logs_ad AFTER DELETE ON logs BEGIN
	   INSERT INTO logs_fts(logs_fts, rowid, line) VALUES('delete', old.id, old.line);
	 END;
	 INSERT INTO logs_fts(logs_fts) VALUES('rebuild');`,

	// Parsed access-log rows (nginx combined / common). ts is unix ms, taken
	// from the Docker log timestamp of the emitting container.
	`CREATE TABLE access(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,
	   container_id TEXT NOT NULL, container TEXT NOT NULL, app TEXT NOT NULL,
	   ip TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL,
	   status INTEGER NOT NULL, bytes INTEGER NOT NULL,
	   referer TEXT NOT NULL DEFAULT '', ua TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX idx_access_app_ts ON access(app, ts);
	 CREATE INDEX idx_access_ts ON access(ts);`,

	// Crash catch-up (C8) and restart-loop detection (X7):
	// die_ts is the death time from the Docker event (0 for older rows);
	// restarts_window is how many times the container had died inside the
	// loop window at that moment. state is a tiny integer key-value table
	// holding the Docker event cursor so restarts can replay missed deaths.
	`ALTER TABLE snapshots ADD COLUMN die_ts INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE snapshots ADD COLUMN restarts_window INTEGER NOT NULL DEFAULT 0;
	 CREATE INDEX idx_snapshots_cid_die ON snapshots(container_id, die_ts);
	 CREATE TABLE state(key TEXT PRIMARY KEY, value INTEGER NOT NULL);`,

	// Alert history (A1 to A5): every firing is recorded here, and the last
	// firing per (rule, app) doubles as the silence check that prevents
	// re-notifying on every evaluation tick.
	`CREATE TABLE alerts(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,             -- unix milliseconds, firing time
	   rule TEXT NOT NULL,
	   app TEXT NOT NULL,
	   kind TEXT NOT NULL,
	   message TEXT NOT NULL
	 );
	 CREATE INDEX idx_alerts_app_ts ON alerts(app, ts);
	 CREATE INDEX idx_alerts_rule_app_ts ON alerts(rule, app, ts);`,

	// Delivery status (v0.7): notify() records per-channel outcomes so the
	// alert history table can show whether each firing actually reached
	// someone. "sent" = at least one channel succeeded, "failed" = all
	// attempted channels errored, "" = not yet recorded (older rows).
	`ALTER TABLE alerts ADD COLUMN delivery TEXT NOT NULL DEFAULT '';`,

	// Deploy markers (V9): a user (or CI) records a release so the charts and
	// the log timeline can show it, and a crash can be correlated with the
	// deploy that preceded it.
	`CREATE TABLE deploys(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,             -- unix milliseconds, deploy time
	   app TEXT NOT NULL,
	   version TEXT NOT NULL DEFAULT '',
	   note TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX idx_deploys_app_ts ON deploys(app, ts);`,

	// Expiring share links (V10): capability links "/s/<token>" that restore a
	// saved view (query string + optional crash hash) until they expire.
	`CREATE TABLE shares(
	   token TEXT PRIMARY KEY,
	   created_ms INTEGER NOT NULL,
	   expires_ms INTEGER NOT NULL,
	   qs TEXT NOT NULL,
	   hash TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX idx_shares_expiry ON shares(expires_ms);`,

	// Agent host ingestion (Phase 4.4): every stored row carries host_id so
	// the hub can merge data from many agents and the UI can filter per host.
	`ALTER TABLE logs ADD COLUMN host_id TEXT NOT NULL DEFAULT '';
	 ALTER TABLE metrics ADD COLUMN host_id TEXT NOT NULL DEFAULT '';
	 ALTER TABLE containers ADD COLUMN host_id TEXT NOT NULL DEFAULT '';
	 ALTER TABLE snapshots ADD COLUMN host_id TEXT NOT NULL DEFAULT '';
	 CREATE INDEX idx_logs_host ON logs(host_id);
	 CREATE INDEX idx_metrics_host ON metrics(host_id);
	 CREATE INDEX idx_containers_host ON containers(host_id);
	 CREATE INDEX idx_snapshots_host ON snapshots(host_id);
	 CREATE TABLE hosts(
	   host_id TEXT PRIMARY KEY,
	   labels_json TEXT NOT NULL DEFAULT '',
	   first_seen INTEGER NOT NULL,
	   last_seen INTEGER NOT NULL
	 );
	 CREATE INDEX idx_hosts_last_seen ON hosts(last_seen);`,

	// Multi-user (Phase 3.3): users table replaces the single shared
	// credential, and access rows record who made each request for audit.
	`CREATE TABLE users(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   name TEXT NOT NULL UNIQUE,
	   password_hash TEXT NOT NULL,
	   role TEXT NOT NULL DEFAULT 'viewer',
	   created_ms INTEGER NOT NULL
	 );
	 CREATE INDEX idx_users_name ON users(name);
	 ALTER TABLE access ADD COLUMN user TEXT NOT NULL DEFAULT '';`,

	// Dashboard performance (Phase 5): composite indexes so /api/dashboard and
	// /api/apps can scan the last-hour/error windows without touching every
	// row of a multi-gigabyte logs table.
	`CREATE INDEX IF NOT EXISTS idx_logs_ts_app ON logs(ts, app);
	 CREATE INDEX IF NOT EXISTS idx_snapshots_ts_app ON snapshots(ts, app);`,
	// Per-user application scopes (RBAC): admins have all apps; viewers must
	// have an explicit row in this table for each application they may inspect.
	`CREATE TABLE user_apps(
	   user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	   app TEXT NOT NULL,
	   PRIMARY KEY(user_id, app)
	 );
	 CREATE INDEX idx_user_apps_app ON user_apps(app);`,
	// S6 row normalization compatibility view: exposes the old `logs` column
	// set (including container and app names) via a JOIN with containers, so
	// existing SQL queries and /api/sql keep working after NormalizeSchema
	// removes those columns from the logs table itself. Uses LEFT JOIN so
	// log rows survive even if their container row has been removed.
	`CREATE VIEW IF NOT EXISTS logs_view AS
	   SELECT l.id, l.ts, l.container_id, c.name AS container, c.app AS app,
	      l.host_id, l.stream, l.level, l.line
	   FROM logs l LEFT JOIN containers c ON l.container_id = c.id;`,
	// Log cluster templates (semantic clustering): groups log lines that share
	// the same template (variable parts replaced with placeholders) so that
	// recurring messages can be seen as clusters instead of individual rows.
	`CREATE TABLE IF NOT EXISTS log_clusters(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   template_hash TEXT NOT NULL UNIQUE,
	   template TEXT NOT NULL,
	   sample_line TEXT NOT NULL,
	   count INTEGER NOT NULL DEFAULT 1,
	   first_seen INTEGER NOT NULL,   -- unix ms
	   last_seen INTEGER NOT NULL,    -- unix ms
	   app TEXT NOT NULL DEFAULT '',
	   level TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX IF NOT EXISTS idx_log_clusters_hash ON log_clusters(template_hash);
	 CREATE INDEX IF NOT EXISTS idx_log_clusters_app ON log_clusters(app);
	 CREATE INDEX IF NOT EXISTS idx_log_clusters_last_seen ON log_clusters(last_seen);`,
	// Evidence chain (immutable audit): tamper-evident hash chain over log rows.
	// Each entry chains SHA256(prev_hash + data_hash), so modifying any row
	// breaks all subsequent chain entries. The head hash can be published
	// externally (e.g., DNS, tweet, blockchain) as proof of integrity.
	`CREATE TABLE IF NOT EXISTS chain(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   seq INTEGER NOT NULL UNIQUE,           -- sequence number
	   prev_hash TEXT NOT NULL,               -- hash of previous entry
	   entry_hash TEXT NOT NULL UNIQUE,       -- hash of prev_hash + data_hash
	   data_hash TEXT NOT NULL,               -- hash of batched row data
	   row_count INTEGER NOT NULL DEFAULT 0,  -- number of rows in this batch
	   ts_ms INTEGER NOT NULL                 -- unix ms when entry was written
	 );
	 CREATE INDEX IF NOT EXISTS idx_chain_seq ON chain(seq);
	 CREATE INDEX IF NOT EXISTS idx_chain_entry_hash ON chain(entry_hash);`,
	// Code-to-log correlation: add git_commit column to containers table.
	// Populated from Docker image labels (org.opencontainers.image.revision).
	`ALTER TABLE containers ADD COLUMN git_commit TEXT NOT NULL DEFAULT '';`,
	`CREATE INDEX IF NOT EXISTS idx_containers_git_commit ON containers(git_commit);`,

	// Distributed tracing (Phase 3): spans table with indexes on trace_id and
	// service for trace waterfall and service-graph queries.
	`CREATE TABLE IF NOT EXISTS spans(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   trace_id TEXT NOT NULL,
	   span_id TEXT NOT NULL,
	   parent_span_id TEXT NOT NULL DEFAULT '',
	   service TEXT NOT NULL,
	   name TEXT NOT NULL,
	   kind TEXT NOT NULL DEFAULT '',
	   start_ts INTEGER NOT NULL,   -- unix nanoseconds
	   end_ts INTEGER NOT NULL DEFAULT 0,  -- unix nanoseconds (0 = still open)
	   status TEXT NOT NULL DEFAULT '',
	   attributes TEXT NOT NULL DEFAULT '{}',  -- JSON blob
	   host_id TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX IF NOT EXISTS idx_spans_trace_id ON spans(trace_id);
	 CREATE INDEX IF NOT EXISTS idx_spans_service ON spans(service);
	 CREATE INDEX IF NOT EXISTS idx_spans_parent ON spans(parent_span_id);
	 CREATE INDEX IF NOT EXISTS idx_spans_host ON spans(host_id);`,

	// GeoIP (C9.7): country label on parsed access-log rows.
	`ALTER TABLE access ADD COLUMN country TEXT NOT NULL DEFAULT '';
	 CREATE INDEX IF NOT EXISTS idx_access_country ON access(country);`,

	// Storage tiering (Phase 4): archive_index tracks which Parquet/JSONL
	// archive files hold rolled-off data, so queries can fall back to the
	// archive tier for older ranges.
	`CREATE TABLE IF NOT EXISTS archive_index(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   kind TEXT NOT NULL,              -- logs | metrics | spans
	   day TEXT NOT NULL,               -- YYYY-MM-DD partition
	   app TEXT NOT NULL DEFAULT '',
	   host_id TEXT NOT NULL DEFAULT '',
	   path TEXT NOT NULL,              -- file path or object key
	   rows INTEGER NOT NULL DEFAULT 0,
	   min_ts INTEGER NOT NULL,         -- unix ms
	   max_ts INTEGER NOT NULL,         -- unix ms
	   created_ms INTEGER NOT NULL
	 );
	 CREATE INDEX IF NOT EXISTS idx_archive_kind_day ON archive_index(kind, day);
	 CREATE INDEX IF NOT EXISTS idx_archive_app ON archive_index(kind, app, day);
	 CREATE INDEX IF NOT EXISTS idx_archive_ts ON archive_index(min_ts, max_ts);`,

	// Incident management (Phase 5): incidents turn alert firings into
	// operational workflow with open/ack/resolve lifecycle.
	`CREATE TABLE IF NOT EXISTS incidents(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   title TEXT NOT NULL,
	   severity TEXT NOT NULL DEFAULT 'warning',
	   state TEXT NOT NULL DEFAULT 'open',   -- open | ack | resolved
	   started_at INTEGER NOT NULL,          -- unix ms
	   ended_at INTEGER NOT NULL DEFAULT 0,
	   related_alert_ids TEXT NOT NULL DEFAULT '',  -- JSON array
	   app TEXT NOT NULL DEFAULT '',
	   host_id TEXT NOT NULL DEFAULT ''
	 );
	 CREATE INDEX IF NOT EXISTS idx_incidents_state ON incidents(state);
	 CREATE INDEX IF NOT EXISTS idx_incidents_app ON incidents(app);
	 CREATE INDEX IF NOT EXISTS idx_incidents_started ON incidents(started_at);`,

	// Audit log (Phase 7): append-only audit trail for security-sensitive actions.
	`CREATE TABLE IF NOT EXISTS audit_log(
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ts INTEGER NOT NULL,               -- unix ms
	   actor TEXT NOT NULL,               -- user id or "anonymous"/"proxy"
	   action TEXT NOT NULL,              -- sql | alert_rule | deploy | incident | user_mgmt
	   resource TEXT NOT NULL,            -- e.g. "/api/sql", "alert_rules"
	   target TEXT NOT NULL DEFAULT '',   -- target resource id or name
	   meta TEXT NOT NULL DEFAULT '{}'    -- JSON blob of additional context
	 );
	 CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
	 CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor);
	 CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action);`,
}

// Open creates or migrates the database at path.
func Open(path string) (*Store, error) {
	// cache_size is in KiB (negative = KiB). The driver default (2 MiB) is
	// per connection, and this store keeps 1 writer + 4 readers, so the page
	// cache alone could reach ~10 MiB with no ceiling of our own. Capping it
	// keeps RSS predictable; 1 MiB per connection is ample for the small
	// point lookups and indexed log scans this does.
	base := "file:" + path + "?_busy_timeout=30000&_journal_mode=WAL&_synchronous=NORMAL&_pragma=cache_size(-1000)"
	w, err := sql.Open("sqlite", base)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	r, err := sql.Open("sqlite", base+"&_query_only=1")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return &Store{w: w, r: r}, nil
}

func (s *Store) Close() error {
	s.r.Close()
	return s.w.Close()
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		if i == 0 {
			// Only effective before the first table exists; lets retention shrink the file.
			if _, err := db.Exec("PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
				return err
			}
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			// Heal a half-applied upgrade: an earlier build may already have
			// created this migration's objects (e.g. the deploys table) while
			// user_version lagged behind — a crash between exec and commit, or
			// an intermediate image that shipped the schema early. Instead of
			// fatal-looping on "already exists", skip past the migration once
			// we've confirmed its objects really are present.
			if isDuplicateSchemaErr(err) && migrationObjectsExist(db, i) {
				if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- types ----

type LogRow struct {
	ID          int64  `json:"id"`
	TS          int64  `json:"-"` // unix nanoseconds
	TSMs        int64  `json:"ts"`
	ContainerID string `json:"container_id"`
	Container   string `json:"container"`
	App         string `json:"app"`
	HostID      string `json:"host_id,omitempty"` // agent host that collected the row (Phase 4.4)
	Stream      string `json:"stream"`            // "o" | "e"
	Level       string `json:"level"`
	Line        string `json:"line"`
}

type MetricPoint struct {
	TS        int64   `json:"ts"`                  // unix ms
	HostID    string  `json:"host_id,omitempty"`   // agent host that collected the sample (Phase 4.4)
	Container string  `json:"container,omitempty"` // container id the sample came from
	App       string  `json:"app,omitempty"`       // app the sample came from
	CPU       float64 `json:"cpu"`
	Mem       int64   `json:"mem"`
	MemLimit  int64   `json:"mem_limit"`
	NetRx     int64   `json:"net_rx"`
	NetTx     int64   `json:"net_tx"`
	BlkR      int64   `json:"blk_r"`
	BlkW      int64   `json:"blk_w"`
	Pids      int64   `json:"pids"`
}

type ContainerRow struct {
	ID, Name, App, Image, Replica, State string
	HostID                               string // agent host that runs the container (Phase 4.4)
	FirstSeen, LastSeen                  int64
	GitCommit                            string // git commit SHA from Docker labels (org.opencontainers.image.revision)
}

type Snapshot struct {
	ID             int64           `json:"id"`
	TS             int64           `json:"ts"`
	HostID         string          `json:"host_id,omitempty"` // agent host the container ran on (Phase 4.4)
	DieTS          int64           `json:"die_ts"`            // death time from the Docker event (0 when unknown)
	Kind           string          `json:"kind"`
	ContainerID    string          `json:"container_id"`
	Container      string          `json:"container"`
	App            string          `json:"app"`
	Image          string          `json:"image"`
	ExitCode       int             `json:"exit_code"`
	OOMKilled      bool            `json:"oom_killed"`
	Error          string          `json:"error"`
	RestartCount   int             `json:"restart_count"`
	RestartsWindow int             `json:"restarts_window"` // deaths of this container inside the loop window
	MemLimit       int64           `json:"mem_limit"`
	UptimeSec      int64           `json:"uptime_sec"`
	Summary        string          `json:"summary"`
	Logs           []LogRow        `json:"logs,omitempty"`
	Metrics        []MetricPoint   `json:"metrics,omitempty"`
	Inspect        json.RawMessage `json:"inspect,omitempty"`
	Deploy         *Deploy         `json:"deploy,omitempty"`     // newest deploy before the crash (V9, API only, never stored)
	GitCommit      string          `json:"git_commit,omitempty"` // git commit SHA from Docker image labels
}

type AppSummary struct {
	App        string `json:"app"`
	Containers int    `json:"containers"`
	LastSeen   int64  `json:"last_seen"`
	Errors1h   int64  `json:"errors_1h"`
	Warns1h    int64  `json:"warns_1h"`
}

// AppAnomaly is one anomalous signal for an app: log volume or error rate
// deviating significantly from the 24-hour baseline. Score is a 0-100
// confidence estimate; Reason is a human-readable explanation.
type AppAnomaly struct {
	App      string `json:"app"`
	Kind     string `json:"kind"`     // "volume" or "errors"
	Score    int    `json:"score"`    // 0-100
	Current  int64  `json:"current"`  // current 1h count
	Baseline int64  `json:"baseline"` // baseline per-hour count (24h avg)
	Reason   string `json:"reason"`   // human-readable explanation
}

// LogCluster is one semantic log cluster: a group of log lines that share
// the same template (variable parts replaced with placeholders).
type LogCluster struct {
	TemplateHash string `json:"template_hash"` // stable hash of the template
	Template     string `json:"template"`      // the normalized template string
	SampleLine   string `json:"sample_line"`   // one example line from the cluster
	Count        int64  `json:"count"`         // total occurrences
	FirstSeen    int64  `json:"first_seen"`    // unix ms
	LastSeen     int64  `json:"last_seen"`     // unix ms
	App          string `json:"app"`           // app name (from containers table)
	Level        string `json:"level"`         // log level
}

// AppCost is one app's resource cost estimate for a time window.
// Cost is computed from metric samples: CPU core-hours and memory GB-hours,
// multiplied by configurable rates.
type AppCost struct {
	App          string  `json:"app"`
	CPUCoreHours float64 `json:"cpu_core_hours"` // sum of CPU cores used * hours
	MemGBHours   float64 `json:"mem_gb_hours"`   // sum of memory GB * hours
	CostCPU      float64 `json:"cost_cpu"`       // CPUCoreHours * cpuRate
	CostMem      float64 `json:"cost_mem"`       // MemGBHours * memRate
	CostTotal    float64 `json:"cost_total"`     // CostCPU + CostMem
	// Right-sizing recommendations
	CPUPercent         float64 `json:"cpu_percent"`         // avg CPU usage as % of one core
	MemPercent         float64 `json:"mem_percent"`         // avg mem as % of mem_limit (0 if unlimited)
	CPUOverProvisioned bool    `json:"cpu_overprovisioned"` // avg CPU < 30%
	MemOverProvisioned bool    `json:"mem_overprovisioned"` // avg mem < 50% of limit
	// Carbon footprint estimate (kg CO2)
	CarbonKg        float64 `json:"carbon_kg"` // estimated CO2 kg for the window
	CarbonKgPerHour float64 `json:"carbon_kg_per_hour"`
}

// AppCosts computes cost estimates for all apps over a time window.
// cpuRate is $/core-hour, memRate is $/GB-hour.
// Uses metric samples from metrics table; averages per container per bucket,
// then sums across containers per app.
func (s *Store) AppCosts(ctx context.Context, hostID string, sinceMs, untilMs int64, cpuRate, memRate float64) ([]AppCost, error) {
	if sinceMs <= 0 {
		sinceMs = time.Now().Add(-24 * time.Hour).UnixMilli()
	}
	if untilMs <= 0 {
		untilMs = time.Now().UnixMilli()
	}

	var cl []string
	var args []any
	if hostID != "" {
		cl, args = append(cl, "host_id = ?"), append(args, hostID)
	}
	cl, args = append(cl, "ts >= ?"), append(args, sinceMs)
	cl, args = append(cl, "ts <= ?"), append(args, untilMs)

	q := `SELECT app, AVG(cpu), AVG(mem), AVG(mem_limit) FROM metrics WHERE ` + whereClause(cl) + ` GROUP BY app`

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type appStats struct {
		app         string
		avgCPU      float64
		avgMem      float64
		avgMemLimit float64
	}
	stats := map[string]appStats{}
	for rows.Next() {
		var app string
		var avgCPU, avgMem, avgMemLimit float64
		if err := rows.Scan(&app, &avgCPU, &avgMem, &avgMemLimit); err != nil {
			return nil, err
		}
		stats[app] = appStats{app: app, avgCPU: avgCPU, avgMem: avgMem, avgMemLimit: avgMemLimit}
	}
	rows.Close()

	durationHours := float64(untilMs-sinceMs) / 3_600_000.0

	var out []AppCost
	for app, st := range stats {
		cpuCoreHours := st.avgCPU / 100.0 * durationHours           // cpu is percent of one core
		memGBHours := (st.avgMem / 1_073_741_824.0) * durationHours // mem is bytes
		costCPU := cpuCoreHours * cpuRate
		costMem := memGBHours * memRate

		cpuPercent := st.avgCPU // already percent of one core
		memPercent := 0.0
		if st.avgMemLimit > 0 {
			memPercent = st.avgMem / st.avgMemLimit * 100.0
		}

		// Carbon estimate: ~0.5 kg CO2 per kWh, ~0.5 kWh per core-hour, ~0.1 kWh per GB-hour
		cpuCarbonKg := cpuCoreHours * 0.5 * 0.5
		memCarbonKg := memGBHours * 0.1 * 0.5
		carbonKg := cpuCarbonKg + memCarbonKg
		carbonKgPerHour := carbonKg / durationHours

		out = append(out, AppCost{
			App:                app,
			CPUCoreHours:       cpuCoreHours,
			MemGBHours:         memGBHours,
			CostCPU:            costCPU,
			CostMem:            costMem,
			CostTotal:          costCPU + costMem,
			CPUPercent:         cpuPercent,
			MemPercent:         memPercent,
			CPUOverProvisioned: cpuPercent < 30.0,
			MemOverProvisioned: memPercent > 0 && memPercent < 50.0,
			CarbonKg:           carbonKg,
			CarbonKgPerHour:    carbonKgPerHour,
		})
	}

	// Sort by cost descending.
	sort.Slice(out, func(i, j int) bool {
		return out[i].CostTotal > out[j].CostTotal
	})
	return out, nil
}

// RecordLogCluster upserts a log cluster entry: if the template_hash already
// exists, it increments count and updates last_seen; otherwise it inserts a
// new row. Returns the cluster id.
func (s *Store) RecordLogCluster(ctx context.Context, hash, template, sampleLine, app, level string, tsMs int64) (int64, error) {
	// Try to insert first; if the hash exists, update instead.
	res, err := s.w.ExecContext(ctx, `
		INSERT INTO log_clusters(template_hash, template, sample_line, count, first_seen, last_seen, app, level)
		VALUES(?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(template_hash) DO UPDATE SET
			count = count + 1,
			last_seen = ?,
			sample_line = CASE WHEN log_clusters.last_seen < ? THEN ? ELSE log_clusters.sample_line END`,
		hash, template, sampleLine, tsMs, tsMs, app, level,
		tsMs, tsMs, sampleLine,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListLogClusters returns log clusters ordered by count (descending), with
// optional app filter and time window.
func (s *Store) ListLogClusters(ctx context.Context, hostID, appFilter string, sinceMs, untilMs int64, limit int) ([]LogCluster, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var cl []string
	var args []any
	if hostID != "" {
		cl, args = append(cl, "host_id = ?"), append(args, hostID)
	}
	if appFilter != "" {
		cl, args = append(cl, "app = ?"), append(args, appFilter)
	}
	if sinceMs > 0 {
		cl, args = append(cl, "last_seen >= ?"), append(args, sinceMs)
	}
	if untilMs > 0 {
		cl, args = append(cl, "last_seen <= ?"), append(args, untilMs)
	}

	q := `SELECT template_hash, template, sample_line, count, first_seen, last_seen, app, level
	      FROM log_clusters` + whereClause(cl) + ` ORDER BY count DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogCluster
	for rows.Next() {
		var c LogCluster
		if err := rows.Scan(&c.TemplateHash, &c.Template, &c.SampleLine, &c.Count, &c.FirstSeen, &c.LastSeen, &c.App, &c.Level); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// BuildClusters scans recent log rows that haven't been clustered yet and
// creates/updates log_cluster entries using semantic template fingerprinting.
// This is called periodically from a background goroutine to avoid the
// single-writer deadlock that would occur if called inside InsertLogs.
func (s *Store) BuildClusters(ctx context.Context, hostID string, sinceMs, untilMs int64, batchSize int) (int, error) {
	if sinceMs <= 0 {
		sinceMs = time.Now().Add(-24 * time.Hour).UnixMilli()
	}
	if untilMs <= 0 {
		untilMs = time.Now().UnixMilli()
	}
	if batchSize <= 0 {
		batchSize = 500
	}

	// Find log rows whose template_hash hasn't been recorded yet.
	// We match on template_hash not being in log_clusters, or last_seen being older
	// than the row's ts (meaning the row is newer than the cluster record).
	var cl []string
	var args []any
	if hostID != "" {
		cl, args = append(cl, "l.host_id = ?"), append(args, hostID)
	}
	cl, args = append(cl, "l.ts >= ?"), append(args, sinceMs)
	cl, args = append(cl, "l.ts <= ?"), append(args, untilMs)

	q := `SELECT l.id, l.ts, l.container_id, l.container, l.app, l.host_id, l.stream, l.level, l.line
	      FROM logs l` + whereClause(cl) + ` ORDER BY l.ts LIMIT ?`
	args = append(args, batchSize)

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var batch []struct {
		id          int64
		ts          int64
		containerID string
		container   string
		app         string
		hostID      string
		stream      string
		level       string
		line        string
	}
	for rows.Next() {
		var r struct {
			id          int64
			ts          int64
			containerID string
			container   string
			app         string
			hostID      string
			stream      string
			level       string
			line        string
		}
		if err := rows.Scan(&r.id, &r.ts, &r.containerID, &r.container, &r.app, &r.hostID, &r.stream, &r.level, &r.line); err != nil {
			return 0, err
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(batch) == 0 {
		return 0, nil
	}

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	count := 0
	for _, r := range batch {
		tmpl := logproc.Template(r.line)
		hash := logproc.TemplateHash(r.line)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO log_clusters(template_hash, template, sample_line, count, first_seen, last_seen, app, level)
			VALUES(?, ?, ?, 1, ?, ?, ?, ?)
			ON CONFLICT(template_hash) DO UPDATE SET
				count = count + 1,
				last_seen = ?,
				sample_line = CASE WHEN log_clusters.last_seen < ? THEN ? ELSE log_clusters.sample_line END`,
			hash, tmpl, r.line, r.ts/1_000_000, r.ts/1_000_000, r.app, r.level,
			r.ts/1_000_000, r.ts/1_000_000, r.line,
		)
		if err != nil {
			return count, err
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

// BuildChain scans recent log rows not yet covered by chain entries and
// appends chain entries covering them. Called periodically from a background
// goroutine to extend the tamper-evident hash chain.
func (s *Store) BuildChain(ctx context.Context, hostID string, sinceMs, untilMs int64, batchSize int) (int64, error) {
	if sinceMs <= 0 {
		sinceMs = time.Now().Add(-24 * time.Hour).UnixMilli()
	}
	if untilMs <= 0 {
		untilMs = time.Now().UnixMilli()
	}
	if batchSize <= 0 {
		batchSize = 1000
	}

	// Get the head entry's hash (prev_hash for the next entry)
	head, err := s.ChainHead(ctx)
	var prevHash string
	if err == nil {
		prevHash = head.EntryHash
	} else {
		prevHash = "" // empty string for genesis entry
	}

	// Find log rows not yet covered by any chain entry.
	// We use the maximum ts covered by chain entries as a lower bound,
	// but simpler: just take rows in the window and chain them all.
	// (Re-chaining is idempotent-ish; duplicate entries would be wrong,
	// so we track by max ts already covered.)
	var cl []string
	var args []any
	if hostID != "" {
		cl, args = append(cl, "l.host_id = ?"), append(args, hostID)
	}
	cl, args = append(cl, "l.ts >= ?"), append(args, sinceMs)
	cl, args = append(cl, "l.ts <= ?"), append(args, untilMs)

	// Get the max ts already covered by chain entries to avoid re-chaining
	var maxCoveredTs int64
	err = s.r.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts_ms), 0) FROM chain`).Scan(&maxCoveredTs)
	if err != nil {
		maxCoveredTs = 0
	}
	if maxCoveredTs > sinceMs {
		cl = append(cl, "l.ts > ?")
		args = append(args, maxCoveredTs)
	}

	q := `SELECT l.id, l.ts, l.container_id, l.container, l.app, l.host_id, l.stream, l.level, l.line
	      FROM logs l` + whereClause(cl) + ` ORDER BY l.ts LIMIT ?`
	args = append(args, batchSize)

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var batch []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.TS, &r.ContainerID, &r.Container, &r.App, &r.HostID, &r.Stream, &r.Level, &r.Line); err != nil {
			return 0, err
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(batch) == 0 {
		return 0, nil
	}

	// Compute data_hash = SHA256 of concatenated row data
	var buf bytes.Buffer
	for _, r := range batch {
		fmt.Fprintf(&buf, "%d:%d:%s:%s:%s:%s:%s:%s:%s\n",
			r.ID, r.TS, r.ContainerID, r.Container, r.App, r.HostID, r.Stream, r.Level, r.Line)
	}
	dataHash := sha256Sum(buf.Bytes())
	lastTs := batch[len(batch)-1].TS / 1_000_000

	entryHash, err := s.AppendChain(ctx, prevHash, dataHash, int64(len(batch)), lastTs)
	if err != nil {
		return 0, err
	}
	_ = entryHash
	return int64(len(batch)), nil
}

// ChainEntry is one link in the tamper-evident hash chain over log rows.
type ChainEntry struct {
	ID        int64  `json:"id"`
	Seq       int64  `json:"seq"`
	PrevHash  string `json:"prev_hash"`
	EntryHash string `json:"entry_hash"`
	DataHash  string `json:"data_hash"`
	RowCount  int64  `json:"row_count"`
	TsMs      int64  `json:"ts_ms"`
}

// ListChainEntries returns the most recent chain entries, ordered by seq DESC.
func (s *Store) ListChainEntries(ctx context.Context, limit int) ([]ChainEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT id, seq, prev_hash, entry_hash, data_hash, row_count, ts_ms
		FROM chain ORDER BY seq DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChainEntry
	for rows.Next() {
		var ce ChainEntry
		if err := rows.Scan(&ce.ID, &ce.Seq, &ce.PrevHash, &ce.EntryHash, &ce.DataHash, &ce.RowCount, &ce.TsMs); err != nil {
			return nil, err
		}
		out = append(out, ce)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ChainVerifyResult is the result of verifying the entire evidence chain.
type ChainVerifyResult struct {
	Valid    bool   `json:"valid"`
	Entries  int64  `json:"entries"`
	HeadHash string `json:"head_hash"`
	Error    string `json:"error,omitempty"` // first error encountered, if any
}

// AppendChain appends a new chain entry covering a batch of log rows.
// dataHash is SHA256 of the concatenated row data for this batch.
// Returns the new entry hash.
func (s *Store) AppendChain(ctx context.Context, prevHash, dataHash string, rowCount int64, tsMs int64) (string, error) {
	entryHash := sha256Sum([]byte(prevHash + dataHash))

	res, err := s.w.ExecContext(ctx, `
		INSERT INTO chain(seq, prev_hash, entry_hash, data_hash, row_count, ts_ms)
		VALUES(
			COALESCE((SELECT MAX(seq) FROM chain) + 1, 1),
			?, ?, ?, ?, ?
		)`,
		prevHash, entryHash, dataHash, rowCount, tsMs,
	)
	if err != nil {
		return "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	_ = id
	return entryHash, nil
}

// ChainHead returns the head (latest) chain entry, or zero values if empty.
func (s *Store) ChainHead(ctx context.Context) (ChainEntry, error) {
	var ce ChainEntry
	err := s.r.QueryRowContext(ctx, `
		SELECT id, seq, prev_hash, entry_hash, data_hash, row_count, ts_ms
		FROM chain ORDER BY seq DESC LIMIT 1
	`).Scan(&ce.ID, &ce.Seq, &ce.PrevHash, &ce.EntryHash, &ce.DataHash, &ce.RowCount, &ce.TsMs)
	if err != nil {
		return ce, err
	}
	return ce, nil
}

// VerifyChain walks the entire chain and verifies that each entry's hash
// matches SHA256(prev_hash + data_hash) and that the chain is contiguous
// (each entry's prev_hash matches the previous entry's entry_hash).
func (s *Store) VerifyChain(ctx context.Context) (ChainVerifyResult, error) {
	result := ChainVerifyResult{Valid: true}

	rows, err := s.r.QueryContext(ctx, `
		SELECT id, seq, prev_hash, entry_hash, data_hash, row_count, ts_ms
		FROM chain ORDER BY seq ASC
	`)
	if err != nil {
		result.Valid = false
		result.Error = err.Error()
		return result, err
	}
	defer rows.Close()

	var prevEntryHash string
	var count int64
	for rows.Next() {
		var ce ChainEntry
		if err := rows.Scan(&ce.ID, &ce.Seq, &ce.PrevHash, &ce.EntryHash, &ce.DataHash, &ce.RowCount, &ce.TsMs); err != nil {
			result.Valid = false
			result.Error = err.Error()
			return result, err
		}

		// Verify chain continuity
		if count > 0 && ce.PrevHash != prevEntryHash {
			result.Valid = false
			result.Error = fmt.Sprintf("chain break at seq %d: prev_hash %s != previous entry_hash %s", ce.Seq, ce.PrevHash, prevEntryHash)
			return result, nil
		}

		// Verify entry hash: SHA256(prev_hash + data_hash)
		expectedHash := sha256Sum([]byte(ce.PrevHash + ce.DataHash))
		if ce.EntryHash != expectedHash {
			result.Valid = false
			result.Error = fmt.Sprintf("hash mismatch at seq %d: entry_hash %s != expected %s", ce.Seq, ce.EntryHash, expectedHash)
			return result, nil
		}

		prevEntryHash = ce.EntryHash
		count++
	}
	if err := rows.Err(); err != nil {
		result.Valid = false
		result.Error = err.Error()
		return result, err
	}

	result.Entries = count
	if count > 0 {
		// Head hash is the last entry's entry_hash
		result.HeadHash = prevEntryHash
	}
	return result, nil
}

// sha256Sum returns the hex SHA256 of the input.
func sha256Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// AppAnomalies detects apps whose log volume or error rate deviates
// significantly from their 24-hour baseline. Returns anomalies sorted by
// score (highest first). threshold is the multiplier above which a deviation
// is flagged (2.0 = current must be 2x baseline).
func (s *Store) AppAnomalies(ctx context.Context, hostID string, threshold float64) ([]AppAnomaly, error) {
	if threshold <= 0 {
		threshold = 2.0
	}
	now := time.Now()
	// Baselines: per-hour averages over the last 24 hours.
	baselineStart := now.Add(-24 * time.Hour).UnixMilli()
	currentStart := now.Add(-time.Hour).UnixMilli()

	var cl []string
	var args []any
	if hostID != "" {
		cl, args = append(cl, "host_id = ?"), append(args, hostID)
	}

	// Current 1h counts per app.
	type counts struct {
		app   string
		total int64
		errs  int64
	}
	current := map[string]counts{}

	// Total lines per app in the last hour.
	qTotal := `SELECT app, COUNT(*) FROM logs_view WHERE ts >= ?` + whereClause(cl) + ` GROUP BY app`
	argsTotal := append([]any{currentStart}, args...)
	rows, err := s.r.QueryContext(ctx, qTotal, argsTotal...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var app string
		var total int64
		if err := rows.Scan(&app, &total); err != nil {
			return nil, err
		}
		current[app] = counts{app: app, total: total}
	}
	rows.Close()

	// Error lines per app in the last hour.
	qErrs := `SELECT app, COUNT(*) FROM logs_view WHERE ts >= ? AND level='error'` + whereClause(cl) + ` GROUP BY app`
	argsErrs := append([]any{currentStart}, args...)
	r2, err := s.r.QueryContext(ctx, qErrs, argsErrs...)
	if err != nil {
		return nil, err
	}
	defer r2.Close()
	for r2.Next() {
		var app string
		var errs int64
		if err := r2.Scan(&app, &errs); err != nil {
			return nil, err
		}
		if c, ok := current[app]; ok {
			c.errs = errs
			current[app] = c
		} else {
			current[app] = counts{app: app, errs: errs}
		}
	}
	r2.Close()

	// Baseline: per-hour averages over the last 24h.
	qBaseTotal := `SELECT app, COUNT(*) / 24.0 FROM logs_view WHERE ts >= ? AND ts < ?` + whereClause(cl) + ` GROUP BY app`
	argsBaseTotal := append([]any{baselineStart, currentStart}, args...)
	r3, err := s.r.QueryContext(ctx, qBaseTotal, argsBaseTotal...)
	if err != nil {
		return nil, err
	}
	defer r3.Close()
	baselineTotal := map[string]float64{}
	for r3.Next() {
		var app string
		var avg float64
		if err := r3.Scan(&app, &avg); err != nil {
			return nil, err
		}
		baselineTotal[app] = avg
	}
	r3.Close()

	qBaseErrs := `SELECT app, COUNT(*) / 24.0 FROM logs_view WHERE ts >= ? AND ts < ? AND level='error'` + whereClause(cl) + ` GROUP BY app`
	argsBaseErrs := append([]any{baselineStart, currentStart}, args...)
	r4, err := s.r.QueryContext(ctx, qBaseErrs, argsBaseErrs...)
	if err != nil {
		return nil, err
	}
	defer r4.Close()
	baselineErrs := map[string]float64{}
	for r4.Next() {
		var app string
		var avg float64
		if err := r4.Scan(&app, &avg); err != nil {
			return nil, err
		}
		baselineErrs[app] = avg
	}
	r4.Close()

	var out []AppAnomaly
	for app, c := range current {
		// Volume anomaly
		bt := baselineTotal[app]
		if bt > 0 && float64(c.total)/bt >= threshold {
			score := int(math.Min(100, float64(c.total)/bt*20))
			out = append(out, AppAnomaly{
				App:      app,
				Kind:     "volume",
				Score:    score,
				Current:  c.total,
				Baseline: int64(bt + 0.5),
				Reason:   fmt.Sprintf("log volume %dx above 24h baseline", int64(float64(c.total)/bt+0.5)),
			})
		}
		// Error anomaly
		be := baselineErrs[app]
		if be > 0 && float64(c.errs)/be >= threshold {
			score := int(math.Min(100, float64(c.errs)/be*20))
			out = append(out, AppAnomaly{
				App:      app,
				Kind:     "errors",
				Score:    score,
				Current:  c.errs,
				Baseline: int64(be + 0.5),
				Reason:   fmt.Sprintf("error rate %dx above 24h baseline", int64(float64(c.errs)/be+0.5)),
			})
		}
	}

	// Sort by score descending.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out, nil
}

// User is one multi-user account (Phase 3.3). Role is "admin" or "viewer".
type User struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	PasswordHash string `json:"-"`    // never sent to the client
	Role         string `json:"role"` // "admin" or "viewer"
	CreatedMs    int64  `json:"created_ms"`
}

// UserAppScope is an application assigned to a viewer account.
type UserAppScope struct {
	UserID int64  `json:"user_id"`
	App    string `json:"app"`
}

// AccessRow is one parsed access-log request, keyed by the container that
// emitted it (App is the reverse proxy's app, not the route's target).
type AccessRow struct {
	TS          int64  `json:"ts"` // unix ms
	ContainerID string `json:"container_id"`
	Container   string `json:"container"`
	App         string `json:"app"`
	IP          string `json:"ip"`
	Country     string `json:"country,omitempty"` // GeoIP country code (GEOIP_DB), empty when disabled
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	Bytes       int64  `json:"bytes"`
	Referer     string `json:"referer,omitempty"`
	UA          string `json:"ua,omitempty"`
	User        string `json:"user,omitempty"` // authenticated user who made the request (Phase 3.3)
}

// ---- logs ----

// InsertLogs writes a batch in one transaction and fills in each row's ID.
func (s *Store) InsertLogs(rows []LogRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	if s.normalized.Load() {
		stmt, err := tx.Prepare(`INSERT INTO logs(ts,container_id,host_id,stream,level,line) VALUES(?,?,?,?,?,?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		defer stmt.Close()
		for i := range rows {
			r := &rows[i]
			res, err := stmt.Exec(r.TS, r.ContainerID, r.HostID, r.Stream, r.Level, r.Line)
			if err != nil {
				tx.Rollback()
				return err
			}
			r.ID, _ = res.LastInsertId()
		}
	} else {
		stmt, err := tx.Prepare(`INSERT INTO logs(ts,container_id,container,app,host_id,stream,level,line) VALUES(?,?,?,?,?,?,?,?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		defer stmt.Close()
		for i := range rows {
			r := &rows[i]
			res, err := stmt.Exec(r.TS, r.ContainerID, r.Container, r.App, r.HostID, r.Stream, r.Level, r.Line)
			if err != nil {
				tx.Rollback()
				return err
			}
			r.ID, _ = res.LastInsertId()
		}
	}
	return tx.Commit()
}

type LogQuery struct {
	App, Container, Stream, Q string
	Levels                    []string
	AllowedApps               []string       // optional server-side viewer scope
	Regex                     *regexp.Regexp // when set, Q is ignored and matching happens in Go
	SinceMs, UntilMs          int64
	AfterID, BeforeID         int64
	// Asc orders results by id ascending (used for "context after this line").
	Asc   bool
	Limit int
	// HostID filters rows to one agent host (empty = all hosts).
	HostID string
}

const maxRegexScan = 200000

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (q LogQuery) where() (string, []any) {
	return q.whereOpts(true)
}

// whereOpts builds the filter clauses. withQ controls the text-search clause:
// false when the caller supplies its own (FTS MATCH) instead of LIKE.
func (q LogQuery) whereOpts(withQ bool) (string, []any) {
	var cl []string
	var args []any
	if q.App != "" {
		cl, args = append(cl, "app = ?"), append(args, q.App)
	}
	if len(q.AllowedApps) > 0 {
		cl = append(cl, "app IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.AllowedApps)), ",")+")")
		for _, app := range q.AllowedApps {
			args = append(args, app)
		}
	}
	if q.HostID != "" {
		cl, args = append(cl, "host_id = ?"), append(args, q.HostID)
	}
	if q.Container != "" {
		cl = append(cl, "(container = ? OR container_id LIKE ?)")
		args = append(args, q.Container, q.Container+"%")
	}
	if q.Stream != "" {
		cl, args = append(cl, "stream = ?"), append(args, q.Stream)
	}
	if len(q.Levels) > 0 {
		cl = append(cl, "level IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Levels)), ",")+")")
		for _, l := range q.Levels {
			args = append(args, l)
		}
	}
	if withQ && q.Q != "" && q.Regex == nil {
		cl, args = append(cl, `line LIKE ? ESCAPE '\'`), append(args, "%"+escapeLike(q.Q)+"%")
	}
	if q.SinceMs > 0 {
		cl, args = append(cl, "ts >= ?"), append(args, q.SinceMs*1_000_000)
	}
	if q.UntilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, q.UntilMs*1_000_000)
	}
	if q.AfterID > 0 {
		cl, args = append(cl, "id > ?"), append(args, q.AfterID)
	}
	if q.BeforeID > 0 {
		cl, args = append(cl, "id < ?"), append(args, q.BeforeID)
	}
	if len(cl) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(cl, " AND "), args
}

// Match applies the same filters in Go, for live streaming.
func (q LogQuery) Match(r LogRow) bool {
	if q.App != "" && r.App != q.App {
		return false
	}
	if len(q.AllowedApps) > 0 {
		ok := false
		for _, app := range q.AllowedApps {
			if r.App == app {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q.HostID != "" && r.HostID != q.HostID {
		return false
	}
	if q.Container != "" && r.Container != q.Container && !strings.HasPrefix(r.ContainerID, q.Container) {
		return false
	}
	// Live-tail resume: an SSE subscriber that reconnects with since_id (or the
	// browser's Last-Event-ID header) only wants rows it has not seen yet.
	if q.AfterID > 0 && r.ID <= q.AfterID {
		return false
	}
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if len(q.Levels) > 0 {
		ok := false
		for _, l := range q.Levels {
			if l == r.Level {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q.Regex != nil {
		return q.Regex.MatchString(r.Line)
	}
	if q.Q != "" && !strings.Contains(strings.ToLower(r.Line), strings.ToLower(q.Q)) {
		return false
	}
	return true
}

// ftsQuery converts free text into a safe FTS5 MATCH expression: each
// whitespace-separated token is quoted (embedded quotes doubled) and
// prefix-matched, so as-you-type queries like "dat" find "database".
func ftsQuery(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, `""`)
		quoted = append(quoted, `"`+f+`"*`)
	}
	return strings.Join(quoted, " AND ")
}

// QueryLogs returns rows oldest-first. Without AfterID it returns the newest Limit rows.
// Text search uses the FTS5 index as a fast path and falls back to LIKE when the
// index finds nothing (so mid-word substring searches still work).
func (s *Store) QueryLogs(ctx context.Context, q LogQuery) ([]LogRow, error) {
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500
	}
	if q.Q != "" && q.Regex == nil {
		if rows, err := s.queryLogsFTS(ctx, q); err == nil && len(rows) > 0 {
			return rows, nil
		}
	}
	where, args := q.where()
	order := "DESC"
	if q.AfterID > 0 || q.Asc {
		order = "ASC"
	}
	query := "SELECT id, ts, container_id, container, app, host_id, stream, level, line FROM logs_view" + where + " ORDER BY id " + order
	if q.Regex == nil {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LogRow, 0, 64)
	scanned := 0
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.TS, &r.ContainerID, &r.Container, &r.App, &r.HostID, &r.Stream, &r.Level, &r.Line); err != nil {
			return nil, err
		}
		r.TSMs = r.TS / 1_000_000
		if q.Regex != nil {
			if scanned++; scanned > maxRegexScan {
				break
			}
			if !q.Regex.MatchString(r.Line) {
				continue
			}
		}
		out = append(out, r)
		if len(out) >= q.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if order == "DESC" {
		flipAsc(out)
	}
	return out, nil
}

// queryLogsFTS searches via the FTS5 index. Returns nil, nil when the query
// has no usable tokens.
func (s *Store) queryLogsFTS(ctx context.Context, q LogQuery) ([]LogRow, error) {
	match := ftsQuery(q.Q)
	if match == "" {
		return nil, nil
	}
	where, args := q.whereOpts(false)
	if where == "" {
		where = " WHERE "
	} else {
		where += " AND "
	}
	where += "id IN (SELECT rowid FROM logs_fts WHERE logs_fts MATCH ?)"
	args = append(args, match)
	order := "DESC"
	if q.AfterID > 0 {
		order = "ASC"
	}
	query := "SELECT id, ts, container_id, container, app, host_id, stream, level, line FROM logs_view" + where + " ORDER BY id " + order + " LIMIT ?"
	args = append(args, q.Limit)
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LogRow, 0, 64)
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.TS, &r.ContainerID, &r.Container, &r.App, &r.HostID, &r.Stream, &r.Level, &r.Line); err != nil {
			return nil, err
		}
		r.TSMs = r.TS / 1_000_000
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if order == "DESC" {
		flipAsc(out)
	}
	return out, nil
}

func flipAsc(out []LogRow) {
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
}

// ---- metrics ----

func (s *Store) InsertMetric(containerID, app string, p MetricPoint) error {
	_, err := s.w.Exec(`INSERT INTO metrics(ts,container_id,app,host_id,cpu,mem,mem_limit,net_rx,net_tx,blk_r,blk_w,pids) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.TS, containerID, app, p.HostID, p.CPU, p.Mem, p.MemLimit, p.NetRx, p.NetTx, p.BlkR, p.BlkW, p.Pids)
	return err
}

// QueryMetrics buckets samples. For an app it averages per container inside each bucket,
// then sums across containers, so the chart shows total app usage.
func (s *Store) QueryMetrics(ctx context.Context, app, containerID, hostID string, sinceMs, untilMs, bucketMs int64) ([]MetricPoint, error) {
	if bucketMs < 1000 {
		bucketMs = 1000
	}
	var cl []string
	var args []any
	args = append(args, bucketMs, bucketMs)
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	if hostID != "" {
		cl, args = append(cl, "host_id = ?"), append(args, hostID)
	}
	if containerID != "" {
		cl, args = append(cl, "container_id LIKE ?"), append(args, containerID+"%")
	}
	cl, args = append(cl, "ts >= ?"), append(args, sinceMs)
	if untilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, untilMs)
	}
	q := `SELECT b, AVG(cpu), SUM(mem), SUM(lim), SUM(rx), SUM(tx) FROM (
	  SELECT (ts/?)*? AS b, container_id, AVG(cpu) AS cpu, MAX(mem) AS mem, MAX(mem_limit) AS lim, MAX(net_rx) AS rx, MAX(net_tx) AS tx
	  FROM metrics WHERE ` + strings.Join(cl, " AND ") + ` GROUP BY b, container_id) GROUP BY b ORDER BY b`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricPoint{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.TS, &p.CPU, &p.Mem, &p.MemLimit, &p.NetRx, &p.NetTx); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- containers and apps ----

func (s *Store) UpsertContainer(c ContainerRow) error {
	_, err := s.w.Exec(`INSERT INTO containers(id,name,app,image,replica,host_id,state,first_seen,last_seen,git_commit) VALUES(?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(id) DO UPDATE SET state=excluded.state, last_seen=excluded.last_seen, name=excluded.name, app=excluded.app, image=excluded.image, host_id=excluded.host_id, git_commit=excluded.git_commit`,
		c.ID, c.Name, c.App, c.Image, c.Replica, c.HostID, c.State, c.FirstSeen, c.LastSeen, c.GitCommit)
	return err
}

func (s *Store) MarkContainerState(id, state string) error {
	_, err := s.w.Exec(`UPDATE containers SET state=?, last_seen=? WHERE id=?`, state, time.Now().UnixMilli(), id)
	return err
}

// StateGet returns the integer stored under key, or 0 when unset. It holds
// small pipeline checkpoints such as the Docker event cursor.
func (s *Store) StateGet(ctx context.Context, key string) int64 {
	var v int64
	if err := s.r.QueryRowContext(ctx, `SELECT value FROM state WHERE key=?`, key).Scan(&v); err != nil {
		return 0
	}
	return v
}

func (s *Store) StateSet(ctx context.Context, key string, v int64) error {
	_, err := s.w.Exec(`INSERT INTO state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, v)
	return err
}

// whereClause builds a SQL WHERE fragment from condition strings. Returns ""
// when there are no conditions, otherwise " AND cond1 AND cond2 ...".
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// AppSummaries returns per-app aggregates, optionally filtered to one host.
func (s *Store) AppSummaries(ctx context.Context, hostID string) ([]AppSummary, error) {
	var cl []string
	var args []any
	if hostID != "" {
		cl, args = append(cl, "host_id = ?"), append(args, hostID)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT app, SUM(CASE WHEN state='running' THEN 1 ELSE 0 END), MAX(last_seen) FROM containers`+whereClause(cl)+` GROUP BY app ORDER BY app`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[string]int{}
	var out []AppSummary
	for rows.Next() {
		var a AppSummary
		if err := rows.Scan(&a.App, &a.Containers, &a.LastSeen); err != nil {
			return nil, err
		}
		idx[a.App] = len(out)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	since := time.Now().Add(-time.Hour).UnixNano()
	var lc []string
	var la []any
	if hostID != "" {
		lc, la = append(lc, "host_id = ?"), append(la, hostID)
	}
	r2, err := s.r.QueryContext(ctx, `SELECT app, SUM(CASE WHEN level='error' THEN 1 ELSE 0 END), SUM(CASE WHEN level='warn' THEN 1 ELSE 0 END) FROM logs_view`+whereClause(append([]string{"ts >= ?"}, lc...))+` GROUP BY app`, append([]any{since}, la...)...)
	if err != nil {
		return nil, err
	}
	defer r2.Close()
	for r2.Next() {
		var app string
		var e, w int64
		if err := r2.Scan(&app, &e, &w); err != nil {
			return nil, err
		}
		if i, ok := idx[app]; ok {
			out[i].Errors1h, out[i].Warns1h = e, w
		}
	}
	return out, r2.Err()
}

// Host is one agent host registered with the hub (Phase 4.4).
type Host struct {
	HostID    string            `json:"host_id"`
	Labels    map[string]string `json:"labels,omitempty"`
	FirstSeen int64             `json:"first_seen"`
	LastSeen  int64             `json:"last_seen"`
}

// ListContainers returns known containers, optionally filtered to one host.
func (s *Store) ListContainers(ctx context.Context, hostID string) ([]ContainerRow, error) {
	var q string
	var args []any
	if hostID != "" {
		q = `SELECT id,name,app,image,replica,host_id,state,first_seen,last_seen FROM containers WHERE host_id=? ORDER BY app, name`
		args = append(args, hostID)
	} else {
		q = `SELECT id,name,app,image,replica,host_id,state,first_seen,last_seen FROM containers ORDER BY app, name`
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContainerRow{}
	for rows.Next() {
		var c ContainerRow
		if err := rows.Scan(&c.ID, &c.Name, &c.App, &c.Image, &c.Replica, &c.HostID, &c.State, &c.FirstSeen, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertHost registers or touches an agent host (called on Hello from an agent).
func (s *Store) UpsertHost(hostID string, labels map[string]string) error {
	labelsJSON, _ := json.Marshal(labels)
	now := time.Now().UnixMilli()
	_, err := s.w.Exec(`INSERT INTO hosts(host_id,labels_json,first_seen,last_seen) VALUES(?,?,?,?)
	  ON CONFLICT(host_id) DO UPDATE SET labels_json=excluded.labels_json, last_seen=excluded.last_seen`,
		hostID, string(labelsJSON), now, now)
	return err
}

// ListHosts returns all registered agent hosts, newest first.
func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT host_id,labels_json,first_seen,last_seen FROM hosts ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		var h Host
		var labelsJSON string
		if err := rows.Scan(&h.HostID, &labelsJSON, &h.FirstSeen, &h.LastSeen); err != nil {
			return nil, err
		}
		if labelsJSON != "" {
			_ = json.Unmarshal([]byte(labelsJSON), &h.Labels)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- snapshots ----

func (s *Store) InsertSnapshot(sn Snapshot) (int64, error) {
	logs, _ := json.Marshal(sn.Logs)
	metrics, _ := json.Marshal(sn.Metrics)
	insp := string(sn.Inspect)
	if insp == "" {
		insp = "{}"
	}
	oom := 0
	if sn.OOMKilled {
		oom = 1
	}
	res, err := s.w.Exec(`INSERT INTO snapshots(ts,kind,container_id,container,app,host_id,image,exit_code,oom_killed,error,restart_count,restarts_window,mem_limit,uptime_sec,die_ts,summary,logs_json,metrics_json,inspect_json)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sn.TS, sn.Kind, sn.ContainerID, sn.Container, sn.App, sn.HostID, sn.Image, sn.ExitCode, oom, sn.Error, sn.RestartCount, sn.RestartsWindow, sn.MemLimit, sn.UptimeSec, sn.DieTS, sn.Summary, string(logs), string(metrics), insp)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SnapshotExists reports whether a snapshot for this death was already stored.
// It deduplicates die events replayed from the Docker event buffer after a
// restart (C8). dieMs 0 (older rows, unknown death time) never deduplicates.
func (s *Store) SnapshotExists(ctx context.Context, containerID, hostID string, dieMs int64) (bool, error) {
	if dieMs <= 0 {
		return false, nil
	}
	var one int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM snapshots WHERE container_id=? AND host_id=? AND die_ts=? LIMIT 1`, containerID, hostID, dieMs).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// CountDeathsSince counts stored deaths of one container at or after sinceMs.
// It feeds restart-loop detection (X7); only deaths whose kind is stored as a
// snapshot are visible here, which keeps rollout noise (killed) out by default.
func (s *Store) CountDeathsSince(ctx context.Context, containerID string, sinceMs int64) (int64, error) {
	var n int64
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots WHERE container_id=? AND die_ts>=?`, containerID, sinceMs).Scan(&n)
	return n, err
}

const snapCols = `id,ts,kind,container_id,container,app,host_id,image,exit_code,oom_killed,error,restart_count,restarts_window,mem_limit,uptime_sec,die_ts,summary`

func scanSnap(sc interface{ Scan(...any) error }, extra ...any) (Snapshot, error) {
	var sn Snapshot
	var oom int
	dest := append([]any{&sn.ID, &sn.TS, &sn.Kind, &sn.ContainerID, &sn.Container, &sn.App, &sn.HostID, &sn.Image, &sn.ExitCode, &oom, &sn.Error, &sn.RestartCount, &sn.RestartsWindow, &sn.MemLimit, &sn.UptimeSec, &sn.DieTS, &sn.Summary}, extra...)
	err := sc.Scan(dest...)
	sn.OOMKilled = oom == 1
	return sn, err
}

// ListSnapshots returns light rows (no logs or metrics), newest first.
func (s *Store) ListSnapshots(ctx context.Context, app, hostID string, limit int) ([]Snapshot, error) {
	return s.listSnapshots(ctx, app, hostID, 0, limit)
}

// SnapshotsSince is like ListSnapshots but bounded to ts >= sinceMs, which lets
// the dashboard aggregate a 24h window without scanning the whole table.
func (s *Store) SnapshotsSince(ctx context.Context, sinceMs int64, hostID string) ([]Snapshot, error) {
	return s.listSnapshots(ctx, "", hostID, sinceMs, snapSinceLimit)
}

// snapSinceLimit bounds a windowed snapshot scan. Without it the 24h
// dashboard query has no LIMIT at all (the cap below only applies when no
// window is set) and materializes every snapshot row in the window — a
// restart storm turns that into tens of MB held for the whole request.
const snapSinceLimit = 10_000

func (s *Store) listSnapshots(ctx context.Context, app, hostID string, sinceMs int64, limit int) ([]Snapshot, error) {
	if limit < 0 {
		limit = 0
	}
	// Windowed scans (dashboard) carry their own, wider ceiling; everything
	// else is clamped to a page so a caller cannot ask for the whole table.
	max := 500
	if sinceMs > 0 {
		max = snapSinceLimit
	}
	if limit > max {
		limit = max
	}
	q := `SELECT ` + snapCols + ` FROM snapshots`
	var where []string
	var args []any
	if app != "" {
		where = append(where, `app = ?`)
		args = append(args, app)
	}
	if hostID != "" {
		where = append(where, `host_id = ?`)
		args = append(args, hostID)
	}
	if sinceMs > 0 {
		where = append(where, `ts >= ?`)
		args = append(args, sinceMs)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	if limit == 0 && sinceMs == 0 {
		limit = 100 // unbounded scans still need a cap
	}
	if limit > 0 {
		q += ` ORDER BY ts DESC LIMIT ?`
		args = append(args, limit)
	} else {
		q += ` ORDER BY ts DESC`
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		sn, err := scanSnap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) GetSnapshot(ctx context.Context, id int64) (Snapshot, error) {
	var logs, metrics, insp string
	sn, err := scanSnap(s.r.QueryRowContext(ctx, `SELECT `+snapCols+`,logs_json,metrics_json,inspect_json FROM snapshots WHERE id=?`, id), &logs, &metrics, &insp)
	if err != nil {
		return sn, err
	}
	_ = json.Unmarshal([]byte(logs), &sn.Logs)
	_ = json.Unmarshal([]byte(metrics), &sn.Metrics)
	sn.Inspect = json.RawMessage(insp)
	return sn, nil
}

// ---- access logs ----

// InsertAccess writes a batch of parsed requests in one transaction.
func (s *Store) InsertAccess(rows []AccessRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO access(ts,container_id,container,app,ip,country,method,path,status,bytes,referer,ua,user) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i := range rows {
		r := rows[i]
		if _, err := stmt.Exec(r.TS, r.ContainerID, r.Container, r.App, r.IP, r.Country, r.Method, r.Path, r.Status, r.Bytes, r.Referer, r.UA, r.User); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---- users (Phase 3.3: multi-user) ----

// CreateUser inserts a new user. Returns an error if the name already exists.
func (s *Store) CreateUser(name, passwordHash, role string) (int64, error) {
	if role != "admin" && role != "viewer" {
		role = "viewer"
	}
	res, err := s.w.Exec(`INSERT INTO users(name,password_hash,role,created_ms) VALUES(?,?,?,?)`,
		name, passwordHash, role, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetUserByName returns the user with the given name, or nil if not found.
func (s *Store) GetUserByName(name string) (*User, error) {
	row := s.r.QueryRow(`SELECT id,name,password_hash,role,created_ms FROM users WHERE name=?`, name)
	var u User
	if err := row.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.CreatedMs); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID returns the user with the given id, or nil if not found.
func (s *Store) GetUserByID(id int64) (*User, error) {
	row := s.r.QueryRow(`SELECT id,name,password_hash,role,created_ms FROM users WHERE id=?`, id)
	var u User
	if err := row.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.CreatedMs); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers returns all users.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.r.Query(`SELECT id,name,password_hash,role,created_ms FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserApps returns the explicitly assigned application scopes for a user.
func (s *Store) UserApps(userID int64) ([]string, error) {
	rows, err := s.r.Query(`SELECT app FROM user_apps WHERE user_id=? ORDER BY app`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []string
	for rows.Next() {
		var app string
		if err := rows.Scan(&app); err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// ReplaceUserApps atomically replaces a viewer's application scopes.
func (s *Store) ReplaceUserApps(userID int64, apps []string) error {
	seen := make(map[string]struct{}, len(apps))
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM user_apps WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, app := range apps {
		app = strings.TrimSpace(app)
		if app == "" {
			continue
		}
		if _, ok := seen[app]; ok {
			continue
		}
		seen[app] = struct{}{}
		if _, err := tx.Exec(`INSERT INTO user_apps(user_id,app) VALUES(?,?)`, userID, app); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UserCanAccessApp reports whether a non-admin user has an explicit scope.
func (s *Store) UserCanAccessApp(userID int64, app string) (bool, error) {
	var n int
	err := s.r.QueryRow(`SELECT COUNT(*) FROM user_apps WHERE user_id=? AND app=?`, userID, app).Scan(&n)
	return n > 0, err
}

// SeedFirstUser creates the first user from AUTH_USER/AUTH_PASS if no users
// exist yet. Returns true if a user was created.
func (s *Store) SeedFirstUser(name, passwordHash string) (bool, error) {
	var n int64
	if err := s.r.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	_, err := s.CreateUser(name, passwordHash, "admin")
	return err == nil, err
}

// UpdateUserRole changes a user's role. Refuses to demote the last admin.
// The count and the update run in one transaction (like DeleteUser) to close
// the TOCTOU where two concurrent demotions both observe n==1 and leave zero
// admins. SQLite serializes writers on s.w, so this is safe.
func (s *Store) UpdateUserRole(id int64, role string) error {
	if role != "admin" && role != "viewer" {
		return fmt.Errorf("invalid role: %s", role)
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role == "viewer" {
		var n int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			var currentRole string
			if err := tx.QueryRow(`SELECT role FROM users WHERE id=?`, id).Scan(&currentRole); err != nil {
				return err
			}
			if currentRole == "admin" {
				return fmt.Errorf("cannot demote the last admin")
			}
		}
	}
	if _, err := tx.Exec(`UPDATE users SET role=? WHERE id=?`, role, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUser removes a user. Refuses to delete the last admin.
// Uses a transaction to avoid TOCTOU between counting and deleting.
func (s *Store) DeleteUser(id int64) error {
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	u, err := s.getUserByIDTx(tx, id)
	if err != nil {
		return err
	}
	if u.Role == "admin" {
		var n int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("cannot delete the last admin")
		}
	}
	_, err = tx.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// getUserByIDTx returns the user with the given id within a transaction.
func (s *Store) getUserByIDTx(tx *sql.Tx, id int64) (*User, error) {
	row := tx.QueryRow(`SELECT id,name,password_hash,role,created_ms FROM users WHERE id=?`, id)
	var u User
	if err := row.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.CreatedMs); err != nil {
		return nil, err
	}
	return &u, nil
}

// AccessSummary holds overall counts for one window.
type AccessSummary struct {
	Requests  int64 `json:"requests"`
	ClientErr int64 `json:"client_err"`
	ServerErr int64 `json:"server_err"`
	UniqueIPs int64 `json:"unique_ips"`
	Bytes     int64 `json:"bytes"`
}

// AccessCode is one HTTP status code and how often it appeared.
type AccessCode struct {
	Code int   `json:"code"`
	N    int64 `json:"n"`
}

// AccessTop is a row of a "top N" table (paths, referrers).
type AccessTop struct {
	Key    string `json:"key"`
	Hits   int64  `json:"hits"`
	Bytes  int64  `json:"bytes"`
	Errors int64  `json:"errors"` // 4xx + 5xx
}

// AccessPoint is one timeline bucket: requests, errors and traffic.
type AccessPoint struct {
	TS     int64 `json:"ts"` // unix ms, bucket start
	Hits   int64 `json:"hits"`
	Errors int64 `json:"errors"`
	Bytes  int64 `json:"bytes"`
}

// AccessAggregates bundles everything the Access dashboard tab shows.
type AccessAggregates struct {
	SinceMs  int64         `json:"since_ms"`
	UntilMs  int64         `json:"until_ms"`
	BucketMs int64         `json:"bucket_ms"`
	Totals   AccessSummary `json:"totals"`
	Codes    []AccessCode  `json:"codes"`
	Paths    []AccessTop   `json:"paths"`
	IPs      []AccessTop   `json:"ips"`
	Refs     []AccessTop   `json:"refs"`
	// Countries is the top-N breakdown of requests by GeoIP country code
	// (empty when GEOIP_DB is not configured).
	Countries []AccessTop   `json:"countries,omitempty"`
	Tl        []AccessPoint `json:"timeline"`
}

// QueryAccessRows returns raw parsed requests oldest-first, for CSV export.
// limit is clamped to at most 100000.
func (s *Store) QueryAccessRows(ctx context.Context, app string, sinceMs, untilMs, limit int64) ([]AccessRow, error) {
	return s.QueryAccessRowsFiltered(ctx, app, "", sinceMs, untilMs, limit)
}

// QueryAccessRowsFiltered returns parsed access rows with an optional status class filter.
func (s *Store) QueryAccessRowsFiltered(ctx context.Context, app, status string, sinceMs, untilMs, limit int64) ([]AccessRow, error) {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	cl, args := []string{"ts >= ?"}, []any{sinceMs}
	if untilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, untilMs)
	}
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	if lo, hi, ok := accessStatusRange(status); ok {
		cl, args = append(cl, "status >= ? AND status < ?"), append(args, lo, hi)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT ts,container_id,container,app,ip,country,method,path,status,bytes,referer,ua FROM access WHERE `+strings.Join(cl, " AND ")+` ORDER BY id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRow{}
	for rows.Next() {
		var r AccessRow
		if err := rows.Scan(&r.TS, &r.ContainerID, &r.Container, &r.App, &r.IP, &r.Country, &r.Method, &r.Path, &r.Status, &r.Bytes, &r.Referer, &r.UA); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EachAccessRowFiltered pages the same filter as QueryAccessRowsFiltered but
// hands fn one page at a time, so an export streams to the client instead of
// materializing up to 100000 AccessRows (each carrying path/referer/UA
// strings) for the whole request. It follows the same id-ordered keyset
// pagination as eachLogRow: ids ascend, so `id > last` never repeats or
// skips a row.
func (s *Store) EachAccessRowFiltered(ctx context.Context, app, status string, sinceMs, untilMs, limit int64, fn func([]AccessRow) error) error {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	const page = 2000
	var lastID int64
	written := int64(0)
	for written < limit && ctx.Err() == nil {
		cl, args := []string{"ts >= ?"}, []any{sinceMs}
		if untilMs > 0 {
			cl, args = append(cl, "ts <= ?"), append(args, untilMs)
		}
		if app != "" {
			cl, args = append(cl, "app = ?"), append(args, app)
		}
		if lo, hi, ok := accessStatusRange(status); ok {
			cl, args = append(cl, "status >= ? AND status < ?"), append(args, lo, hi)
		}
		if lastID > 0 {
			cl, args = append(cl, "id > ?"), append(args, lastID)
		}
		n := page
		if left := limit - written; left < int64(n) {
			n = int(left)
		}
		rows, err := s.r.QueryContext(ctx, `SELECT id,ts,container_id,container,app,ip,country,method,path,status,bytes,referer,ua FROM access WHERE `+strings.Join(cl, " AND ")+` ORDER BY id LIMIT ?`, append(args, n)...)
		if err != nil {
			return err
		}
		batch := make([]AccessRow, 0, n)
		for rows.Next() {
			var r AccessRow
			var id int64
			if err := rows.Scan(&id, &r.TS, &r.ContainerID, &r.Container, &r.App, &r.IP, &r.Country, &r.Method, &r.Path, &r.Status, &r.Bytes, &r.Referer, &r.UA); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
			lastID = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		if err := fn(batch); err != nil {
			return err
		}
		written += int64(len(batch))
		if len(batch) < n {
			return nil // partial page: result set exhausted
		}
	}
	return nil
}

// RecordArchive records one archive file in the archive_index table so
// queries can fall back to the archive tier for older ranges.
func (s *Store) RecordArchive(ctx context.Context, kind, day, app, hostID, path string, rows, minTs, maxTs int64) error {
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO archive_index(kind, day, app, host_id, path, rows, min_ts, max_ts, created_ms)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, day, app, hostID, path, rows, minTs, maxTs, time.Now().UnixMilli(),
	)
	return err
}

// DeleteLogsBefore removes log rows older than cutoff (unix ms) from the hot
// DB after they have been archived.
func (s *Store) DeleteLogsBefore(ctx context.Context, cutoff int64) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM logs WHERE ts < ?`, cutoff)
	return err
}

// DeleteMetricsBefore removes metric samples older than cutoff (unix ms)
// from the hot DB after they have been archived.
func (s *Store) DeleteMetricsBefore(ctx context.Context, cutoff int64) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM metrics WHERE ts < ?`, cutoff)
	return err
}

// DeleteSpansBefore removes spans older than cutoff (unix ms) from the hot
// DB after they have been archived.
func (s *Store) DeleteSpansBefore(ctx context.Context, cutoff int64) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM spans WHERE start_ts < ?`, cutoff*1_000_000)
	return err
}

// QuerySpansBefore returns spans older than cutoff (unix ms), ordered by
// start time ascending.
func (s *Store) QuerySpansBefore(ctx context.Context, cutoff int64) ([]Span, error) {
	return s.querySpans(ctx, `WHERE start_ts < ? ORDER BY start_ts`, cutoff*1_000_000)
}

// QueryAccess computes the dashboard aggregates over one window.
// topN limits the "top paths/IPs/referrers" lists (clamped 0..100).
func (s *Store) QueryAccess(ctx context.Context, app string, sinceMs, untilMs, bucketMs int64, topN int) (AccessAggregates, error) {
	return s.QueryAccessFiltered(ctx, app, "", sinceMs, untilMs, bucketMs, topN)
}

// QueryAccessFiltered computes access aggregates with an optional status class filter.
func (s *Store) QueryAccessFiltered(ctx context.Context, app, status string, sinceMs, untilMs, bucketMs int64, topN int) (AccessAggregates, error) {
	if bucketMs < 30000 {
		bucketMs = 30000
	}
	if topN < 0 || topN > 100 {
		topN = 20
	}
	agg := AccessAggregates{SinceMs: sinceMs, UntilMs: untilMs, BucketMs: bucketMs,
		Codes: []AccessCode{}, Paths: []AccessTop{}, IPs: []AccessTop{}, Refs: []AccessTop{}, Countries: []AccessTop{}, Tl: []AccessPoint{}}

	cl, args := []string{"ts >= ?"}, []any{sinceMs}
	if untilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, untilMs)
	}
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	if lo, hi, ok := accessStatusRange(status); ok {
		cl, args = append(cl, "status >= ? AND status < ?"), append(args, lo, hi)
	}
	where := " WHERE " + strings.Join(cl, " AND ")
	withArgs := func(extra ...any) []any { return append(append([]any{}, args...), extra...) }

	row := s.r.QueryRowContext(ctx, `SELECT COUNT(*),
	  COALESCE(SUM(CASE WHEN status BETWEEN 400 AND 499 THEN 1 ELSE 0 END),0),
	  COALESCE(SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END),0),
	  COALESCE(SUM(bytes),0),
	  COUNT(DISTINCT ip) FROM access`+where, args...)
	if err := row.Scan(&agg.Totals.Requests, &agg.Totals.ClientErr, &agg.Totals.ServerErr, &agg.Totals.Bytes, &agg.Totals.UniqueIPs); err != nil {
		return agg, err
	}

	// status code distribution
	rows, err := s.r.QueryContext(ctx, `SELECT status, COUNT(*) FROM access`+where+` GROUP BY status ORDER BY 2 DESC LIMIT 32`, args...)
	if err != nil {
		return agg, err
	}
	for rows.Next() {
		var c AccessCode
		if err := rows.Scan(&c.Code, &c.N); err != nil {
			rows.Close()
			return agg, err
		}
		agg.Codes = append(agg.Codes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return agg, err
	}

	// top paths, IPs, referrers
	for _, j := range []struct {
		sql string
		dst *[]AccessTop
	}{
		{`SELECT path, COUNT(*), COALESCE(SUM(bytes),0), COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0) FROM access` + where + ` GROUP BY path ORDER BY 2 DESC LIMIT ?`, &agg.Paths},
		{`SELECT ip, COUNT(*), COALESCE(SUM(bytes),0), COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0) FROM access` + where + ` GROUP BY ip ORDER BY 2 DESC LIMIT ?`, &agg.IPs},
		{`SELECT referer, COUNT(*), 0, COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0) FROM access` + where + ` AND referer NOT IN ('','-') GROUP BY referer ORDER BY 2 DESC LIMIT ?`, &agg.Refs},
	} {
		rows, err := s.r.QueryContext(ctx, j.sql, withArgs(int64(topN))...)
		if err != nil {
			return agg, err
		}
		for rows.Next() {
			var t AccessTop
			if err := rows.Scan(&t.Key, &t.Hits, &t.Bytes, &t.Errors); err != nil {
				rows.Close()
				return agg, err
			}
			t.Key = strings.TrimSpace(t.Key)
			*j.dst = append(*j.dst, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return agg, err
		}
	}

	// top countries by GeoIP (empty when GEOIP_DB is not configured)
	rows, err = s.r.QueryContext(ctx, `SELECT country, COUNT(*), COALESCE(SUM(bytes),0), COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0) FROM access`+where+` AND country != '' GROUP BY country ORDER BY 2 DESC LIMIT ?`, withArgs(int64(topN))...)
	if err != nil {
		return agg, err
	}
	for rows.Next() {
		var t AccessTop
		if err := rows.Scan(&t.Key, &t.Hits, &t.Bytes, &t.Errors); err != nil {
			rows.Close()
			return agg, err
		}
		t.Key = strings.TrimSpace(t.Key)
		agg.Countries = append(agg.Countries, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return agg, err
	}

	// timeline buckets
	// timeline buckets: (ts/?)*? needs the bucket args before the WHERE args
	rows, err = s.r.QueryContext(ctx, `SELECT (ts/?)*? AS b, COUNT(*), COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0), COALESCE(SUM(bytes),0) FROM access`+where+` GROUP BY b ORDER BY b`, append([]any{bucketMs, bucketMs}, args...)...)
	if err != nil {
		return agg, err
	}
	for rows.Next() {
		var p AccessPoint
		if err := rows.Scan(&p.TS, &p.Hits, &p.Errors, &p.Bytes); err != nil {
			rows.Close()
			return agg, err
		}
		agg.Tl = append(agg.Tl, p)
	}
	rows.Close()
	return agg, rows.Err()
}

func accessStatusRange(status string) (int, int, bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "2xx":
		return 200, 300, true
	case "4xx":
		return 400, 500, true
	case "5xx":
		return 500, 600, true
	default:
		return 0, 0, false
	}
}

func (s *Store) deleteBatched(ctx context.Context, table, col string, cutoff int64) (int64, error) {
	var total int64
	q := fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s < ? LIMIT 5000)`, table, table, col)
	for ctx.Err() == nil {
		res, err := s.w.ExecContext(ctx, q, cutoff)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 {
			break
		}
	}
	return total, ctx.Err()
}

// Prune deletes rows older than the given retention windows.
func (s *Store) Prune(ctx context.Context, logs, metrics, snaps time.Duration) (int64, error) {
	now := time.Now()
	var total int64
	for _, j := range []struct {
		table, col string
		cutoff     int64
		keep       time.Duration
	}{
		{"logs", "ts", now.Add(-logs).UnixNano(), logs},
		{"metrics", "ts", now.Add(-metrics).UnixMilli(), metrics},
		{"snapshots", "ts", now.Add(-snaps).UnixMilli(), snaps},
		{"access", "ts", now.Add(-logs).UnixMilli(), logs}, // parsed requests share the log retention
	} {
		if j.keep <= 0 {
			continue
		}
		n, err := s.deleteBatched(ctx, j.table, j.col, j.cutoff)
		total += n
		s.prunesTotal.Add(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// UsedBytes is the size of live data (total pages minus free pages).
func (s *Store) UsedBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	if err := s.r.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.r.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return 0, err
	}
	if err := s.r.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	return (pages - free) * size, nil
}

// PrunesTotal returns the total number of rows pruned by retention / size
// enforcement (prometheus: capscope_retention_prunes_total).
func (s *Store) PrunesTotal() int64 { return s.prunesTotal.Load() }

// EnforceSize drops the oldest rows — logs first, then metrics, access,
// snapshots, alerts and deploys — until live data fits under maxBytes. The
// low-frequency tables are included so crash/alert/deploy data cannot pin
// the database above the configured cap once the high-volume tables are
// already empty.
func (s *Store) EnforceSize(ctx context.Context, maxBytes int64) (int64, error) {
	var deleted int64
	for i := 0; i < 50 && ctx.Err() == nil; i++ {
		used, err := s.UsedBytes(ctx)
		if err != nil || used <= maxBytes {
			return deleted, err
		}
		trimmed := false
		for _, table := range []string{"logs", "metrics", "access", "snapshots", "alerts", "deploys"} {
			var n int64
			if err := s.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				return deleted, err
			}
			if n == 0 {
				continue
			}
			drop := n / 10
			if drop < 1000 {
				drop = 1000
			}
			res, err := s.w.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM %s ORDER BY id LIMIT ?)`, table, table), drop)
			if err != nil {
				return deleted, err
			}
			c, _ := res.RowsAffected()
			deleted += c
			s.prunesTotal.Add(c)
			trimmed = true
			break
		}
		if !trimmed {
			break
		}
	}
	_, _ = s.w.ExecContext(ctx, "PRAGMA incremental_vacuum")
	return deleted, nil
}

// PrunePerApp drops, for each app with a shorter retention than the global
// one, log and access rows older than that app's cutoff (S7). The global
// Prune still runs and is the upper bound, so a per-app value can only make
// retention tighter, never longer.
func (s *Store) PrunePerApp(ctx context.Context, ret map[string]time.Duration) (int64, error) {
	if len(ret) == 0 {
		return 0, nil
	}
	now := time.Now()
	var total int64
	for app, keep := range ret {
		if keep <= 0 {
			continue
		}
		cutoff := now.Add(-keep).UnixNano()
		for _, tbl := range []struct {
			name, col string
			cutoff    int64
		}{
			{"logs", "ts", cutoff},
			{"access", "ts", now.Add(-keep).UnixMilli()},
		} {
			var query string
			if tbl.name == "logs" {
				// After normalization, logs no longer has an app column; match via containers.
				query = fmt.Sprintf(`DELETE FROM %s WHERE container_id IN (SELECT id FROM containers WHERE app=?) AND %s<?`, tbl.name, tbl.col)
			} else {
				query = fmt.Sprintf(`DELETE FROM %s WHERE app=? AND %s<?`, tbl.name, tbl.col)
			}
			res, err := s.w.ExecContext(ctx, query, app, tbl.cutoff)
			if err != nil {
				return total, err
			}
			c, _ := res.RowsAffected()
			total += c
		}
	}
	return total, nil
}

// AppLogBytes estimates the stored log bytes of one app: the sum of line
// lengths plus a measured per-row overhead (indexes and fixed columns). Good
// enough to enforce a budget without a per-row size table.
func (s *Store) AppLogBytes(ctx context.Context, app string) (int64, error) {
	var n, chars int64
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(line)),0) FROM logs_view WHERE app=?`, app).Scan(&n, &chars); err != nil {
		return 0, err
	}
	return chars + n*rowOverhead, nil
}

// rowOverhead is the measured fixed cost of one log row (id, ts, container_id,
// container, app, stream, level columns plus index entries), ~270 bytes.
const rowOverhead = 270

// EnforcePerAppSize drops the oldest logs of any app whose estimated log
// bytes exceed its budget (S7), until every app fits. It runs before the
// global size cap so a single noisy app is trimmed on its own first.
func (s *Store) EnforcePerAppSize(ctx context.Context, budgets map[string]int64) (int64, error) {
	if len(budgets) == 0 {
		return 0, nil
	}
	var deleted int64
	for app, maxBytes := range budgets {
		if maxBytes <= 0 {
			continue
		}
		for i := 0; i < 50 && ctx.Err() == nil; i++ {
			used, err := s.AppLogBytes(ctx, app)
			if err != nil || used <= maxBytes {
				break
			}
			var n int64
			if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs_view WHERE app=?`, app).Scan(&n); err != nil || n == 0 {
				break
			}
			drop := n / 10
			if drop < 100 {
				drop = 100
			}
			if drop > n {
				drop = n
			}
			res, err := s.w.ExecContext(ctx, `DELETE FROM logs WHERE id IN (SELECT id FROM logs_view WHERE app=? ORDER BY id LIMIT ?)`, app, drop)
			if err != nil {
				return deleted, err
			}
			c, _ := res.RowsAffected()
			deleted += c
			if c == 0 {
				break
			}
		}
	}
	_, _ = s.w.ExecContext(ctx, "PRAGMA incremental_vacuum")
	return deleted, nil
}

// NormalizeSchema performs the PRD S6 row normalization: it removes the
// denormalized `container` and `app` columns from the logs table (and the
// equivalent columns from metrics, access, and snapshots) so each log row no
// longer repeats the container name and app string. The containers table
// already persists name+app per container_id, so logs can JOIN to recover them.
//
// This saves ~80 bytes per log row (~20% of the measured ~400 bytes/row).
//
// It is intentionally NOT auto-applied: it rewrites the logs table (the
// largest table), rebuilds the FTS5 index, and changes the column set that
// /api/sql and ad-hoc queries see. Operators should run it during a planned
// maintenance window, after taking a backup of the SQLite file.
//
// To apply: set CAPSCOPE_ENABLE_NORMALIZE=1 and call this function (e.g. via
// a one-off admin command or the /api/admin/normalize-schema endpoint).
//
// After normalization, all queries that previously selected `container`/`app`
// from `logs` must JOIN with `containers` instead. A compatibility view
// `logs_view` (id, ts, container_id, container, app, host_id, stream, level,
// line) is created so existing /api/sql queries keep working unchanged.
func (s *Store) NormalizeSchema(ctx context.Context) error {
	if os.Getenv("CAPSCOPE_ENABLE_NORMALIZE") != "1" {
		return nil // opt-in only; refuse to run silently
	}
	// Idempotent: if logs already lacks the container/app column, we're done.
	var hasContainer int
	err := s.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('logs') WHERE name='container'`).Scan(&hasContainer)
	if err != nil {
		return fmt.Errorf("normalize: inspect logs schema: %w", err)
	}
	if hasContainer == 0 {
		return nil // already normalized
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("normalize: begin tx: %w", err)
	}
	defer tx.Rollback()
	// 1. Build the normalized logs table (no container/app columns).
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE logs_n(
		   id INTEGER PRIMARY KEY AUTOINCREMENT,
		   ts INTEGER NOT NULL,
		   container_id TEXT NOT NULL,
		   host_id TEXT NOT NULL DEFAULT '',
		   stream TEXT NOT NULL,
		   level TEXT NOT NULL DEFAULT '',
		   line TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("normalize: create logs_n: %w", err)
	}
	// 2. Copy data across.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO logs_n(id, ts, container_id, host_id, stream, level, line)
		SELECT id, ts, container_id, host_id, stream, level, line FROM logs`); err != nil {
		return fmt.Errorf("normalize: copy logs: %w", err)
	}
	// 3. Swap tables.
	if _, err := tx.ExecContext(ctx, `DROP TABLE logs`); err != nil {
		return fmt.Errorf("normalize: drop logs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE logs_n RENAME TO logs`); err != nil {
		return fmt.Errorf("normalize: rename logs_n: %w", err)
	}
	// 4. Recreate indexes (without app).
	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX idx_logs_cid_id ON logs(container_id, id);
		CREATE INDEX idx_logs_ts ON logs(ts);
		CREATE INDEX IF NOT EXISTS idx_logs_ts_app ON logs(ts, container_id);
		CREATE INDEX idx_logs_host ON logs(host_id);`); err != nil {
		return fmt.Errorf("normalize: recreate indexes: %w", err)
	}
	// 5. Rebuild FTS5 triggers and index.
	if _, err := tx.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS logs_ai;
		DROP TRIGGER IF EXISTS logs_ad;
		CREATE TRIGGER logs_ai AFTER INSERT ON logs BEGIN
		  INSERT INTO logs_fts(rowid, line) VALUES (new.id, new.line);
		END;
		CREATE TRIGGER logs_ad AFTER DELETE ON logs BEGIN
		  INSERT INTO logs_fts(logs_fts, rowid, line) VALUES('delete', old.id, old.line);
		END;
		INSERT INTO logs_fts(logs_fts) VALUES('rebuild');`); err != nil {
		return fmt.Errorf("normalize: rebuild FTS5: %w", err)
	}
	// 6. Compatibility view: old column set via JOIN with containers.
	if _, err := tx.ExecContext(ctx, `
		CREATE VIEW logs_view AS
		  SELECT l.id, l.ts, l.container_id, c.name AS container, c.app AS app,
		         l.host_id, l.stream, l.level, l.line
		  FROM logs l JOIN containers c ON l.container_id = c.id`); err != nil {
		return fmt.Errorf("normalize: create logs_view: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("normalize: commit: %w", err)
	}
	s.normalized.Store(true)
	return nil
}

// ---- alerts ----

// Alert is one recorded alert firing.
type Alert struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"` // unix ms
	Rule     string `json:"rule"`
	App      string `json:"app"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Delivery string `json:"delivery"` // "sent" | "failed" | "" (older rows)
}

func (s *Store) InsertAlert(a Alert) (int64, error) {
	res, err := s.w.Exec(`INSERT INTO alerts(ts,rule,app,kind,message) VALUES(?,?,?,?,?)`, a.TS, a.Rule, a.App, a.Kind, a.Message)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Delivery is filled by SetAlertDelivery once notify() has run; the
	// column defaults to '' so older rows stay readable.
	return id, nil
}

// SetAlertDelivery records the outcome of notifying about one alert:
// "sent" when at least one channel accepted the message, "failed" when
// every attempted channel errored. An unknown id is a silent no-op.
func (s *Store) SetAlertDelivery(id int64, status string) error {
	_, err := s.w.Exec(`UPDATE alerts SET delivery=? WHERE id=?`, status, id)
	return err
}

// ListAlerts returns fired alerts newest first; app filters to one app.
func (s *Store) ListAlerts(ctx context.Context, app string, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cl, args := []string{"1=1"}, []any{}
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id,ts,rule,app,kind,message,delivery FROM alerts WHERE `+strings.Join(cl, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.TS, &a.Rule, &a.App, &a.Kind, &a.Message, &a.Delivery); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LastAlertTS returns the newest firing time (unix ms) of one rule for one
// app, or 0 when it never fired. It drives the silence window between
// notifications for the same condition.
func (s *Store) LastAlertTS(ctx context.Context, rule, app string) int64 {
	var ts int64
	_ = s.r.QueryRowContext(ctx, `SELECT ts FROM alerts WHERE rule=? AND app=? ORDER BY id DESC LIMIT 1`, rule, app).Scan(&ts)
	return ts
}

// LastLogTS returns the newest stored log timestamp of an app in unix ms,
// or 0 when the app has no logs. Feeds the dead-man rule (A1).
func (s *Store) LastLogTS(ctx context.Context, app string) int64 {
	var ts int64
	_ = s.r.QueryRowContext(ctx, `SELECT MAX(ts) FROM logs_view WHERE app=?`, app).Scan(&ts)
	if ts > 0 {
		return ts / 1_000_000
	}
	return 0
}

// MetricSamples returns raw (un-bucketed) samples of one app in a time range,
// oldest first, capped so an alert tick cannot scan forever. Feeds the metric
// threshold rule (A3), which needs every sample, not buckets.
func (s *Store) MetricSamples(ctx context.Context, app string, sinceMs, untilMs int64, limit int) ([]MetricPoint, error) {
	if limit <= 0 || limit > maxRegexScan {
		limit = maxRegexScan
	}
	q := `SELECT ts,cpu,mem,mem_limit,net_rx,net_tx,blk_r,blk_w,pids FROM metrics WHERE app=? AND ts>=?`
	args := []any{app, sinceMs}
	if untilMs > 0 {
		q += ` AND ts<=?`
		args = append(args, untilMs)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricPoint{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.TS, &p.CPU, &p.Mem, &p.MemLimit, &p.NetRx, &p.NetTx, &p.BlkR, &p.BlkW, &p.Pids); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The query returns newest-first; reverse so callers (evalMetric) see
	// ascending samples and pts[len-1] is genuinely the newest sample, not the
	// 2000th-oldest one. Without this the freshness check on busy apps reads
	// a stale sample and can suppress a firing alert.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CountAppDeathsSince counts stored deaths of any container of one app at or
// after sinceMs. Feeds the restart-loop alert rule (A2).
func (s *Store) CountAppDeathsSince(ctx context.Context, app string, sinceMs int64) (int64, error) {
	var n int64
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots WHERE app=? AND die_ts>=?`, app, sinceMs).Scan(&n)
	return n, err
}

// CountLogsMatching counts stored log lines of an app at or after sinceMs
// whose line matches re. Feeds the log-pattern alert rule (A4). The scan is
// capped like regex search so a huge window cannot pin the read pool.
func (s *Store) CountLogsMatching(ctx context.Context, app string, re *regexp.Regexp, sinceMs int64) (int64, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT line FROM logs_view WHERE app=? AND ts>=?`, app, sinceMs*1_000_000)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n, scanned int64
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return n, err
		}
		if scanned++; scanned > maxRegexScan {
			break
		}
		if re.MatchString(line) {
			n++
		}
	}
	return n, rows.Err()
}

// ---- deploys ----

// Deploy is a recorded release marker (V9). It is written by a user or CI so
// the charts and log timeline can show when a release landed, and a crash can
// be correlated with the deploy just before it.
type Deploy struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"` // unix milliseconds
	App     string `json:"app"`
	Version string `json:"version"`
	Note    string `json:"note,omitempty"`
}

// RecordDeploy stores one marker and fills in its ID. ts 0 means "now".
func (s *Store) RecordDeploy(d Deploy) (int64, error) {
	if d.TS <= 0 {
		d.TS = time.Now().UnixMilli()
	}
	res, err := s.w.Exec(`INSERT INTO deploys(ts,app,version,note) VALUES(?,?,?,?)`, d.TS, d.App, d.Version, d.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListDeploys returns markers newest first. app filters to one app; when it
// is empty every app's markers are returned (the UI labels them by app).
// sinceMs/untilMs bound the range (either may be 0 to leave it open).
func (s *Store) ListDeploys(ctx context.Context, app string, sinceMs, untilMs int64, limit int) ([]Deploy, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cl, args := []string{"1=1"}, []any{}
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	if sinceMs > 0 {
		cl, args = append(cl, "ts >= ?"), append(args, sinceMs)
	}
	if untilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, untilMs)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id,ts,app,version,note FROM deploys WHERE `+strings.Join(cl, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deploy{}
	for rows.Next() {
		var d Deploy
		if err := rows.Scan(&d.ID, &d.TS, &d.App, &d.Version, &d.Note); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeployBefore returns the newest marker of an app at or before beforeMs, or
// the zero Deploy when there is none. The crash detail view uses it to answer
// "did a deploy just before this crash?".
func (s *Store) DeployBefore(ctx context.Context, app string, beforeMs int64) (Deploy, error) {
	var d Deploy
	err := s.r.QueryRowContext(ctx, `SELECT id,ts,app,version,note FROM deploys WHERE app=? AND ts<=? ORDER BY id DESC LIMIT 1`, app, beforeMs).Scan(&d.ID, &d.TS, &d.App, &d.Version, &d.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Deploy{}, nil
	}
	return d, err
}

// isDuplicateSchemaErr reports whether err is SQLite complaining that an
// object being created already exists ("table X already exists",
// "index X already exists", or a duplicate ALTER TABLE column). migrate()
// uses this to distinguish a half-applied upgrade from a real failure.
func isDuplicateSchemaErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate column name")
}

// migrationObjectsExist verifies the schema objects a migration creates are
// actually present in sqlite_master (and, for ALTER ADD COLUMN, on the live
// table). Only used after isDuplicateSchemaErr, so it must answer strictly:
// every object named by a CREATE statement in mig must exist; otherwise the
// "already exists" error came from something unexpected and the migration
// should still fail loudly.
func migrationObjectsExist(db *sql.DB, i int) bool {
	seen := 0
	for _, line := range strings.Split(migrations[i], "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		var kind, name string
		switch {
		case strings.HasPrefix(upper, "CREATE VIRTUAL TABLE"):
			kind, name = "table", extractIdent(line[len("CREATE VIRTUAL TABLE"):])
		case strings.HasPrefix(upper, "CREATE TABLE"):
			kind, name = "table", extractIdent(line[len("CREATE TABLE"):])
		case strings.HasPrefix(upper, "CREATE INDEX"), strings.HasPrefix(upper, "CREATE UNIQUE INDEX"):
			kind, name = "index", extractIdent(strings.TrimPrefix(strings.TrimPrefix(line, "CREATE UNIQUE INDEX"), "CREATE INDEX"))
		case strings.HasPrefix(upper, "CREATE TRIGGER"):
			kind, name = "trigger", extractIdent(line[len("CREATE TRIGGER"):])
		default:
			continue
		}
		if name == "" {
			return false
		}
		var n int
		q := `SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`
		if err := db.QueryRow(q, kind, name).Scan(&n); err != nil || n == 0 {
			return false
		}
		// Indexes and triggers live inside the same migration as their table;
		// a half-applied run can have the table but not its index/trigger, so
		// require every object in the migration to be present before skipping.
		seen++
	}
	if seen > 0 {
		return true
	}
	// No CREATE statements: the duplicate must be an ALTER TABLE ... ADD
	// COLUMN. Confirm every added column exists on its table.
	cols := 0
	for _, m := range regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+ADD\s+COLUMN\s+(\w+)`).FindAllStringSubmatch(migrations[i], -1) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, m[1], m[2]).Scan(&n); err != nil || n == 0 {
			return false
		}
		cols++
	}
	return cols > 0
}

// extractIdent pulls the first token of a CREATE clause, stripping IF NOT
// EXISTS and any quoting/backticks or trailing punctuation (SQLite lets you
// write "CREATE TABLE t(" with no space before the paren).
func extractIdent(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(strings.ToUpper(s), "IF NOT EXISTS "); idx >= 0 {
		s = s[idx+len("IF NOT EXISTS "):]
	}
	s = strings.TrimSpace(s)
	s = strings.Fields(s)[0]
	return strings.Trim(s, "`\"[]();,")
}

// Incident state constants used by CreateIncident / UpdateIncidentState.
const (
	StateOpen    = "open"
	StateResolved = "resolved"
)

// Audit log (Phase 7): append-only audit trail for security-sensitive actions.
const (
	AuditActionSQL       = "sql"
	AuditActionAlertRule = "alert_rule"
	AuditActionDeploy    = "deploy"
	AuditActionIncident  = "incident"
	AuditActionUserMgmt  = "user_mgmt"
)

// ---- incident management (Phase 5) ----

// CreateIncident inserts a new incident and returns its ID. If an open
// incident with the same title+app already exists, returns its ID instead.
func (s *Store) CreateIncident(ctx context.Context, title, severity, state string, startedAt int64, relatedAlerts []int64, app, hostID string) (int64, error) {
	// Deduplicate open incidents by title+app.
	if state == StateOpen {
		var id int64
		err := s.r.QueryRowContext(ctx, `SELECT id FROM incidents WHERE state='open' AND title=? AND app=? LIMIT 1`, title, app).Scan(&id)
		if err == nil {
			return id, nil
		}
	}
	alertsJSON, _ := json.Marshal(relatedAlerts)
	res, err := s.w.ExecContext(ctx, `
		INSERT INTO incidents(title, severity, state, started_at, ended_at,
		                      related_alert_ids, app, host_id)
		VALUES(?, ?, ?, ?, 0, ?, ?, ?)`,
		title, severity, state, startedAt, string(alertsJSON), app, hostID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetIncident returns the incident with the given ID, or nil if not found.
func (s *Store) GetIncident(ctx context.Context, id int64) (*IncidentRow, error) {
	var inc IncidentRow
	var alertsJSON string
	err := s.r.QueryRowContext(ctx, `
		SELECT id, title, severity, state, started_at, ended_at,
		       related_alert_ids, app, host_id FROM incidents WHERE id = ?`,
		id).Scan(&inc.ID, &inc.Title, &inc.Severity, &inc.State,
		&inc.StartedAt, &inc.EndedAt, &alertsJSON, &inc.App, &inc.HostID)
	if err != nil {
		return nil, err
	}
	if alertsJSON != "" {
		_ = json.Unmarshal([]byte(alertsJSON), &inc.RelatedAlerts)
	}
	return &inc, nil
}

// ListIncidents returns incidents, optionally filtered by state and app.
func (s *Store) ListIncidents(ctx context.Context, state, app string, limit int) ([]IncidentRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var cl []string
	var args []any
	if state != "" {
		cl, args = append(cl, "state = ?"), append(args, state)
	}
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	where := ""
	if len(cl) > 0 {
		where = " WHERE " + strings.Join(cl, " AND ")
	}
	q := `SELECT id, title, severity, state, started_at, ended_at,
	               related_alert_ids, app, host_id FROM incidents` + where +
		` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IncidentRow
	for rows.Next() {
		var inc IncidentRow
		var alertsJSON string
		if err := rows.Scan(&inc.ID, &inc.Title, &inc.Severity, &inc.State,
			&inc.StartedAt, &inc.EndedAt, &alertsJSON, &inc.App, &inc.HostID); err != nil {
			return nil, err
		}
		if alertsJSON != "" {
			_ = json.Unmarshal([]byte(alertsJSON), &inc.RelatedAlerts)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// UpdateIncidentState changes an incident's state. When resolving, sets
// ended_at to now.
func (s *Store) UpdateIncidentState(ctx context.Context, id int64, state string) error {
	var endedAt int64
	if state == StateResolved {
		endedAt = time.Now().UnixMilli()
	}
	_, err := s.w.ExecContext(ctx, `UPDATE incidents SET state=?, ended_at=? WHERE id=?`, state, endedAt, id)
	return err
}

// AddIncidentAlert associates an alert firing ID with an incident.
func (s *Store) AddIncidentAlert(ctx context.Context, incidentID, alertID int64) error {
	inc, err := s.GetIncident(ctx, incidentID)
	if err != nil {
		return err
	}
	for _, a := range inc.RelatedAlerts {
		if a == alertID {
			return nil
		}
	}
	inc.RelatedAlerts = append(inc.RelatedAlerts, alertID)
	alertsJSON, _ := json.Marshal(inc.RelatedAlerts)
	_, err = s.w.ExecContext(ctx, `UPDATE incidents SET related_alert_ids=? WHERE id=?`, string(alertsJSON), incidentID)
	return err
}

// IncidentRow is the database row representation of an incident.
type IncidentRow struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Severity      string  `json:"severity"`
	State         string  `json:"state"`
	StartedAt     int64   `json:"started_at"`
	EndedAt       int64   `json:"ended_at"`
	RelatedAlerts []int64 `json:"related_alert_ids"`
	App           string  `json:"app"`
	HostID        string  `json:"host_id"`
}

// InsertAuditLog appends an audit entry (Phase 7). Fail-closed: any error is
// returned so callers can decide whether to block the underlying action.
func (s *Store) InsertAuditLog(ctx context.Context, actor, action, resource, target, meta string) error {
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO audit_log(ts, actor, action, resource, target, meta)
		VALUES(?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), actor, action, resource, target, meta,
	)
	return err
}

// ListAuditLog returns audit entries, newest first, optionally filtered by
// actor or action.
func (s *Store) ListAuditLog(ctx context.Context, actor, action string, limit int) ([]AuditLogRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var cl []string
	var args []any
	if actor != "" {
		cl, args = append(cl, "actor = ?"), append(args, actor)
	}
	if action != "" {
		cl, args = append(cl, "action = ?"), append(args, action)
	}
	where := ""
	if len(cl) > 0 {
		where = " WHERE " + strings.Join(cl, " AND ")
	}
	q := `SELECT id, ts, actor, action, resource, target, meta FROM audit_log` + where +
		` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditLogRow
	for rows.Next() {
		var a AuditLogRow
		var metaJSON string
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.Resource, &a.Target, &metaJSON); err != nil {
			return nil, err
		}
		if metaJSON != "" {
			_ = json.Unmarshal([]byte(metaJSON), &a.Meta)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditLogRow is one audit log entry.
type AuditLogRow struct {
	ID      int64            `json:"id"`
	TS      int64            `json:"ts"`
	Actor   string           `json:"actor"`
	Action  string           `json:"action"`
	Resource string          `json:"resource"`
	Target  string           `json:"target"`
	Meta    map[string]string `json:"meta,omitempty"`
}


