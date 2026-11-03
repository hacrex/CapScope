// dashboard.go — global overview endpoints (v0.7):
//
//	GET /api/dashboard     aggregated stat row + per-app health cards
//	GET /api/crashloops    containers with repeated deaths in a window
package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"capscope/internal/store"
)

// crashLoopWindow matches the collector's restart-loop detection window.
const crashLoopWindow = 15 * time.Minute

// AppCard is one app's health summary for the Global Dashboard grid.
type AppCard struct {
	App        string              `json:"app"`
	Containers int                 `json:"containers"`
	Running    int                 `json:"running"`
	Errors1h   int64               `json:"errors_1h"`
	Warns1h    int64               `json:"warns_1h"`
	Restarts24 int64               `json:"restarts_24h"`
	LastCrash  int64               `json:"last_crash"` // ms, 0 when none
	CrashLoop  bool                `json:"crash_loop"`
	Spark      []store.MetricPoint `json:"spark"` // last hour, 1-min buckets
}

// DashboardStats is the top stat row of the Global Dashboard.
type DashboardStats struct {
	Containers   int   `json:"containers"`
	Restarts24h  int64 `json:"restarts_24h"`
	CrashLoops   int   `json:"crash_loops"`
	DBBytes      int64 `json:"db_bytes"`
	Snapshots24h int64 `json:"snapshots_24h"`
}

// CrashLoopEntry flags one container whose death count inside the loop window
// reached the threshold.
type CrashLoopEntry struct {
	ContainerID string `json:"container_id"`
	Container   string `json:"container"`
	App         string `json:"app"`
	Deaths      int64  `json:"deaths"`
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// The whole payload is assembled in parallel and cached briefly: the first
	// paint on a big database used to serialize dozens of sparkline queries and
	// made the dashboard take many seconds to appear.
	gen, fresh := s.dash.begin()
	if fresh {
		if v, ok := s.dash.get(); ok {
			writeJSON(w, v)
			return
		}
	}

	summaries, err := s.st.AppSummaries(ctx, r.URL.Query().Get("host"))
	if err != nil {
		fail(w, 500, err)
		return
	}

	since24 := time.Now().Add(-24 * time.Hour).UnixMilli()

	// Deaths per app over 24h, plus the crash-loop set, come from snapshots —
	// bounded by ts so we never scan the full table here.
	snaps, err := s.st.SnapshotsSince(ctx, since24, r.URL.Query().Get("host"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	deathsByApp := map[string]int64{}
	loopApps := map[string]bool{}
	lastCrash := map[string]int64{} // snaps are newest-first: first hit wins
	var restarts24, snaps24 int64
	for _, sn := range snaps {
		restarts24++
		deathsByApp[sn.App]++
		snaps24++
		if _, seen := lastCrash[sn.App]; !seen {
			lastCrash[sn.App] = sn.TS
		}
		if sn.RestartsWindow >= 3 {
			loopApps[sn.App] = true
		}
	}

	// Live containers give us "running now" counts.
	conts := s.mgr.Containers(r.URL.Query().Get("host"))
	runByApp := map[string]int{}
	for _, c := range conts {
		runByApp[c.App]++
	}

	sparkSince := time.Now().Add(-time.Hour).UnixMilli()
	type sparkResult struct {
		idx int
		pts []store.MetricPoint
		err error
	}
	cards := make([]AppCard, 0, len(summaries))
	sparkCh := make(chan sparkResult, len(summaries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // bound concurrent readers; SQLite handles them fine but keep it sane
	for i, a := range summaries {
		card := AppCard{
			App: a.App, Containers: a.Containers, Running: runByApp[a.App],
			Errors1h: a.Errors1h, Warns1h: a.Warns1h,
			Restarts24: deathsByApp[a.App], CrashLoop: loopApps[a.App],
			LastCrash: lastCrash[a.App],
		}
		cards = append(cards, card)
		wg.Add(1)
		go func(i int, app string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pts, err := s.st.QueryMetrics(ctx, app, "", r.URL.Query().Get("host"), sparkSince, 0, 60_000)
			sparkCh <- sparkResult{i, pts, err}
		}(i, a.App)
	}
	wg.Wait()
	close(sparkCh)
	var sparkErr error
	for res := range sparkCh {
		if res.err != nil {
			sparkErr = res.err
			continue
		}
		if res.pts == nil {
			res.pts = []store.MetricPoint{}
		}
		cards[res.idx].Spark = res.pts
	}
	if sparkErr != nil {
		fail(w, 500, sparkErr)
		return
	}

	dbBytes, _ := s.st.UsedBytes(ctx)
	stats := DashboardStats{
		Containers:   len(conts),
		Restarts24h:  restarts24,
		CrashLoops:   len(loopApps),
		DBBytes:      dbBytes,
		Snapshots24h: snaps24,
	}
	payload := map[string]any{"stats": stats, "apps": cards}
	s.dash.store(gen, payload)
	writeJSON(w, payload)
}

func (s *Server) crashLoops(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	threshold := 3
	if n, err := strconv.Atoi(r.URL.Query().Get("threshold")); err == nil && n > 0 {
		threshold = n
	}
	snaps, err := s.st.ListSnapshots(ctx, r.URL.Query().Get("app"), r.URL.Query().Get("host"), 0)
	if err != nil {
		fail(w, 500, err)
		return
	}
	cutoff := time.Now().Add(-crashLoopWindow).UnixMilli()
	seen := map[string]bool{}
	out := []CrashLoopEntry{}
	for _, sn := range snaps {
		ts := sn.DieTS
		if ts == 0 {
			ts = sn.TS
		}
		if ts < cutoff {
			continue // loop window already passed for this snapshot
		}
		if seen[sn.ContainerID] {
			continue
		}
		seen[sn.ContainerID] = true
		if sn.RestartsWindow >= threshold {
			out = append(out, CrashLoopEntry{
				ContainerID: sn.ContainerID, Container: sn.Container, App: sn.App,
				Deaths: int64(sn.RestartsWindow),
			})
		}
	}
	writeJSON(w, map[string]any{"loops": out, "window_ms": crashLoopWindow.Milliseconds()})
}
