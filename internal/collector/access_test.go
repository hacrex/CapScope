package collector

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
)

// fakeDockerAccess simulates two containers:
//   - c-ac: /captain-nginx, excluded by regex, streams access-log lines
//     (combined format). It must be tracked access-only: no log rows land in
//     the logs table, only parsed rows in the access table.
//   - c-app: /shop.1.x, an application container that emits access-log lines
//     too (auto-detection should switch on after the probe window) plus plain
//     app lines that stay in the normal logs table.
func fakeDockerAccess(t *testing.T) *httptest.Server {
	const accLine = `203.0.113.42 - - [29/Oct/2026:10:12:03 +0000] "GET /api/items HTTP/1.1" 200 1234 "-" "curl/8"`
	const appLine = "INFO shop is up"
	logsFor := func(id string, lines ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Distinct timestamps per line: the collector dedupes by
			// timestamp across reconnects, so a tight-loop time.Now()
			// (same nanosecond on Windows) would drop everything but
			// the first line.
			base := time.Now().Add(-time.Minute)
			for i, s := range lines {
				ts := base.Add(time.Duration(i) * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
				w.Write(mux(1, ts+" "+s+"\n"))
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}
	insp := func() map[string]any { // both containers alive for the whole test
		return map[string]any{
			"State": map[string]any{"Status": "running", "Running": true},
		}
	}
	inspect := func(id string) map[string]any {
		base := map[string]any{
			"Id": id, "RestartCount": 0,
			"Config": map[string]any{"Tty": false, "Image": "img"},
		}
		switch id {
		case "c-ac":
			base["Name"] = "/captain-nginx"
			base["Config"].(map[string]any)["Labels"] = map[string]string{}
		case "c-app":
			base["Name"] = "/shop.1.x"
			base["Config"].(map[string]any)["Labels"] = map[string]string{
				"com.docker.swarm.service.name": "srv-captain--shop",
				"com.docker.swarm.task.name":    "srv-captain--shop.1.x",
			}
		}
		for k, v := range insp() {
			base[k] = v
		}
		return base
	}
	mx := http.NewServeMux()
	mx.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "OK") })
	mx.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "c-ac", "Names": []string{"/captain-nginx"}},
			{"Id": "c-app", "Names": []string{"/shop.1.x"}},
		})
	})
	mx.HandleFunc("/containers/c-ac/json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(inspect("c-ac")) })
	mx.HandleFunc("/containers/c-app/json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(inspect("c-app")) })
	mx.HandleFunc("/containers/c-ac/logs", logsFor("c-ac", repeat(accLine, 30)...))
	mx.HandleFunc("/containers/c-app/logs", logsFor("c-app", append([]string{appLine}, repeat(accLine, 30)...)...))
	mx.HandleFunc("/containers/c-ac/stats", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	mx.HandleFunc("/containers/c-app/stats", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	mx.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fl.Flush()
		<-r.Context().Done()
	})
	return httptest.NewServer(mx)
}

func repeat(s string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s)
	}
	return out
}

func TestAccessDetectionAndPipeline(t *testing.T) {
	srv := fakeDockerAccess(t)
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "acc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dk, _ := docker.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	cfg := config.Config{
		StatsInterval: time.Second, InitialTail: 100, MaxLineBytes: 16384,
		AccessLog: "auto", AnonIPS: true,
		SnapshotKinds: map[string]bool{},
		Exclude:       regexp.MustCompile("captain-nginx"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(cfg, dk, st, hub.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	// Wait until the probe window (>= 20 lines for the app container) has
	// passed and the access writer flushed.
	deadline := time.Now().Add(10 * time.Second)
	var agg store.AccessAggregates
	for time.Now().Before(deadline) {
		agg, err = st.QueryAccess(context.Background(), "", time.Now().Add(-time.Hour).UnixMilli(), 0, 60000, 20)
		if err == nil && agg.Totals.Requests >= 31 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if agg.Totals.Requests < 31 {
		t.Fatalf("access rows = %d, want >= 31 (30 proxy + >=1 auto-detected)", agg.Totals.Requests)
	}
	// both sources: proxy app "captain-nginx" and auto-detected app "shop"
	rows, err := st.QueryAccessRows(context.Background(), "captain-nginx", 0, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 30 {
		t.Fatalf("proxy rows = %d, want 30", len(rows))
	}
	if rows[0].IP != "203.0.113.0" {
		t.Errorf("anonymized ip = %q, want 203.0.113.0", rows[0].IP)
	}
	appRows, _ := st.QueryAccessRows(context.Background(), "shop", 0, 0, 100000)
	if len(appRows) < 1 {
		t.Fatalf("auto-detected access rows = %d, want >= 1", len(appRows))
	}
	if appRows[0].IP != "203.0.113.0" {
		t.Errorf("auto-detected anonymized ip = %q", appRows[0].IP)
	}

	// The excluded container's lines must not enter the logs table, and the
	// app container's non-access lines must be there despite the probe.
	time.Sleep(300 * time.Millisecond)
	appLogs, _ := st.QueryLogs(context.Background(), store.LogQuery{App: "shop"})
	if len(appLogs) == 0 || !strings.Contains(appLogs[0].Line, "shop is up") {
		t.Errorf("app logs missing: %+v", appLogs)
	}
	proxyLogs, _ := st.QueryLogs(context.Background(), store.LogQuery{App: "captain-nginx"})
	if len(proxyLogs) != 0 {
		t.Errorf("access-only container leaked %d rows into the logs table", len(proxyLogs))
	}
	// and it must not appear in the sidebar sources
	apps, _ := st.AppSummaries(context.Background())
	for _, a := range apps {
		if a.App == "captain-nginx" {
			t.Error("access-only container appears in the apps list")
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("manager did not shut down")
	}
}
