package store

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func open(t testing.TB) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func row(app, line, level string, ts time.Time) LogRow {
	return LogRow{TS: ts.UnixNano(), TSMs: ts.UnixMilli(), ContainerID: "c1abc", Container: app + ".1", App: app, Stream: "o", Level: level, Line: line}
}

func TestLogsInsertQueryFilters(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	rows := []LogRow{
		row("web", "started", "info", now),
		row("web", "ERROR db down", "error", now.Add(time.Second)),
		row("web", "100% done_ok", "info", now.Add(2*time.Second)),
		row("api", "other app", "info", now.Add(3*time.Second)),
	}
	if err := s.InsertLogs(rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ID == 0 || rows[3].ID <= rows[0].ID {
		t.Fatalf("ids not assigned: %+v", rows)
	}
	got, _ := s.QueryLogs(ctx, LogQuery{App: "web"})
	if len(got) != 3 || got[0].Line != "started" {
		t.Fatalf("app filter / order wrong: %+v", got)
	}
	got, _ = s.QueryLogs(ctx, LogQuery{App: "web", Levels: []string{"error"}})
	if len(got) != 1 {
		t.Fatalf("level filter: %+v", got)
	}
	got, _ = s.QueryLogs(ctx, LogQuery{Q: "100% done_"}) // LIKE wildcards must be escaped
	if len(got) != 1 {
		t.Fatalf("substring with wildcards: %+v", got)
	}
	got, _ = s.QueryLogs(ctx, LogQuery{Regex: regexp.MustCompile(`(?i)db\s+DOWN`)})
	if len(got) != 1 || got[0].Level != "error" {
		t.Fatalf("regex: %+v", got)
	}
	got, _ = s.QueryLogs(ctx, LogQuery{AfterID: rows[1].ID})
	if len(got) != 2 || got[0].ID != rows[2].ID {
		t.Fatalf("after_id: %+v", got)
	}
	got, _ = s.QueryLogs(ctx, LogQuery{Limit: 2})
	if len(got) != 2 || got[1].Line != "other app" {
		t.Fatalf("limit returns newest: %+v", got)
	}
}

func TestFTSSearch(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	rows := []LogRow{
		row("web", "connected to database primary", "info", now),
		row("web", "ERROR db down", "error", now.Add(time.Second)),
		row("web", "normal line", "info", now.Add(2*time.Second)),
		row("api", "database replica lagging", "warn", now.Add(3*time.Second)),
	}
	if err := s.InsertLogs(rows); err != nil {
		t.Fatal(err)
	}
	// token match across words
	got, err := s.QueryLogs(ctx, LogQuery{Q: "db down"})
	if err != nil || len(got) != 1 || got[0].Level != "error" {
		t.Fatalf("token match: got=%+v err=%v", got, err)
	}
	// prefix match (as-you-type): "dat" must find "database"
	got, err = s.QueryLogs(ctx, LogQuery{Q: "dat"})
	if err != nil || len(got) != 2 {
		t.Fatalf("prefix match: got=%+v err=%v", got, err)
	}
	// combined with app filter
	got, err = s.QueryLogs(ctx, LogQuery{Q: "database", App: "api"})
	if err != nil || len(got) != 1 || got[0].App != "api" {
		t.Fatalf("fts + app filter: got=%+v err=%v", got, err)
	}
	// LIKE fallback: mid-word substring FTS cannot see
	got, err = s.QueryLogs(ctx, LogQuery{Q: "abase prim"})
	if err != nil || len(got) != 1 {
		t.Fatalf("like fallback: got=%+v err=%v", got, err)
	}
	// quoted user input must not break the MATCH parser
	got, err = s.QueryLogs(ctx, LogQuery{Q: `we"ird`})
	if err != nil || len(got) != 0 {
		t.Fatalf("quoted input: got=%+v err=%v", got, err)
	}
	// retention deletes must keep the index in sync
	if _, err := s.Prune(ctx, time.Nanosecond, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err = s.QueryLogs(ctx, LogQuery{Q: "database"})
	if err != nil || len(got) != 0 {
		t.Fatalf("search after prune: got=%+v err=%v", got, err)
	}
}

func TestFTSQuerySanitized(t *testing.T) {
	cases := map[string]string{
		"dat":        `"dat"*`,
		"db down":    `"db"* AND "down"*`,
		`say "hi"`:   `"say"* AND """hi"""*`,
		"   ":        "",
		`""`:         `""""""*`,
		"error code": `"error"* AND "code"*`,
	}
	for in, want := range cases {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, err := s.InsertSnapshot(Snapshot{
		TS: time.Now().UnixMilli(), Kind: "oom", ContainerID: "c1", Container: "web.1", App: "web", Image: "img",
		ExitCode: 137, OOMKilled: true, Summary: "x",
		Logs:    []LogRow{{Line: "last words", Level: "error"}},
		Metrics: []MetricPoint{{TS: 1, CPU: 1.5, Mem: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := s.GetSnapshot(ctx, id)
	if err != nil || !sn.OOMKilled || len(sn.Logs) != 1 || sn.Logs[0].Line != "last words" || len(sn.Metrics) != 1 {
		t.Fatalf("round trip failed: %+v err=%v", sn, err)
	}
	list, _ := s.ListSnapshots(ctx, "web", 10)
	if len(list) != 1 || len(list[0].Logs) != 0 {
		t.Fatalf("list should be light: %+v", list)
	}
}

func TestMetricsBucketAndSum(t *testing.T) {
	s := open(t)
	base := time.Now().Add(-time.Minute).UnixMilli()
	for i := 0; i < 4; i++ {
		ts := base + int64(i)*1000
		_ = s.InsertMetric("c1", "web", MetricPoint{TS: ts, CPU: 10, Mem: 100, MemLimit: 1000})
		_ = s.InsertMetric("c2", "web", MetricPoint{TS: ts, CPU: 20, Mem: 300, MemLimit: 1000})
	}
	pts, err := s.QueryMetrics(context.Background(), "web", "", base-1000, 0, 60_000)
	if err != nil || len(pts) == 0 {
		t.Fatalf("pts=%v err=%v", pts, err)
	}
	var cpu float64
	for _, p := range pts {
		cpu += p.CPU
	}
	if pts[0].Mem != 400 {
		t.Errorf("app memory should sum replicas: %+v", pts)
	}
	_ = cpu
}

func TestPruneAndSizeCap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	var rows []LogRow
	for i := 0; i < 6000; i++ {
		rows = append(rows, row("web", fmt.Sprintf("old line %d", i), "info", old))
	}
	rows = append(rows, row("web", "fresh", "info", time.Now()))
	if err := s.InsertLogs(rows); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, 24*time.Hour, time.Hour, time.Hour)
	if err != nil || n != 6000 {
		t.Fatalf("pruned %d err=%v, want 6000", n, err)
	}
	left, _ := s.QueryLogs(ctx, LogQuery{})
	if len(left) != 1 || left[0].Line != "fresh" {
		t.Fatalf("unexpected leftovers: %+v", left)
	}
	// size cap: fill, then enforce a tiny limit and expect old rows to be dropped
	var big []LogRow
	for i := 0; i < 5000; i++ {
		big = append(big, row("web", fmt.Sprintf("padding %d xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", i), "info", time.Now()))
	}
	_ = s.InsertLogs(big)
	before, _ := s.UsedBytes(ctx)
	if _, err := s.EnforceSize(ctx, before/4); err != nil {
		t.Fatal(err)
	}
	after, _ := s.UsedBytes(ctx)
	if after >= before {
		t.Errorf("size cap did not shrink live data: before=%d after=%d", before, after)
	}
}

func BenchmarkInsertLogs(b *testing.B) {
	s := open(b)
	batch := make([]LogRow, 1000)
	now := time.Now()
	for i := range batch {
		batch[i] = row("web", fmt.Sprintf(`{"level":"info","msg":"request handled","path":"/api/items/%d","ms":12}`, i), "info", now)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.InsertLogs(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*len(batch))/b.Elapsed().Seconds(), "rows/s")
}
