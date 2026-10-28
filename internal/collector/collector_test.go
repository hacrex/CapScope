package collector

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
)

func TestAppName(t *testing.T) {
	app, rep := AppName("srv-captain--shop.2.x9y", map[string]string{
		"com.docker.swarm.service.name": "srv-captain--shop",
		"com.docker.swarm.task.name":    "srv-captain--shop.2.x9y",
	})
	if app != "shop" || rep != "2" {
		t.Errorf("swarm labels: %q %q", app, rep)
	}
	app, rep = AppName("/srv-captain--blog.1.abc", nil)
	if app != "blog" || rep != "1" {
		t.Errorf("name fallback: %q %q", app, rep)
	}
	if app, _ = AppName("/redis", nil); app != "redis" {
		t.Errorf("plain: %q", app)
	}
}

func TestClassifyAndSummary(t *testing.T) {
	cases := []struct {
		exit int
		oom  bool
		sig  string
		want string
	}{{137, true, "", "oom"}, {0, false, "", "clean_exit"}, {143, false, "15", "killed"}, {1, false, "", "crash"}, {137, false, "", "crash"}}
	for _, c := range cases {
		if got := classify(c.exit, c.oom, c.sig); got != c.want {
			t.Errorf("classify(%d,%v,%q)=%s want %s", c.exit, c.oom, c.sig, got, c.want)
		}
	}
	s := summarize("oom", 137, 500<<20, 512<<20, 2, "boom")
	if !strings.Contains(s, "OOM") || !strings.Contains(s, "500.0 MiB") || !strings.Contains(s, "boom") {
		t.Errorf("summary: %s", s)
	}
}

func mux(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

// fakeDocker simulates one container that logs, reports stats, hits the OOM
// killer after ~400ms, and then dies with exit code 137.
func fakeDocker(t *testing.T) *httptest.Server {
	var dead atomic.Bool
	die := make(chan struct{})
	inspect := func() map[string]any {
		st := map[string]any{"Status": "running", "Running": true, "StartedAt": "2026-10-28T08:00:00Z"}
		if dead.Load() {
			st = map[string]any{"Status": "exited", "Running": false, "OOMKilled": true, "ExitCode": 137,
				"StartedAt": "2026-10-28T08:00:00Z", "FinishedAt": "2026-10-28T08:10:00Z"}
		}
		return map[string]any{
			"Id": "c1deadbeef", "Name": "/srv-captain--demo.1.abc", "RestartCount": 3,
			"Config":     map[string]any{"Image": "demo:latest", "Tty": false, "Labels": map[string]string{"com.docker.swarm.service.name": "srv-captain--demo", "com.docker.swarm.task.name": "srv-captain--demo.1.abc"}},
			"State":      st,
			"HostConfig": map[string]any{"Memory": 536870912, "RestartPolicy": map[string]any{"Name": "on-failure"}},
		}
	}
	mx := http.NewServeMux()
	mx.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "OK") })
	mx.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"Id": "c1deadbeef", "Names": []string{"/srv-captain--demo.1.abc"}}})
	})
	mx.HandleFunc("/containers/c1deadbeef/json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(inspect()) })
	mx.HandleFunc("/containers/c1deadbeef/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Write(mux(1, "2026-10-28T08:09:58.000000001Z INFO listening on :3000\n"))
		w.Write(mux(1, "2026-10-28T08:09:59.000000001Z calling api with password=hunter2hunter2\n"))
		w.Write(mux(2, "2026-10-28T08:09:59.500000001Z ERROR allocation failed: out of memory\n"))
		w.(http.Flusher).Flush()
		select {
		case <-die:
		case <-r.Context().Done():
		}
	})
	mx.HandleFunc("/containers/c1deadbeef/stats", func(w http.ResponseWriter, r *http.Request) {
		for i := 1; i <= 100; i++ {
			fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d},"memory_stats":{"usage":%d,"limit":536870912}}`+"\n",
				i*2000, i*10000, (i-1)*2000+1, (i-1)*10000+1, i*5<<20)
			w.(http.Flusher).Flush()
			select {
			case <-time.After(30 * time.Millisecond):
			case <-die:
				return
			case <-r.Context().Done():
				return
			}
		}
	})
	mx.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fl.Flush()
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		ev := func(action string, attrs map[string]string) {
			b, _ := json.Marshal(map[string]any{"Type": "container", "Action": action, "timeNano": time.Now().UnixNano(),
				"Actor": map[string]any{"ID": "c1deadbeef", "Attributes": attrs}})
			w.Write(append(b, '\n'))
			fl.Flush()
		}
		ev("oom", nil)
		dead.Store(true)
		ev("die", map[string]string{"exitCode": "137"})
		close(die)
		<-r.Context().Done()
	})
	return httptest.NewServer(mx)
}

func TestCrashSnapshotEndToEnd(t *testing.T) {
	srv := fakeDocker(t)
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dk, _ := docker.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	cfg := config.Config{
		StatsInterval: 20 * time.Millisecond, InitialTail: 100, MaxLineBytes: 16384, Redact: true,
		SnapshotKinds: map[string]bool{"crash": true, "oom": true}, SnapshotLogLines: 200, SnapshotMetricSamples: 60,
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(cfg, dk, st, hub.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	var list []store.Snapshot
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		list, _ = st.ListSnapshots(context.Background(), "", 10)
		if len(list) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(list))
	}
	sn, err := st.GetSnapshot(context.Background(), list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if sn.Kind != "oom" || !sn.OOMKilled || sn.ExitCode != 137 || sn.App != "demo" || sn.RestartCount != 3 {
		t.Errorf("snapshot fields: %+v", sn)
	}
	if sn.UptimeSec != 600 {
		t.Errorf("uptime = %d, want 600", sn.UptimeSec)
	}
	if len(sn.Logs) != 3 {
		t.Fatalf("want 3 log lines in snapshot, got %d", len(sn.Logs))
	}
	joined := fmt.Sprint(sn.Logs)
	if strings.Contains(joined, "hunter2hunter2") {
		t.Error("secret leaked into snapshot logs")
	}
	if sn.Logs[2].Level != "error" || sn.Logs[2].Stream != "e" {
		t.Errorf("stderr error line misclassified: %+v", sn.Logs[2])
	}
	if len(sn.Metrics) == 0 {
		t.Error("snapshot has no metric samples")
	}
	if !strings.Contains(sn.Summary, "OOM") {
		t.Errorf("summary: %s", sn.Summary)
	}
	// logs were also persisted for normal querying
	time.Sleep(400 * time.Millisecond) // writer flush interval
	rows, _ := st.QueryLogs(context.Background(), store.LogQuery{App: "demo"})
	if len(rows) != 3 {
		t.Errorf("persisted rows = %d, want 3", len(rows))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("manager did not shut down")
	}
}
