package store

import (
	"context"
	"testing"
	"time"
)

func seedAccess(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now().UnixMilli()
	rows := []AccessRow{
		{TS: now - 10000, ContainerID: "ng", Container: "captain-nginx", App: "nginx", IP: "203.0.113.7", Method: "GET", Path: "/api/items", Status: 200, Bytes: 1200, Referer: "https://ref.example", UA: "UA/1"},
		{TS: now - 9000, ContainerID: "ng", Container: "captain-nginx", App: "nginx", IP: "203.0.113.7", Method: "GET", Path: "/api/items", Status: 500, Bytes: 300},
		{TS: now - 8000, ContainerID: "ng", Container: "captain-nginx", App: "nginx", IP: "198.51.100.9", Method: "POST", Path: "/login", Status: 403, Bytes: 88},
		{TS: now - 7000, ContainerID: "ng", Container: "captain-nginx", App: "nginx", IP: "-", Method: "GET", Path: "/", Status: 200, Bytes: 10, Referer: "-"},
		{TS: now - 3*24*3600*1000, ContainerID: "ng", Container: "captain-nginx", App: "old", IP: "9.9.9.9", Method: "GET", Path: "/old", Status: 200, Bytes: 5},
		// an app-scoped record alongside the proxy ones
		{TS: now - 5000, ContainerID: "web", Container: "web.1", App: "web", IP: "203.0.113.7", Method: "GET", Path: "/x", Status: 200, Bytes: 20},
	}
	if err := s.InsertAccess(rows); err != nil {
		t.Fatal(err)
	}
}

func TestQueryAccessAggregates(t *testing.T) {
	s := open(t)
	defer s.Close()
	seedAccess(t, s)
	ctx := context.Background()
	agg, err := s.QueryAccess(ctx, "nginx", time.Now().Add(-time.Hour).UnixMilli(), 0, 60000, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 4 requests for app nginx in the window: 2x GET /api/items (200 + 500), /login 403, "/" 200
	if agg.Totals.Requests != 4 {
		t.Errorf("requests = %d, want 4", agg.Totals.Requests)
	}
	if agg.Totals.ClientErr != 1 || agg.Totals.ServerErr != 1 {
		t.Errorf("client/server errors = %d/%d, want 1/1", agg.Totals.ClientErr, agg.Totals.ServerErr)
	}
	if agg.Totals.UniqueIPs != 3 { // two real IPs + "-"
		t.Errorf("unique ips = %d, want 3", agg.Totals.UniqueIPs)
	}
	if agg.Totals.Bytes != 1200+300+88+10 {
		t.Errorf("bytes = %d", agg.Totals.Bytes)
	}
	if len(agg.Codes) != 3 { // 200, 403, 500
		t.Fatalf("codes = %+v", agg.Codes)
	}
	if agg.Codes[0].N != 2 {
		t.Errorf("top code count = %d, want 2", agg.Codes[0].N)
	}
	if len(agg.Paths) != 3 || agg.Paths[0].Key != "/api/items" || agg.Paths[0].Hits != 2 || agg.Paths[0].Errors != 1 {
		t.Errorf("paths = %+v", agg.Paths)
	}
	if len(agg.Refs) != 1 || agg.Refs[0].Key != "https://ref.example" {
		t.Errorf("refs = %+v", agg.Refs)
	}
	var tlHits int64
	for _, p := range agg.Tl {
		tlHits += p.Hits
	}
	if tlHits != 4 {
		t.Errorf("timeline hits = %d, want 4 (buckets %d)", tlHits, len(agg.Tl))
	}

	// app filter keeps the other rows out
	agg, err = s.QueryAccess(ctx, "web", time.Now().Add(-time.Hour).UnixMilli(), 0, 60000, 20)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Totals.Requests != 1 || len(agg.Paths) != 1 || agg.Paths[0].Key != "/x" {
		t.Errorf("web-only aggregates: %+v", agg)
	}
}

func TestAccessRowsAndLimit(t *testing.T) {
	s := open(t)
	defer s.Close()
	seedAccess(t, s)
	ctx := context.Background()
	rows, err := s.QueryAccessRows(ctx, "", time.Now().Add(-time.Hour).UnixMilli(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows)) // the 3-day-old /old row is outside the window
	}
	asc := true
	for i := 1; i < len(rows); i++ {
		if rows[i].TS < rows[i-1].TS {
			asc = false
		}
	}
	if !asc {
		t.Error("rows not oldest first")
	}
	// app filter keeps the other rows out: only the 4 proxy rows carry app "nginx"
	rows, err = s.QueryAccessRows(ctx, "nginx", 0, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Errorf("app-filtered rows = %d, want 4", len(rows))
	}
	// limit clamp: never more than 100000
	rows, err = s.QueryAccessRows(ctx, "", 0, 0, -1)
	if err != nil || len(rows) != 6 {
		t.Fatalf("default limit rows = %d err %v", len(rows), err)
	}
}

func TestAccessRetentionAndSize(t *testing.T) {
	s := open(t)
	defer s.Close()
	seedAccess(t, s)
	ctx := context.Background()
	n, err := s.Prune(ctx, 2*24*time.Hour, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1 (the 3-day-old access row)", n)
	}
	var c int64
	if err := s.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM access").Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 5 {
		t.Fatalf("rows after prune = %d, want 5", c)
	}

	// Size cap: only access rows remain, so the cap must hit the access table.
	if n, err := s.EnforceSize(ctx, 4096); err != nil || n == 0 {
		t.Fatalf("EnforceSize dropped %d (%v), want >0", n, err)
	}
	if err := s.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM access").Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 0 {
		t.Fatalf("access rows after size cap = %d, want 0", c)
	}
}
