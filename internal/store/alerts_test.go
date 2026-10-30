package store

import (
	"context"
	"regexp"
	"testing"
	"time"
)

func TestAlertInsertListAndLastTS(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if last := s.LastAlertTS(ctx, "r1", "app"); last != 0 {
		t.Fatalf("empty store last alert = %d", last)
	}

	id1, err := s.InsertAlert(Alert{TS: now - 1000, Rule: "r1", App: "app", Kind: "deadman", Message: "m1"})
	if err != nil || id1 == 0 {
		t.Fatalf("insert1: id=%d err=%v", id1, err)
	}
	id2, err := s.InsertAlert(Alert{TS: now, Rule: "r1", App: "app", Kind: "deadman", Message: "m2"})
	if err != nil || id2 <= id1 {
		t.Fatalf("insert2: id=%d err=%v", id2, err)
	}
	if _, err := s.InsertAlert(Alert{TS: now, Rule: "r2", App: "other", Kind: "metric", Message: "m3"}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListAlerts(ctx, "app", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("app list = %d, want 2", len(list))
	}
	if list[0].Message != "m2" || list[1].Message != "m1" {
		t.Errorf("order: %+v", list)
	}

	all, err := s.ListAlerts(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all list = %d, want 3", len(all))
	}

	if last := s.LastAlertTS(ctx, "r1", "app"); last != now {
		t.Errorf("last alert ts = %d, want %d", last, now)
	}
	if last := s.LastAlertTS(ctx, "r1", "nosuch"); last != 0 {
		t.Errorf("unknown pair last = %d, want 0", last)
	}
}

func TestListAlertsLimitClamped(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.InsertAlert(Alert{TS: int64(i), Rule: "r", App: "a", Kind: "k", Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListAlerts(ctx, "", 0) // 0 => default 200, no error
	if err != nil || len(list) != 5 {
		t.Fatalf("list = %d err=%v", len(list), err)
	}
	list, err = s.ListAlerts(ctx, "", 2) // explicit small limit respected
	if err != nil || len(list) != 2 {
		t.Fatalf("limit=2 -> %d err=%v", len(list), err)
	}
}

func TestLastLogTS(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if ts := s.LastLogTS(ctx, "app"); ts != 0 {
		t.Fatalf("empty last log ts = %d", ts)
	}
	now := time.Now()
	if err := s.InsertLogs([]LogRow{
		row("app", "a", "info", now.Add(-time.Minute)),
		row("app", "b", "info", now),
	}); err != nil {
		t.Fatal(err)
	}
	if ts := s.LastLogTS(ctx, "app"); ts != now.UnixMilli() {
		t.Errorf("last log ts = %d, want %d", ts, now.UnixMilli())
	}
	if ts := s.LastLogTS(ctx, "nosuch"); ts != 0 {
		t.Errorf("nosuch last log ts = %d, want 0", ts)
	}
}

func TestCountAppDeathsSince(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for _, ms := range []int64{now - 60_000, now - 30_000, now - 5_000} {
		if _, err := s.InsertSnapshot(Snapshot{TS: ms, DieTS: ms, Kind: "crash", ContainerID: "c1", Container: "app.1", App: "app", ExitCode: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// One more snapshot with no die_ts (older row, unknown death time) is not counted.
	if _, err := s.InsertSnapshot(Snapshot{TS: now, Kind: "crash", ContainerID: "c2", Container: "app.2", App: "app", ExitCode: 1}); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountAppDeathsSince(ctx, "app", now-45_000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("deaths since 45s = %d, want 2", n)
	}
	n, _ = s.CountAppDeathsSince(ctx, "app", now-3600_000)
	if n != 3 {
		t.Errorf("deaths since 1h = %d, want 3 (die_ts only)", n)
	}
	n, _ = s.CountAppDeathsSince(ctx, "nosuch", 0)
	if n != 0 {
		t.Errorf("nosuch deaths = %d, want 0", n)
	}
}

func TestCountLogsMatching(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertLogs([]LogRow{
		row("app", "status=500 broken", "error", now.Add(-10*time.Minute)),
		row("app", "status=503 broken", "error", now.Add(-time.Minute)),
		row("app", "status=200 ok", "info", now),
	}); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`status=5\d\d`)
	n, err := s.CountLogsMatching(ctx, "app", re, now.Add(-15*time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("matches in 15m = %d, want 2", n)
	}
	// Window excludes the older 500.
	n, _ = s.CountLogsMatching(ctx, "app", re, now.Add(-90*time.Second).UnixMilli())
	if n != 1 {
		t.Errorf("matches in 90s = %d, want 1", n)
	}
	// Only the 200 line is inside 30s; it does not match.
	n, _ = s.CountLogsMatching(ctx, "app", re, now.Add(-30*time.Second).UnixMilli())
	if n != 0 {
		t.Errorf("matches in 30s = %d, want 0", n)
	}
	n, _ = s.CountLogsMatching(ctx, "nosuch", re, 0)
	if n != 0 {
		t.Errorf("nosuch matches = %d, want 0", n)
	}
}

func TestMetricSamples(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	pts := []MetricPoint{
		{TS: now - 3000, CPU: 10, Mem: 100, MemLimit: 1000},
		{TS: now - 2000, CPU: 20, Mem: 200, MemLimit: 1000},
		{TS: now - 1000, CPU: 30, Mem: 300, MemLimit: 1000},
	}
	for _, p := range pts {
		if err := s.InsertMetric("c1", "app", p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertMetric("c2", "other", MetricPoint{TS: now, CPU: 99}); err != nil {
		t.Fatal(err)
	}

	got, err := s.MetricSamples(ctx, "app", now-5000, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("samples = %d, want 3", len(got))
	}
	if got[0].CPU != 10 || got[2].CPU != 30 {
		t.Errorf("order: first=%v last=%v", got[0], got[2])
	}
	// Window filters.
	got, _ = s.MetricSamples(ctx, "app", now-1500, now, 100)
	if len(got) != 1 || got[0].CPU != 30 {
		t.Errorf("window samples = %+v", got)
	}
	got, _ = s.MetricSamples(ctx, "nosuch", 0, 0, 100)
	if len(got) != 0 {
		t.Errorf("nosuch samples = %d", len(got))
	}
}
