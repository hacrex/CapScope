// Package store persists logs, metrics, container metadata and crash snapshots in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	ID           int64           `json:"id"`
	TS           int64           `json:"ts"`
	Kind         string          `json:"kind"`
	ContainerID  string          `json:"container_id"`
	Container    string          `json:"container"`
	App          string          `json:"app"`
	Image        string          `json:"image"`
	ExitCode     int             `json:"exit_code"`
	OOMKilled    bool            `json:"oom_killed"`
	Error        string          `json:"error"`
	RestartCount int             `json:"restart_count"`
	MemLimit     int64           `json:"mem_limit"`
	UptimeSec    int64           `json:"uptime_sec"`
	Summary      string          `json:"summary"`
	Logs         []LogRow        `json:"logs,omitempty"`
	Metrics      []MetricPoint   `json:"metrics,omitempty"`
	Inspect      json.RawMessage `json:"inspect,omitempty"`
}

type AppSummary struct {
	App        string `json:"app"`
	Containers int    `json:"containers"`
	LastSeen   int64  `json:"last_seen"`
	Errors1h   int64  `json:"errors_1h"`
	Warns1h    int64  `json:"warns_1h"`
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
	if q.Q != "" && q.Regex == nil {
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

// QueryLogs returns rows oldest-first. Without AfterID it returns the newest Limit rows.
func (s *Store) QueryLogs(ctx context.Context, q LogQuery) ([]LogRow, error) {
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500
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
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
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
	res, err := s.w.Exec(`INSERT INTO snapshots(ts,kind,container_id,container,app,image,exit_code,oom_killed,error,restart_count,mem_limit,uptime_sec,summary,logs_json,metrics_json,inspect_json)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sn.TS, sn.Kind, sn.ContainerID, sn.Container, sn.App, sn.Image, sn.ExitCode, oom, sn.Error, sn.RestartCount, sn.MemLimit, sn.UptimeSec, sn.Summary, string(logs), string(metrics), insp)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const snapCols = `id,ts,kind,container_id,container,app,image,exit_code,oom_killed,error,restart_count,mem_limit,uptime_sec,summary`

func scanSnap(sc interface{ Scan(...any) error }, extra ...any) (Snapshot, error) {
	var sn Snapshot
	var oom int
	dest := append([]any{&sn.ID, &sn.TS, &sn.Kind, &sn.ContainerID, &sn.Container, &sn.App, &sn.Image, &sn.ExitCode, &oom, &sn.Error, &sn.RestartCount, &sn.MemLimit, &sn.UptimeSec, &sn.Summary}, extra...)
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

// ---- retention ----

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

// EnforceSize drops the oldest logs (then metrics) until live data fits under maxBytes.
func (s *Store) EnforceSize(ctx context.Context, maxBytes int64) (int64, error) {
	var deleted int64
	for i := 0; i < 50 && ctx.Err() == nil; i++ {
		used, err := s.UsedBytes(ctx)
		if err != nil || used <= maxBytes {
			return deleted, err
		}
		trimmed := false
		for _, table := range []string{"logs", "metrics"} {
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
