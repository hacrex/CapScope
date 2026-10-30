package store

import (
	"context"
	"testing"
	"time"
)

func TestPrunePerApp(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	// web has old rows; api has old rows too.
	if err := s.InsertLogs([]LogRow{
		row("web", "old", "info", now.Add(-48*time.Hour)),
		row("web", "new", "info", now.Add(-1*time.Hour)),
		row("api", "old", "info", now.Add(-48*time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}

	// Only web has a 24h per-app retention; api keeps the global (7d).
	n, err := s.PrunePerApp(ctx, map[string]time.Duration{"web": 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1 (web's old row)", n)
	}

	if got := countLogs(t, s, "web"); got != 1 {
		t.Errorf("web rows = %d, want 1", got)
	}
	if got := countLogs(t, s, "api"); got != 1 {
		t.Errorf("api rows = %d, want 1 (untouched)", got)
	}
}

func TestPrunePerAppEmpty(t *testing.T) {
	s := open(t)
	n, err := s.PrunePerApp(context.Background(), nil)
	if err != nil || n != 0 {
		t.Fatalf("empty budgets: n=%d err=%v", n, err)
	}
}

func TestAppLogBytes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	if b, err := s.AppLogBytes(ctx, "none"); err != nil || b != 0 {
		t.Fatalf("empty app bytes=%d err=%v", b, err)
	}

	if err := s.InsertLogs([]LogRow{
		row("web", "hello", "info", now),
		row("web", "world!", "info", now),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.AppLogBytes(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	// 5 + 6 chars + 2 rows * overhead.
	want := int64(5 + 6 + 2*rowOverhead)
	if b != want {
		t.Errorf("bytes = %d, want %d", b, want)
	}
}

func TestEnforcePerAppSize(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	// web: 20 rows of ~100 chars. api: 2 rows, should be untouched.
	var rows []LogRow
	line := string(make([]byte, 100))
	for i := 0; i < 20; i++ {
		rows = append(rows, row("web", line, "info", now.Add(time.Duration(i)*time.Second)))
	}
	rows = append(rows, row("api", "small", "info", now))
	rows = append(rows, row("api", "small", "info", now))
	if err := s.InsertLogs(rows); err != nil {
		t.Fatal(err)
	}

	// Budget fits ~5 web rows: 5*(100+270) = 1850.
	budgets := map[string]int64{"web": 1850}
	d, err := s.EnforcePerAppSize(ctx, budgets)
	if err != nil {
		t.Fatal(err)
	}
	if d == 0 {
		t.Fatal("expected some web rows deleted")
	}
	remaining := countLogs(t, s, "web")
	if remaining >= 20 {
		t.Errorf("web rows = %d, want fewer than 20", remaining)
	}
	// api is under its (unset) budget and untouched.
	if got := countLogs(t, s, "api"); got != 2 {
		t.Errorf("api rows = %d, want 2", got)
	}
	// Remaining web bytes now fit the budget.
	if used, err := s.AppLogBytes(ctx, "web"); err != nil {
		t.Fatal(err)
	} else if used > budgets["web"] {
		t.Errorf("web still over budget: %d > %d", used, budgets["web"])
	}
}

func TestEnforcePerAppSizeZeroBudgetSkipped(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.InsertLogs([]LogRow{row("web", "x", "info", time.Now())}); err != nil {
		t.Fatal(err)
	}
	d, err := s.EnforcePerAppSize(ctx, map[string]int64{"web": 0})
	if err != nil || d != 0 {
		t.Fatalf("zero budget should be skipped: d=%d err=%v", d, err)
	}
	if got := countLogs(t, s, "web"); got != 1 {
		t.Errorf("web rows = %d, want 1", got)
	}
}

func countLogs(t *testing.T, s *Store, app string) int {
	t.Helper()
	var n int
	if err := s.r.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM logs WHERE app=?`, app).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
