// Command seed-demo fills a database with fake apps, logs, metrics and one crash snapshot,
// so the UI can be tried without Docker: make demo.
package main

import (
	"fmt"
	"os"
	"time"

	"capscope/internal/store"
)

func main() {
	st, err := store.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	now := time.Now()
	for _, app := range []string{"shop", "blog"} {
		_ = st.UpsertContainer(store.ContainerRow{ID: app + "c1", Name: "srv-captain--" + app + ".1.x", App: app, Image: app + ":latest", Replica: "1", State: "running", FirstSeen: now.UnixMilli(), LastSeen: now.UnixMilli()})
		var rows []store.LogRow
		for i := 0; i < 300; i++ {
			ts := now.Add(time.Duration(i-300) * time.Second)
			lvl, line := "info", fmt.Sprintf(`{"level":"info","msg":"request handled","path":"/api/items/%d","ms":%d}`, i, 5+i%30)
			if i%37 == 0 {
				lvl, line = "error", fmt.Sprintf("ERROR upstream timeout calling payments (attempt %d)", i)
			}
			if i%23 == 0 {
				lvl, line = "warn", "WARN slow query 1200ms"
			}
			rows = append(rows, store.LogRow{TS: ts.UnixNano(), TSMs: ts.UnixMilli(), ContainerID: app + "c1", Container: "srv-captain--" + app + ".1.x", App: app, Stream: "o", Level: lvl, Line: line})
		}
		_ = st.InsertLogs(rows)
		for i := 0; i < 120; i++ {
			ts := now.Add(time.Duration(i-120) * 10 * time.Second).UnixMilli()
			_ = st.InsertMetric(app+"c1", app, store.MetricPoint{TS: ts, CPU: 10 + float64(i%20), Mem: int64(100+i*2) << 20, MemLimit: 512 << 20})
		}
	}
	var mets []store.MetricPoint
	for i := 0; i < 30; i++ {
		mets = append(mets, store.MetricPoint{TS: now.Add(time.Duration(i-30) * 10 * time.Second).UnixMilli(), CPU: 20, Mem: int64(200+i*10) << 20, MemLimit: 512 << 20})
	}
	_, _ = st.InsertSnapshot(store.Snapshot{TS: now.Add(-20 * time.Minute).UnixMilli(), Kind: "oom", ContainerID: "shopc0", Container: "srv-captain--shop.1.old", App: "shop", Image: "shop:1.4", ExitCode: 137, OOMKilled: true, RestartCount: 2, MemLimit: 512 << 20, UptimeSec: 5400,
		Summary: "Killed by the kernel OOM killer. Peak sampled memory was 490.0 MiB of a 512.0 MiB limit. Raise the memory limit or look for a leak.",
		Logs:    []store.LogRow{{TSMs: now.UnixMilli(), Stream: "o", Line: "cache grew to 480MB"}, {TSMs: now.UnixMilli(), Stream: "e", Line: "ERROR allocation failed"}}, Metrics: mets})
}
