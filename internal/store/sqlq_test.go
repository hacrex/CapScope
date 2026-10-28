package store

import (
	"context"
	"testing"
	"time"
)

func TestQuerySQLSelect(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertLogs([]LogRow{
		row("web", "started", "info", now),
		row("web", "ERROR db down", "error", now.Add(time.Second)),
		row("api", "other app", "info", now.Add(2*time.Second)),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.QuerySQL(ctx, "SELECT app, count(*) AS n FROM logs GROUP BY app ORDER BY app", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Columns) != 2 || got.Columns[1] != "n" {
		t.Fatalf("columns: %+v", got.Columns)
	}
	if len(got.Rows) != 2 || got.Rows[0][0] != "api" || got.Rows[1][0] != "web" {
		t.Fatalf("rows: %+v", got.Rows)
	}
	if n, ok := got.Rows[1][1].(int64); !ok || n != 2 {
		t.Fatalf("count type/value: %#v", got.Rows[1][1])
	}
	if got.Truncated {
		t.Fatal("not truncated")
	}

	// WITH ... SELECT is allowed.
	if _, err := s.QuerySQL(ctx, "WITH e AS (SELECT * FROM logs WHERE level='error') SELECT count(*) FROM e;", 0); err != nil {
		t.Fatalf("with-select: %v", err)
	}

	// Row cap reports truncation.
	got, err = s.QuerySQL(ctx, "SELECT id FROM logs", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || !got.Truncated {
		t.Fatalf("truncation: %d rows, truncated=%v", len(got.Rows), got.Truncated)
	}

	// A keyword inside a string literal is not a write.
	if _, err := s.QuerySQL(ctx, "SELECT 'delete me' AS note", 0); err != nil {
		t.Fatalf("literal with keyword: %v", err)
	}
	// A trailing semicolon is fine.
	if _, err := s.QuerySQL(ctx, "SELECT 1;", 0); err != nil {
		t.Fatalf("trailing semicolon: %v", err)
	}
	// Trailing semicolon peeking out of a trailing comment is fine too.
	if _, err := s.QuerySQL(ctx, "SELECT count(*) FROM logs LIMIT 1; -- quick check", 0); err != nil {
		t.Fatalf("semicolon before trailing comment: %v", err)
	}
	// Scalar functions with tricky names are allowed.
	if _, err := s.QuerySQL(ctx, "SELECT replace(level, 'a', 'b') FROM logs LIMIT 1", 0); err != nil {
		t.Fatalf("replace() function: %v", err)
	}
}

func TestQuerySQLRejectsWrites(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.InsertLogs([]LogRow{row("web", "keep me", "info", time.Now())}); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"",
		"   ",
		"DELETE FROM logs",
		"UPDATE logs SET line='x'",
		"INSERT INTO logs(ts, container_id, container, app, stream, line) VALUES(1,2,3,4,5,6)",
		"DROP TABLE logs",
		"ALTER TABLE logs RENAME TO logs2",
		"PRAGMA journal_mode=DELETE",
		"ATTACH DATABASE '/tmp/x.db' AS x",
		"SELECT 1; DROP TABLE logs",
		"WITH x AS (SELECT 1) DELETE FROM logs",
		"SELECT load_extension('/x')",
		"VACUUM",
		"BEGIN",
	}
	for _, q := range bad {
		if _, err := s.QuerySQL(ctx, q, 0); err == nil {
			t.Errorf("expected rejection: %q", q)
		}
	}

	// Nothing was harmed.
	got, err := s.QuerySQL(ctx, "SELECT line FROM logs", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 || got.Rows[0][0] != "keep me" {
		t.Fatalf("table damaged: %+v", got.Rows)
	}
}
