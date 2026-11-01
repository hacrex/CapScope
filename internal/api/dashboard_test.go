package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
)

// setupNoAuth builds a server without auth so tests can hit the new v0.7
// endpoints directly. Returns the store for seeding.
func setupNoAuth(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	cfg := config.Config{SnapshotKinds: map[string]bool{}}
	dk, _ := docker.New("tcp://127.0.0.1:1")
	mgr := collector.NewManager(cfg, dk, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(New(cfg, st, mgr, h, slog.Default()).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestDashboardEndpoint(t *testing.T) {
	srv, st := setupNoAuth(t)
	now := time.Now()

	// AppSummaries is driven by the containers table; seed it first.
	for _, c := range []store.ContainerRow{
		{ID: "aaa1", Name: "app1-1", App: "app1", Image: "img", State: "running"},
		{ID: "bbb1", Name: "app2-1", App: "app2", Image: "img", State: "running"},
	} {
		c.FirstSeen = now.Add(-time.Hour).UnixMilli()
		c.LastSeen = now.UnixMilli()
		if err := st.UpsertContainer(c); err != nil {
			t.Fatal(err)
		}
	}
	// Seed one log row per app so AppSummaries reports them.
	for _, l := range []store.LogRow{
		{TS: now.Add(-time.Minute).UnixNano(), TSMs: now.Add(-time.Minute).UnixMilli(), ContainerID: "aaa1", Container: "app1-1", App: "app1", Stream: "e", Level: "error", Line: "boom"},
		{TS: now.Add(-time.Minute).UnixNano(), TSMs: now.Add(-time.Minute).UnixMilli(), ContainerID: "bbb1", Container: "app2-1", App: "app2", Stream: "o", Level: "info", Line: "hello"},
	} {
		if err := st.InsertLogs([]store.LogRow{l}); err != nil {
			t.Fatal(err)
		}
	}
	// Metrics for sparklines.
	for i := 0; i < 5; i++ {
		if err := st.InsertMetric("aaa1", "app1", store.MetricPoint{TS: now.Add(-time.Duration(i) * time.Minute).UnixMilli(), CPU: 12.5, Mem: 1 << 20}); err != nil {
			t.Fatal(err)
		}
	}
	// A crash-loop snapshot for app1 inside the window.
	if _, err := st.InsertSnapshot(store.Snapshot{
		TS: now.UnixMilli(), DieTS: now.UnixMilli(), Kind: "crash",
		ContainerID: "aaa1", Container: "app1-1", App: "app1", Image: "img",
		RestartsWindow: 4, Summary: "loop",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Get(srv.URL + "/api/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Stats struct {
			Restarts24h  int64 `json:"restarts_24h"`
			CrashLoops   int   `json:"crash_loops"`
			DBBytes      int64 `json:"db_bytes"`
			Snapshots24h int64 `json:"snapshots_24h"`
		} `json:"stats"`
		Apps []struct {
			App        string `json:"app"`
			Errors1h   int64  `json:"errors_1h"`
			Restarts24 int64  `json:"restarts_24h"`
			CrashLoop  bool   `json:"crash_loop"`
			LastCrash  int64  `json:"last_crash"`
			Spark      []struct {
				CPU float64 `json:"cpu"`
			} `json:"spark"`
		} `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Stats.Restarts24h != 1 || out.Stats.CrashLoops != 1 || out.Stats.Snapshots24h != 1 {
		t.Fatalf("stats = %+v", out.Stats)
	}
	if out.Stats.DBBytes <= 0 {
		t.Fatalf("db_bytes = %d", out.Stats.DBBytes)
	}
	if len(out.Apps) != 2 {
		t.Fatalf("apps = %d", len(out.Apps))
	}
	var a1 bool
	for _, a := range out.Apps {
		switch a.App {
		case "app1":
			a1 = true
			if !a.CrashLoop || a.Restarts24 != 1 || a.LastCrash == 0 || a.Errors1h != 1 {
				t.Fatalf("app1 card = %+v", a)
			}
			if len(a.Spark) == 0 {
				t.Fatalf("app1 spark empty")
			}
		case "app2":
			if a.CrashLoop || a.Restarts24 != 0 {
				t.Fatalf("app2 card = %+v", a)
			}
		}
	}
	if !a1 {
		t.Fatal("app1 missing from cards")
	}
}

func TestCrashLoopsEndpoint(t *testing.T) {
	srv, st := setupNoAuth(t)
	now := time.Now()
	if _, err := st.InsertSnapshot(store.Snapshot{
		TS: now.UnixMilli(), DieTS: now.UnixMilli(), Kind: "crash",
		ContainerID: "ccc1", Container: "loop-1", App: "loopapp", Image: "img",
		RestartsWindow: 5, Summary: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertSnapshot(store.Snapshot{
		TS: now.Add(-2 * time.Hour).UnixMilli(), DieTS: now.Add(-2 * time.Hour).UnixMilli(), Kind: "crash",
		ContainerID: "ddd1", Container: "old-1", App: "oldapp", Image: "img",
		RestartsWindow: 9, Summary: "x", // outside window
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Get(srv.URL + "/api/crashloops")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Loops []struct {
			Container string `json:"container"`
			Deaths    int64  `json:"deaths"`
		} `json:"loops"`
		WindowMs int64 `json:"window_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Loops) != 1 || out.Loops[0].Container != "loop-1" || out.Loops[0].Deaths != 5 {
		t.Fatalf("loops = %+v", out.Loops)
	}
	if out.WindowMs != (15 * time.Minute).Milliseconds() {
		t.Fatalf("window = %d", out.WindowMs)
	}
	// threshold above deaths -> empty list
	resp2, err := srv.Client().Get(srv.URL + "/api/crashloops?threshold=6")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Loops []json.RawMessage `json:"loops"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if len(out2.Loops) != 0 {
		t.Fatalf("expected no loops at threshold 6, got %d", len(out2.Loops))
	}
}

func TestPrometheusMetricsEndpoint(t *testing.T) {
	srv, st := setupNoAuth(t)
	now := time.Now()
	if _, err := st.InsertSnapshot(store.Snapshot{
		TS: now.UnixMilli(), Kind: "oom", ContainerID: "eee1", Container: "web-1",
		App: "webapp", Image: "img", OOMKilled: true, RestartsWindow: 1, Summary: "x",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	b := new(strings.Builder)
	if _, err := io.Copy(b, resp.Body); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	for _, want := range []string{
		"capscope_up 1",
		"capscope_restarts_24h 1",
		`capscope_crashes_total{app="webapp"} 1`,
		`capscope_oom_kills_total{app="webapp"} 1`,
		"capscope_db_size_bytes ",
		"# TYPE capscope_containers gauge",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}
