// metrics.go — Prometheus text-format exposition at GET /metrics.
// Exposes container health gauges, DB size, and crash/restart counters so an
// external Prometheus can scrape CapScope without touching the SQLite file.
package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func writeEscapedLabel(w http.ResponseWriter, s string) {
	// Escape backslash, newline and double quote per the Prom spec.
	for _, r := range s {
		switch r {
		case '\\':
			w.Write([]byte(`\\`))
		case '\n':
			w.Write([]byte(`\n`))
		case '"':
			w.Write([]byte(`\"`))
		default:
			fmt.Fprint(w, string(r))
		}
	}
}

func (s *Server) prometheus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	now := time.Now()

	fmt.Fprint(w, "# HELP capscope_up Whether the CapScope API is reachable.\n")
	fmt.Fprint(w, "# TYPE capscope_up gauge\n")
	fmt.Fprintf(w, "capscope_up 1 %d\n", now.UnixMilli())

	fmt.Fprint(w, "# HELP capscope_containers Tracked containers by app and replica.\n")
	fmt.Fprint(w, "# TYPE capscope_containers gauge\n")
	for _, c := range s.mgr.Containers() {
		fmt.Fprint(w, "capscope_containers{app=\"")
		writeEscapedLabel(w, c.App)
		fmt.Fprint(w, "\",replica=\"")
		writeEscapedLabel(w, c.Replica)
		fmt.Fprintf(w, "\"} 1 %d\n", now.UnixMilli())
	}

	fmt.Fprint(w, "# HELP capscope_container_cpu_percent Last sampled CPU usage (percent of one core summed over cores).\n")
	fmt.Fprint(w, "# TYPE capscope_container_cpu_percent gauge\n")
	fmt.Fprint(w, "# HELP capscope_container_memory_bytes Last sampled memory usage in bytes.\n")
	fmt.Fprint(w, "# TYPE capscope_container_memory_bytes gauge\n")
	fmt.Fprint(w, "# HELP capscope_container_memory_limit_bytes Configured memory limit in bytes (0 when unlimited).\n")
	fmt.Fprint(w, "# TYPE capscope_container_memory_limit_bytes gauge\n")
	for _, c := range s.mgr.Containers() {
		id := c.ID
		if len(id) > 12 {
			id = id[:12]
		}
		labels := func() {
			fmt.Fprint(w, "{id=\"")
			writeEscapedLabel(w, id)
			fmt.Fprint(w, "\",app=\"")
			writeEscapedLabel(w, c.App)
			fmt.Fprint(w, "\"}")
		}
		fmt.Fprint(w, "capscope_container_cpu_percent")
		labels()
		fmt.Fprintf(w, " %s %d\n", strconv.FormatFloat(c.CPU, 'f', 2, 64), now.UnixMilli())
		fmt.Fprint(w, "capscope_container_memory_bytes")
		labels()
		fmt.Fprintf(w, " %d %d\n", c.Mem, now.UnixMilli())
		fmt.Fprint(w, "capscope_container_memory_limit_bytes")
		labels()
		fmt.Fprintf(w, " %d %d\n", c.MemLimit, now.UnixMilli())
	}

	ctx := r.Context()
	snaps, _ := s.st.ListSnapshots(ctx, "", 0)
	var restarts24h int64
	oomByApp := map[string]int64{}
	crashesByApp := map[string]int64{}
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	for _, sn := range snaps {
		if sn.TS >= cutoff {
			restarts24h++
			crashesByApp[sn.App]++
			if sn.OOMKilled {
				oomByApp[sn.App]++
			}
		}
	}
	fmt.Fprint(w, "# HELP capscope_restarts_24h Crash snapshots recorded in the last 24 hours.\n")
	fmt.Fprint(w, "# TYPE capscope_restarts_24h gauge\n")
	fmt.Fprintf(w, "capscope_restarts_24h %d %d\n", restarts24h, now.UnixMilli())

	fmt.Fprint(w, "# HELP capscope_crashes_total Crash snapshots per app since retention start.\n")
	fmt.Fprint(w, "# TYPE capscope_crashes_total counter\n")
	for app, n := range crashesByApp {
		fmt.Fprint(w, "capscope_crashes_total{app=\"")
		writeEscapedLabel(w, app)
		fmt.Fprintf(w, "\"} %d\n", n)
	}
	fmt.Fprint(w, "# HELP capscope_oom_kills_total OOM-killed crashes per app since retention start.\n")
	fmt.Fprint(w, "# TYPE capscope_oom_kills_total counter\n")
	for app, n := range oomByApp {
		fmt.Fprint(w, "capscope_oom_kills_total{app=\"")
		writeEscapedLabel(w, app)
		fmt.Fprintf(w, "\"} %d\n", n)
	}

	dbBytes, _ := s.st.UsedBytes(ctx)
	fmt.Fprint(w, "# HELP capscope_db_size_bytes On-disk size of the CapScope SQLite database.\n")
	fmt.Fprint(w, "# TYPE capscope_db_size_bytes gauge\n")
	fmt.Fprintf(w, "capscope_db_size_bytes %d %d\n", dbBytes, now.UnixMilli())

	fmt.Fprint(w, "# HELP capscope_alerts_firing_total Recorded alert firings per rule.\n")
	fmt.Fprint(w, "# TYPE capscope_alerts_firing_total counter\n")
	if s.eng != nil {
		for _, ru := range s.eng.Rules() {
			fmt.Fprint(w, "capscope_alerts_rule_defined{name=\"")
			writeEscapedLabel(w, ru.Name)
			fmt.Fprint(w, "\"} 1\n")
		}
	}
}
