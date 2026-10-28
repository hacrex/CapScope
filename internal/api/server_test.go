package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
)

func setup(t *testing.T) (*httptest.Server, *store.Store, *hub.Hub) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	cfg := config.Config{AuthUser: "u", AuthPass: "p", SnapshotKinds: map[string]bool{}}
	dk, _ := docker.New("tcp://127.0.0.1:1")
	mgr := collector.NewManager(cfg, dk, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(New(cfg, st, mgr, h, slog.Default()).Handler())
	t.Cleanup(srv.Close)
	return srv, st, h
}

func get(t *testing.T, url string, auth bool) *http.Response {
	req, _ := http.NewRequest("GET", url, nil)
	if auth {
		req.SetBasicAuth("u", "p")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthAndUI(t *testing.T) {
	srv, _, _ := setup(t)
	if r := get(t, srv.URL+"/api/logs", false); r.StatusCode != 401 {
		t.Errorf("want 401 without credentials, got %d", r.StatusCode)
	}
	if r := get(t, srv.URL+"/healthz", false); r.StatusCode != 200 {
		t.Errorf("healthz should be open, got %d", r.StatusCode)
	}
	r := get(t, srv.URL+"/", true)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "CapScope") {
		t.Errorf("UI not served: %d", r.StatusCode)
	}
}

func TestLogsAPIAndSnapshotExport(t *testing.T) {
	srv, st, _ := setup(t)
	now := time.Now()
	_ = st.InsertLogs([]store.LogRow{
		{TS: now.UnixNano(), TSMs: now.UnixMilli(), ContainerID: "c1", Container: "web.1", App: "web", Stream: "o", Level: "info", Line: "hello"},
		{TS: now.UnixNano(), TSMs: now.UnixMilli(), ContainerID: "c1", Container: "web.1", App: "web", Stream: "e", Level: "error", Line: "bad thing"},
	})
	var out struct{ Logs []store.LogRow }
	resp := get(t, srv.URL+"/api/logs?app=web&level=error", true)
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Logs) != 1 || out.Logs[0].Line != "bad thing" {
		t.Fatalf("logs: %+v", out.Logs)
	}
	if r := get(t, srv.URL+"/api/logs?q=(&regex=1", true); r.StatusCode != 400 {
		t.Errorf("bad regex should be 400, got %d", r.StatusCode)
	}
	id, _ := st.InsertSnapshot(store.Snapshot{TS: now.UnixMilli(), Kind: "crash", App: "web", Container: "web.1", ExitCode: 1, Summary: "s",
		Logs: []store.LogRow{{TSMs: now.UnixMilli(), Line: "last line", Stream: "e"}}})
	r := get(t, srv.URL+"/api/snapshots/"+itoa(id)+"/export", true)
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "# Crash report: web") || !strings.Contains(string(b), "last line") {
		t.Errorf("markdown export: %s", b)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSQLAPI(t *testing.T) {
	srv, st, _ := setup(t)
	now := time.Now()
	_ = st.InsertLogs([]store.LogRow{
		{TS: now.UnixNano(), TSMs: now.UnixMilli(), ContainerID: "c1", Container: "web.1", App: "web", Stream: "o", Level: "info", Line: "hello"},
		{TS: now.UnixNano(), TSMs: now.UnixMilli(), ContainerID: "c1", Container: "web.1", App: "web", Stream: "e", Level: "error", Line: "bad thing"},
	})
	q := url.QueryEscape("SELECT level, count(*) AS n FROM logs GROUP BY level ORDER BY level")
	var out struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		Truncated bool     `json:"truncated"`
		MS        int64    `json:"ms"`
	}
	resp := get(t, srv.URL+"/api/sql?query="+q, true)
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || len(out.Columns) != 2 || out.Columns[0] != "level" || len(out.Rows) != 2 {
		t.Fatalf("sql: %d %+v", resp.StatusCode, out)
	}
	if r := get(t, srv.URL+"/api/sql?query="+url.QueryEscape("DELETE FROM logs"), true); r.StatusCode != 400 {
		t.Errorf("write statement should be 400, got %d", r.StatusCode)
	}
	if r := get(t, srv.URL+"/api/sql?query=SELECT%201", false); r.StatusCode != 401 {
		t.Errorf("sql should require auth, got %d", r.StatusCode)
	}
	// The rejected write must not have run.
	var still struct{ Rows [][]any }
	resp = get(t, srv.URL+"/api/sql?query="+url.QueryEscape("SELECT count(*) FROM logs"), true)
	_ = json.NewDecoder(resp.Body).Decode(&still)
	if len(still.Rows) != 1 || still.Rows[0][0].(float64) != 2 {
		t.Fatalf("rows after rejected write: %+v", still.Rows)
	}
}

func TestSSEDeliversPublishedRows(t *testing.T) {
	srv, _, h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/logs/stream?app=web", nil)
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Scan() // ": connected"
	go func() {
		time.Sleep(100 * time.Millisecond)
		h.Publish([]store.LogRow{
			{ID: 1, App: "other", Line: "ignored"},
			{ID: 2, App: "web", Line: "live line"},
		})
	}()
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			if !strings.Contains(sc.Text(), "live line") {
				t.Fatalf("wrong row streamed: %s", sc.Text())
			}
			return
		}
	}
	t.Fatal("no SSE data received")
}
