package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"capscope/internal/auth"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// wantReclaimable is what the fake daemon's /system/df fixture must total:
// 50M unused image + 40M unreferenced volume + 1M free build cache + 3M
// stopped container.
const wantReclaimable = int64(94000000)

// wantSizeRW is the SizeRw the fake daemon reports for ?size=true.
const wantSizeRW = int64(123456)

// fakeOverviewCtr describes one container of the fake daemon.
type fakeOverviewCtr struct {
	id, name, image string
}

var fakeOverviewCtrs = []fakeOverviewCtr{
	{"aaa111", "srv-captain--shop.1.abc", "shop:1.4"},
	{"bbb222", "srv-captain--blog.1.def", "blog:1.0"},
}

// fakeStatsStream streams deterministic stats frames: every accepted sample
// computes to exactly 20 core-percent (4e6 CPU delta over a 4e7 system delta
// on 2 cores — frame 1 has no baseline and is skipped by the collector),
// memory climbs 1 MiB per frame, and network/block counters advance by fixed
// amounts so all four rates are always positive.
func fakeStatsStream(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/json")
	var cpu, sys, rx, tx, blkR, blkW uint64 = 0, 0, 1e6, 2e6, 0, 0
	for i := 1; ; i++ {
		prevCPU, prevSys := cpu, sys
		cpu += 4e6
		sys += 4e7
		rx += 10000
		tx += 5000
		blkR += 2000
		blkW += 3000
		fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":2},`+
			`"precpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d},`+
			`"memory_stats":{"usage":%d,"limit":536870912,"stats":{"inactive_file":20971520}},`+
			`"networks":{"eth0":{"rx_bytes":%d,"tx_bytes":%d}},`+
			`"blkio_stats":{"io_service_bytes_recursive":[{"op":"read","value":%d},{"op":"write","value":%d}]}}`+"\n",
			cpu, sys, prevCPU, prevSys, uint64(120<<20+i<<20), rx, tx, blkR, blkW)
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-time.After(150 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
}

// fakeOverviewDocker is the two-container daemon the overview tests run
// against: running containers with live stats, inspect?size=true, and the
// /info + /system/df host endpoints.
func fakeOverviewDocker(t *testing.T) *httptest.Server {
	t.Helper()
	mx := http.NewServeMux()
	mx.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "OK") })
	mx.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"Name": "test-host", "NCPU": 4, "MemTotal": int64(17179869184),
			"DockerRootDir": "", "ServerVersion": "28.1.1",
		})
	})
	mx.HandleFunc("/system/df", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"Images": []map[string]any{
				{"Id": "img-used", "Size": int64(100000000), "SharedSize": int64(20000000), "Containers": 1},
				{"Id": "img-unused", "Size": int64(50000000), "SharedSize": int64(0), "Containers": 0},
			},
			"Containers": []map[string]any{
				{"Id": "aaa111", "SizeRw": int64(5000000), "State": "running"},
				{"Id": "ccc333", "SizeRw": int64(3000000), "State": "exited"},
			},
			"Volumes": []map[string]any{
				{"Name": "free", "UsageData": map[string]any{"RefCount": 0, "Size": int64(40000000)}},
				{"Name": "used", "UsageData": map[string]any{"RefCount": 2, "Size": int64(70000000)}},
			},
			"BuildCache": []map[string]any{
				{"ID": "bc-free", "Size": int64(1000000), "InUse": false},
				{"ID": "bc-busy", "Size": int64(999999), "InUse": true},
			},
		})
	})
	mx.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 0, len(fakeOverviewCtrs))
		for _, c := range fakeOverviewCtrs {
			out = append(out, map[string]any{
				"Id": c.id, "Names": []string{"/" + c.name}, "Image": c.image,
				"State": "running", "Labels": map[string]string{},
			})
		}
		json.NewEncoder(w).Encode(out)
	})
	for _, c := range fakeOverviewCtrs {
		c := c
		mx.HandleFunc("/containers/"+c.id+"/json", func(w http.ResponseWriter, r *http.Request) {
			o := map[string]any{
				"Id": c.id, "Name": "/" + c.name, "RestartCount": 0,
				"Config": map[string]any{"Image": c.image, "Tty": false, "Labels": map[string]string{}},
				"State": map[string]any{"Status": "running", "Running": true, "OOMKilled": false, "ExitCode": 0,
					"StartedAt": time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano),
					"FinishedAt": "0001-01-01T00:00:00Z"},
				"HostConfig": map[string]any{"Memory": int64(536870912), "RestartPolicy": map[string]any{"Name": "on-failure"}},
			}
			if r.URL.Query().Get("size") == "true" {
				o["SizeRw"] = wantSizeRW
			}
			json.NewEncoder(w).Encode(o)
		})
		// Block until the client goes away: the collector keeps these streams
		// open for the tracker's lifetime (and httptest.Server.Close waits for
		// outstanding handlers, so the test cleanup cancels the manager first).
		mx.HandleFunc("/containers/"+c.id+"/logs", func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		mx.HandleFunc("/containers/"+c.id+"/stats", fakeStatsStream)
	}
	mx.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	srv := httptest.NewServer(mx)
	t.Cleanup(srv.Close) // registered first, so it runs after the manager stops
	return srv
}

// loginAs obtains a bearer token for an arbitrary seeded user.
func loginAs(t *testing.T, baseURL, user, pass string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", baseURL+"/api/login",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login %s: status %d", user, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		t.Fatalf("login %s: no token", user)
	}
	return out.Token
}

// setupOverview spins the full stack against the fake daemon: a running
// collector manager (trackers with live metric rings) behind an auth-enabled
// API server with WithDocker wired. Returns the server plus tokens for an
// unrestricted admin and a viewer scoped to the shop app only.
func setupOverview(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	dsrv := fakeOverviewDocker(t)
	dk, err := docker.New("tcp://" + strings.TrimPrefix(dsrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "overview.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := hub.New(1024)
	cfg := config.Config{
		AuthUser: "u", AuthPass: "p", SnapshotKinds: map[string]bool{},
		StatsInterval: 100 * time.Millisecond, InitialTail: 10, MaxLineBytes: 4096,
		SnapshotLogLines: 100, SnapshotMetricSamples: 60,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := collector.NewManager(cfg, dk, st, h, logger)

	// Seed the admin plus a viewer restricted to the shop app.
	hash, _ := bcrypt.GenerateFromPassword([]byte("p"), bcrypt.DefaultCost)
	if _, err := st.SeedFirstUser("u", string(hash)); err != nil {
		t.Fatal(err)
	}
	vhash, _ := bcrypt.GenerateFromPassword([]byte("vp"), bcrypt.DefaultCost)
	vid, err := st.CreateUser("viewer", string(vhash), auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceUserApps(vid, []string{"shop"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel() // must precede dsrv.Close: its log/event handlers block until then
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})

	srv := httptest.NewServer(New(cfg, st, mgr, h, slog.Default(),
		WithAuth(auth.NewSessionManager(slog.Default())), WithDocker(dk)).Handler())
	t.Cleanup(srv.Close)
	return srv, loginAs(t, srv.URL, "u", "p"), loginAs(t, srv.URL, "viewer", "vp")
}

// fetchOverview GETs /api/overview and returns the raw body plus its decode.
func fetchOverview(t *testing.T, baseURL, token, query string) ([]byte, overviewResponse) {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/api/overview"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("overview status %d: %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var ov overviewResponse
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatalf("decode overview: %v (%s)", err, body)
	}
	return body, ov
}

// wantKeys fails unless the JSON object holds exactly these keys: the
// overview body is a published contract the frontend is built against.
func wantKeys(t *testing.T, raw []byte, want ...string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %v (%s)", err, raw)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	return m
}

// overviewReady reports whether the async pieces have landed: two metric
// samples per container (rates become non-zero), the writable-layer size
// (measured after its settle delay) and the background /system/df refresh.
func overviewReady(o overviewResponse) bool {
	if len(o.Containers) != 2 || o.Host.NCPU != 4 || o.Host.Reclaimable != wantReclaimable {
		return false
	}
	for _, c := range o.Containers {
		if c.AvgCPU == 0 || c.NetRxRate == 0 || c.SizeRW != wantSizeRW {
			return false
		}
	}
	return true
}

func TestOverviewEndpoint(t *testing.T) {
	srv, adminTok, viewerTok := setupOverview(t)

	var raw []byte
	var ov overviewResponse
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw, ov = fetchOverview(t, srv.URL, adminTok, "")
		if overviewReady(ov) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !overviewReady(ov) {
		t.Fatalf("overview never became ready: %+v", ov)
	}

	t.Run("contract", func(t *testing.T) {
		// Exact key sets at every level — no extras, nothing missing.
		top := wantKeys(t, raw, "containers", "cpu", "host", "hosts", "mem", "window_ms")
		wantKeys(t, top["host"],
			"disk_free", "disk_total", "docker_version", "id", "mem_total", "name", "ncpu", "reclaimable", "uptime_s")
		wantKeys(t, top["cpu"], "avg", "hist", "pk", "value")
		wantKeys(t, top["mem"], "avg", "hist", "pk", "value")
		// Host grid: one card per host with containers in the window. Both
		// test containers report the same host, so exactly one entry, shaped
		// as the host block plus that host's own gauges and container count.
		var hostRows []json.RawMessage
		if err := json.Unmarshal(top["hosts"], &hostRows); err != nil {
			t.Fatalf("hosts: %v (%s)", err, top["hosts"])
		}
		if len(hostRows) != 1 {
			t.Fatalf("hosts = %d, want 1", len(hostRows))
		}
		wantKeys(t, hostRows[0],
			"containers", "cpu", "mem",
			"disk_free", "disk_total", "docker_version", "id", "mem_total", "name", "ncpu", "reclaimable", "uptime_s")
		var hostObj map[string]json.RawMessage
		if err := json.Unmarshal(hostRows[0], &hostObj); err != nil {
			t.Fatal(err)
		}
		wantKeys(t, hostObj["cpu"], "avg", "hist", "pk", "value")
		wantKeys(t, hostObj["mem"], "avg", "hist", "pk", "value")
		var cpuObj map[string]json.RawMessage
		if err := json.Unmarshal(top["cpu"], &cpuObj); err != nil {
			t.Fatal(err)
		}
		wantKeys(t, cpuObj["hist"], "step_ms", "t0", "v")
		var rows []json.RawMessage
		if err := json.Unmarshal(top["containers"], &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("containers = %d, want 2", len(rows))
		}
		const contKeys = "app,avg_cpu,avg_mem,blk_r_rate,blk_w_rate,cpu,cpu_hist,host_id,id,image,mem,mem_limit,name,net_rx_rate,net_tx_rate,replica,size_rw,started"
		wantKeys(t, rows[0], strings.Split(contKeys, ",")...)
		wantKeys(t, rows[1], strings.Split(contKeys, ",")...)

		if ov.WindowMS != 300000 {
			t.Fatalf("window_ms = %d, want 300000", ov.WindowMS)
		}
		h := ov.Host
		if h.ID != "" || h.Name != "test-host" || h.NCPU != 4 || h.MemTotal != 17179869184 ||
			h.DockerVersion != "28.1.1" || h.DiskTotal != 0 || h.DiskFree != 0 ||
			h.Reclaimable != wantReclaimable {
			t.Fatalf("host = %+v", h)
		}
		if h.UptimeS < 0 {
			t.Fatalf("uptime_s = %d", h.UptimeS)
		} // 0 on Windows, real /proc/uptime on Linux: both valid

		// Host CPU: two containers at 20 core-percent over 4 cores = 10%.
		if ov.CPU.Value < 9 || ov.CPU.Value > 11 {
			t.Fatalf("cpu.value = %v, want ~10", ov.CPU.Value)
		}
		if ov.CPU.Pk < 9 || ov.CPU.Pk > 11 || ov.CPU.Avg <= 0 || ov.CPU.Avg > ov.CPU.Pk+1e-9 {
			t.Fatalf("cpu avg/pk = %v/%v", ov.CPU.Avg, ov.CPU.Pk)
		}
		if ov.Mem.Value < 200<<20 || ov.Mem.Avg <= 0 || ov.Mem.Pk <= 0 {
			t.Fatalf("mem = %+v", ov.Mem)
		}

		byApp := map[string]collector.OverviewContainer{}
		for _, c := range ov.Containers {
			byApp[c.App] = c
		}
		shop, ok := byApp["shop"]
		if !ok {
			t.Fatalf("apps = %v", byApp)
		}
		if shop.ID != "aaa111" || shop.Name != "srv-captain--shop.1.abc" || shop.Replica != "1" ||
			shop.Image != "shop:1.4" || shop.HostID != "" || shop.Started == 0 {
			t.Fatalf("shop identity = %+v", shop)
		}
		if shop.CPU < 19 || shop.CPU > 21 || shop.AvgCPU < 19 || shop.AvgCPU > 21 {
			t.Fatalf("shop cpu = %v avg %v, want ~20", shop.CPU, shop.AvgCPU)
		}
		if shop.Mem < 100<<20 || shop.Mem > 300<<20 || shop.MemLimit != 536870912 || shop.AvgMem <= 0 {
			t.Fatalf("shop mem = %d/%d avg %v", shop.Mem, shop.MemLimit, shop.AvgMem)
		}
		if shop.NetRxRate <= 0 || shop.NetTxRate <= 0 || shop.BlkRRate <= 0 || shop.BlkWRate <= 0 {
			t.Fatalf("shop rates = rx %v tx %v r %v w %v", shop.NetRxRate, shop.NetTxRate, shop.BlkRRate, shop.BlkWRate)
		}
		if shop.SizeRW != wantSizeRW {
			t.Fatalf("shop size_rw = %d, want %d", shop.SizeRW, wantSizeRW)
		}
		hist := shop.CPUHist
		if hist.StepMS != 10000 || len(hist.V) == 0 || len(hist.V) > 60 {
			t.Fatalf("shop cpu_hist = %+v", hist)
		}
		if hist.T0 == 0 || hist.T0%hist.StepMS != 0 {
			t.Fatalf("cpu_hist t0 = %d, want a step-aligned timestamp", hist.T0)
		}
		for i, v := range hist.V {
			if v < 19 || v > 21 {
				t.Fatalf("cpu_hist[%d] = %v, want ~20 (all samples are)", i, v)
			}
		}
		if blog := byApp["blog"]; blog.SizeRW != wantSizeRW || blog.CPU < 19 || len(blog.CPUHist.V) == 0 {
			t.Fatalf("blog row = %+v", blog)
		}
	})

	t.Run("viewer-scope", func(t *testing.T) {
		// Same restriction as /api/containers: the viewer sees only shop —
		// including in the host CPU total, which sums visible containers.
		_, vov := fetchOverview(t, srv.URL, viewerTok, "")
		if len(vov.Containers) != 1 || vov.Containers[0].App != "shop" {
			t.Fatalf("viewer containers = %+v", vov.Containers)
		}
		if vov.Host.NCPU != 4 {
			t.Fatalf("viewer host = %+v", vov.Host)
		}
		if vov.CPU.Value < 4 || vov.CPU.Value > 6 {
			t.Fatalf("viewer cpu.value = %v, want ~5 (one of two containers)", vov.CPU.Value)
		}
	})

	t.Run("remote-host", func(t *testing.T) {
		// A host this process cannot probe: containers still filtered by it
		// (none are tracked under that id), host block all zeros but id/name.
		raw, rov := fetchOverview(t, srv.URL, adminTok, "?host=node-9")
		if rov.Host.ID != "node-9" || rov.Host.Name != "node-9" ||
			rov.Host.NCPU != 0 || rov.Host.MemTotal != 0 || rov.Host.DockerVersion != "" ||
			rov.Host.DiskTotal != 0 || rov.Host.DiskFree != 0 || rov.Host.Reclaimable != 0 || rov.Host.UptimeS != 0 {
			t.Fatalf("remote host = %+v", rov.Host)
		}
		if rov.Containers == nil || len(rov.Containers) != 0 {
			t.Fatalf("remote containers = %#v, want empty (non-null) slice", rov.Containers)
		}
		if rov.WindowMS != 300000 || rov.CPU.Value != 0 || rov.Mem.Value != 0 {
			t.Fatalf("remote gauges = %+v / %+v window %d", rov.CPU, rov.Mem, rov.WindowMS)
		}
		// Empty charts must serialize as [] (never null).
		if !strings.Contains(string(raw), `"v":[]`) {
			t.Fatalf("remote body missing empty arrays: %s", raw)
		}
	})

	t.Run("containers-additive", func(t *testing.T) {
		// /api/containers keeps its shape and gains only size_rw.
		req, _ := http.NewRequest("GET", srv.URL+"/api/containers", nil)
		req.Header.Set("Authorization", "Bearer "+adminTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var wrap struct {
			Containers []map[string]json.RawMessage `json:"containers"`
		}
		if err := json.Unmarshal(body, &wrap); err != nil {
			t.Fatalf("decode containers: %v (%s)", err, body)
		}
		if len(wrap.Containers) != 2 {
			t.Fatalf("containers = %d, want 2", len(wrap.Containers))
		}
		wantKeys(t, mustJSON(t, wrap.Containers[0]),
			"app", "cpu", "host_id", "id", "image", "mem", "mem_limit", "name",
			"replica", "sample_ts", "size_rw", "started")
		var size int64
		if err := json.Unmarshal(wrap.Containers[0]["size_rw"], &size); err != nil || size != wantSizeRW {
			t.Fatalf("size_rw = %s (%v), want %d", wrap.Containers[0]["size_rw"], err, wantSizeRW)
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestOverviewEmpty covers the cold path: no Docker wired and no trackers.
// The contract must still hold — empty arrays (never null), a zeroed host
// block and the fixed window.
func TestOverviewEmpty(t *testing.T) {
	// A client aimed at a closed port: /info fails fast, nothing is cached.
	dead := httptest.NewServer(http.NewServeMux())
	dead.Close()
	dk, err := docker.New("tcp://" + strings.TrimPrefix(dead.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Config{SnapshotKinds: map[string]bool{}}
	h := hub.New(1024)
	mgr := collector.NewManager(cfg, dk, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(New(cfg, st, mgr, h, slog.Default(), WithDocker(dk)).Handler())
	t.Cleanup(srv.Close)

	raw, ov := fetchOverview(t, srv.URL, "", "")
	top := wantKeys(t, raw, "containers", "cpu", "host", "hosts", "mem", "window_ms")
	wantKeys(t, top["host"],
		"disk_free", "disk_total", "docker_version", "id", "mem_total", "name", "ncpu", "reclaimable", "uptime_s")
	if ov.WindowMS != 300000 {
		t.Fatalf("window_ms = %d", ov.WindowMS)
	}
	if ov.Host.ID != "" || ov.Host.Name != "" || ov.Host.NCPU != 0 || ov.Host.DockerVersion != "" {
		t.Fatalf("host = %+v, want all zero", ov.Host)
	}
	if ov.Containers == nil || len(ov.Containers) != 0 {
		t.Fatalf("containers = %#v, want empty (non-null) slice", ov.Containers)
	}
	// No trackers means no hosts either — still [] rather than null.
	if ov.Hosts == nil || len(ov.Hosts) != 0 {
		t.Fatalf("hosts = %#v, want empty (non-null) slice", ov.Hosts)
	}
	// Charts must be present but empty — [] rather than null.
	for name, g := range map[string]overviewGauge{"cpu": ov.CPU, "mem": ov.Mem} {
		if g.Hist.V == nil || len(g.Hist.V) != 0 || g.Value != 0 || g.Avg != 0 || g.Pk != 0 {
			t.Fatalf("%s gauge = %+v, want an empty (non-null) history", name, g)
		}
	}
	if !strings.Contains(string(raw), `"v":[]`) {
		t.Fatalf("body missing empty arrays: %s", raw)
	}
}

