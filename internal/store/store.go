// Package store persists logs, metrics, container metadata and crash snapshots in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	w *sql.DB // single writer connection
	r *sql.DB // read-only pool
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
}

// Open creates or migrates the database at path.
func Open(path string) (*Store, error) {
	base := "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
	w, err := sql.Open("sqlite3", base)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	r, err := sql.Open("sqlite3", base+"&_query_only=1")
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
	Stream      string `json:"stream"` // "o" | "e"
	Level       string `json:"level"`
	Line        string `json:"line"`
}

type MetricPoint struct {
	TS       int64   `json:"ts"` // unix ms
	CPU      float64 `json:"cpu"`
	Mem      int64   `json:"mem"`
	MemLimit int64   `json:"mem_limit"`
	NetRx    int64   `json:"net_rx"`
	NetTx    int64   `json:"net_tx"`
	BlkR     int64   `json:"blk_r"`
	BlkW     int64   `json:"blk_w"`
	Pids     int64   `json:"pids"`
}

type ContainerRow struct {
	ID, Name, App, Image, Replica, State string
	FirstSeen, LastSeen                  int64
}

type Snapshot struct {
	ID             int64           `json:"id"`
	TS             int64           `json:"ts"`
	DieTS          int64           `json:"die_ts"` // death time from the Docker event (0 when unknown)
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
	Deploy         *Deploy         `json:"deploy,omitempty"` // newest deploy before the crash (V9, API only, never stored)
}

type AppSummary struct {
	App        string `json:"app"`
	Containers int    `json:"containers"`
	LastSeen   int64  `json:"last_seen"`
	Errors1h   int64  `json:"errors_1h"`
	Warns1h    int64  `json:"warns_1h"`
}

// AccessRow is one parsed access-log request, keyed by the container that
// emitted it (App is the reverse proxy's app, not the route's target).
type AccessRow struct {
	TS          int64  `json:"ts"` // unix ms
	ContainerID string `json:"container_id"`
	Container   string `json:"container"`
	App         string `json:"app"`
	IP          string `json:"ip"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	Bytes       int64  `json:"bytes"`
	Referer     string `json:"referer,omitempty"`
	UA          string `json:"ua,omitempty"`
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
	stmt, err := tx.Prepare(`INSERT INTO logs(ts,container_id,container,app,stream,level,line) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i := range rows {
		r := &rows[i]
		res, err := stmt.Exec(r.TS, r.ContainerID, r.Container, r.App, r.Stream, r.Level, r.Line)
		if err != nil {
			tx.Rollback()
			return err
		}
		r.ID, _ = res.LastInsertId()
	}
	return tx.Commit()
}

type LogQuery struct {
	App, Container, Stream, Q string
	Levels                    []string
	Regex                     *regexp.Regexp // when set, Q is ignored and matching happens in Go
	SinceMs, UntilMs          int64
	AfterID, BeforeID         int64
	Limit                     int
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
	if q.Container != "" && r.Container != q.Container && !strings.HasPrefix(r.ContainerID, q.Container) {
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
	if q.AfterID > 0 {
		order = "ASC"
	}
	query := "SELECT id, ts, container_id, container, app, stream, level, line FROM logs" + where + " ORDER BY id " + order
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
		if err := rows.Scan(&r.ID, &r.TS, &r.ContainerID, &r.Container, &r.App, &r.Stream, &r.Level, &r.Line); err != nil {
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
	query := "SELECT id, ts, container_id, container, app, stream, level, line FROM logs" + where + " ORDER BY id " + order + " LIMIT ?"
	args = append(args, q.Limit)
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LogRow, 0, 64)
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.TS, &r.ContainerID, &r.Container, &r.App, &r.Stream, &r.Level, &r.Line); err != nil {
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
	_, err := s.w.Exec(`INSERT INTO metrics(ts,container_id,app,cpu,mem,mem_limit,net_rx,net_tx,blk_r,blk_w,pids) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		p.TS, containerID, app, p.CPU, p.Mem, p.MemLimit, p.NetRx, p.NetTx, p.BlkR, p.BlkW, p.Pids)
	return err
}

// QueryMetrics buckets samples. For an app it averages per container inside each bucket,
// then sums across containers, so the chart shows total app usage.
func (s *Store) QueryMetrics(ctx context.Context, app, containerID string, sinceMs, untilMs, bucketMs int64) ([]MetricPoint, error) {
	if bucketMs < 1000 {
		bucketMs = 1000
	}
	var cl []string
	var args []any
	args = append(args, bucketMs, bucketMs)
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
	}
	if containerID != "" {
		cl, args = append(cl, "container_id LIKE ?"), append(args, containerID+"%")
	}
	cl, args = append(cl, "ts >= ?"), append(args, sinceMs)
	if untilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, untilMs)
	}
	q := `SELECT b, SUM(cpu), SUM(mem), SUM(lim), SUM(rx), SUM(tx) FROM (
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
	_, err := s.w.Exec(`INSERT INTO containers(id,name,app,image,replica,state,first_seen,last_seen) VALUES(?,?,?,?,?,?,?,?)
	  ON CONFLICT(id) DO UPDATE SET state=excluded.state, last_seen=excluded.last_seen, name=excluded.name, app=excluded.app, image=excluded.image`,
		c.ID, c.Name, c.App, c.Image, c.Replica, c.State, c.FirstSeen, c.LastSeen)
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

func (s *Store) AppSummaries(ctx context.Context) ([]AppSummary, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT app, SUM(CASE WHEN state='running' THEN 1 ELSE 0 END), MAX(last_seen) FROM containers GROUP BY app ORDER BY app`)
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
	r2, err := s.r.QueryContext(ctx, `SELECT app, SUM(CASE WHEN level='error' THEN 1 ELSE 0 END), SUM(CASE WHEN level='warn' THEN 1 ELSE 0 END) FROM logs WHERE ts >= ? GROUP BY app`, since)
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
	res, err := s.w.Exec(`INSERT INTO snapshots(ts,kind,container_id,container,app,image,exit_code,oom_killed,error,restart_count,restarts_window,mem_limit,uptime_sec,die_ts,summary,logs_json,metrics_json,inspect_json)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sn.TS, sn.Kind, sn.ContainerID, sn.Container, sn.App, sn.Image, sn.ExitCode, oom, sn.Error, sn.RestartCount, sn.RestartsWindow, sn.MemLimit, sn.UptimeSec, sn.DieTS, sn.Summary, string(logs), string(metrics), insp)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SnapshotExists reports whether a snapshot for this death was already stored.
// It deduplicates die events replayed from the Docker event buffer after a
// restart (C8). dieMs 0 (older rows, unknown death time) never deduplicates.
func (s *Store) SnapshotExists(ctx context.Context, containerID string, dieMs int64) (bool, error) {
	if dieMs <= 0 {
		return false, nil
	}
	var one int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM snapshots WHERE container_id=? AND die_ts=? LIMIT 1`, containerID, dieMs).Scan(&one)
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

const snapCols = `id,ts,kind,container_id,container,app,image,exit_code,oom_killed,error,restart_count,restarts_window,mem_limit,uptime_sec,die_ts,summary`

func scanSnap(sc interface{ Scan(...any) error }, extra ...any) (Snapshot, error) {
	var sn Snapshot
	var oom int
	dest := append([]any{&sn.ID, &sn.TS, &sn.Kind, &sn.ContainerID, &sn.Container, &sn.App, &sn.Image, &sn.ExitCode, &oom, &sn.Error, &sn.RestartCount, &sn.RestartsWindow, &sn.MemLimit, &sn.UptimeSec, &sn.DieTS, &sn.Summary}, extra...)
	err := sc.Scan(dest...)
	sn.OOMKilled = oom == 1
	return sn, err
}

// ListSnapshots returns light rows (no logs or metrics), newest first.
func (s *Store) ListSnapshots(ctx context.Context, app string, limit int) ([]Snapshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + snapCols + ` FROM snapshots`
	var args []any
	if app != "" {
		q += ` WHERE app = ?`
		args = append(args, app)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
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
	stmt, err := tx.Prepare(`INSERT INTO access(ts,container_id,container,app,ip,method,path,status,bytes,referer,ua) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i := range rows {
		r := rows[i]
		if _, err := stmt.Exec(r.TS, r.ContainerID, r.Container, r.App, r.IP, r.Method, r.Path, r.Status, r.Bytes, r.Referer, r.UA); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
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
	SinceMs, UntilMs, BucketMs int64
	Totals                     AccessSummary
	Codes                      []AccessCode
	Paths                      []AccessTop
	IPs                        []AccessTop
	Refs                       []AccessTop
	Tl                         []AccessPoint `json:"timeline"`
}

// QueryAccessRows returns raw parsed requests oldest-first, for CSV export.
// limit is clamped to at most 100000.
func (s *Store) QueryAccessRows(ctx context.Context, app string, sinceMs, untilMs, limit int64) ([]AccessRow, error) {
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
	rows, err := s.r.QueryContext(ctx, `SELECT ts,container_id,container,app,ip,method,path,status,bytes,referer,ua FROM access WHERE `+strings.Join(cl, " AND ")+` ORDER BY id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRow{}
	for rows.Next() {
		var r AccessRow
		if err := rows.Scan(&r.TS, &r.ContainerID, &r.Container, &r.App, &r.IP, &r.Method, &r.Path, &r.Status, &r.Bytes, &r.Referer, &r.UA); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryAccess computes the dashboard aggregates over one window.
// topN limits the "top paths/IPs/referrers" lists (clamped 0..100).
func (s *Store) QueryAccess(ctx context.Context, app string, sinceMs, untilMs, bucketMs int64, topN int) (AccessAggregates, error) {
	if bucketMs < 30000 {
		bucketMs = 30000
	}
	if topN < 0 || topN > 100 {
		topN = 20
	}
	agg := AccessAggregates{SinceMs: sinceMs, UntilMs: untilMs, BucketMs: bucketMs,
		Codes: []AccessCode{}, Paths: []AccessTop{}, IPs: []AccessTop{}, Refs: []AccessTop{}, Tl: []AccessPoint{}}

	cl, args := []string{"ts >= ?"}, []any{sinceMs}
	if untilMs > 0 {
		cl, args = append(cl, "ts <= ?"), append(args, untilMs)
	}
	if app != "" {
		cl, args = append(cl, "app = ?"), append(args, app)
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

// EnforceSize drops the oldest logs, then metrics, then parsed access rows
// until live data fits under maxBytes.
func (s *Store) EnforceSize(ctx context.Context, maxBytes int64) (int64, error) {
	var deleted int64
	for i := 0; i < 50 && ctx.Err() == nil; i++ {
		used, err := s.UsedBytes(ctx)
		if err != nil || used <= maxBytes {
			return deleted, err
		}
		trimmed := false
		for _, table := range []string{"logs", "metrics", "access"} {
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
			res, err := s.w.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE app=? AND %s<?`, tbl.name, tbl.col), app, tbl.cutoff)
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
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(line)),0) FROM logs WHERE app=?`, app).Scan(&n, &chars); err != nil {
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
			if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs WHERE app=?`, app).Scan(&n); err != nil || n == 0 {
				break
			}
			drop := n / 10
			if drop < 100 {
				drop = 100
			}
			if drop > n {
				drop = n
			}
			res, err := s.w.ExecContext(ctx, `DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE app=? ORDER BY id LIMIT ?)`, app, drop)
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

// ---- alerts ----

// Alert is one recorded alert firing.
type Alert struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"` // unix ms
	Rule    string `json:"rule"`
	App     string `json:"app"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (s *Store) InsertAlert(a Alert) (int64, error) {
	res, err := s.w.Exec(`INSERT INTO alerts(ts,rule,app,kind,message) VALUES(?,?,?,?,?)`, a.TS, a.Rule, a.App, a.Kind, a.Message)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
	rows, err := s.r.QueryContext(ctx, `SELECT id,ts,rule,app,kind,message FROM alerts WHERE `+strings.Join(cl, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.TS, &a.Rule, &a.App, &a.Kind, &a.Message); err != nil {
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
	_ = s.r.QueryRowContext(ctx, `SELECT MAX(ts) FROM logs WHERE app=?`, app).Scan(&ts)
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
	q += ` ORDER BY ts ASC LIMIT ?`
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
	return out, rows.Err()
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
	rows, err := s.r.QueryContext(ctx, `SELECT line FROM logs WHERE app=? AND ts>=?`, app, sinceMs*1_000_000)
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
