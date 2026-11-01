// dashboard.go — global overview endpoints (v0.7):
//
//	GET /api/dashboard     aggregated stat row + per-app health cards
//	GET /api/crashloops    containers with repeated deaths in a window
package api

import (
	"context"
	"net/http"
	"strconv"
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

	summaries, err := s.st.AppSummaries(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}

	since24 := time.Now().Add(-24 * time.Hour).UnixMilli()

	// Deaths per app over 24h, plus the crash-loop set, come from snapshots.
	snaps, err := s.st.ListSnapshots(ctx, "", 0)
	if err != nil {
		fail(w, 500, err)
		return
	}
	deathsByApp := map[string]int64{}
	loopApps := map[string]bool{}
	var restarts24, snaps24 int64
	for _, sn := range snaps {
		if sn.TS >= since24 {
			restarts24++
			deathsByApp[sn.App]++
			snaps24++
		}
		if sn.RestartsWindow >= 3 {
			loopApps[sn.App] = true
		}
	}

	// Live containers give us "running now" counts.
	conts := s.mgr.Containers()
	runByApp := map[string]int{}
	for _, c := range conts {
		runByApp[c.App]++
	}

	sparkSince := time.Now().Add(-time.Hour).UnixMilli()
	cards := make([]AppCard, 0, len(summaries))
	for _, a := range summaries {
		card := AppCard{
			App: a.App, Containers: a.Containers, Running: runByApp[a.App],
			Errors1h: a.Errors1h, Warns1h: a.Warns1h,
			Restarts24: deathsByApp[a.App], CrashLoop: loopApps[a.App],
		}
		for _, sn := range snaps { // ListSnapshots is newest-first
			if sn.App == a.App {
				card.LastCrash = sn.TS
				break
			}
		}
		pts, err := s.st.QueryMetrics(ctx, a.App, "", sparkSince, 0, 60_000)
		if err != nil {
			fail(w, 500, err)
			return
		}
		if pts == nil {
			pts = []store.MetricPoint{}
		}
		card.Spark = pts
		cards = append(cards, card)
	}

	dbBytes, _ := s.st.UsedBytes(ctx)
	stats := DashboardStats{
		Containers:   len(conts),
		Restarts24h:  restarts24,
		CrashLoops:   len(loopApps),
		DBBytes:      dbBytes,
		Snapshots24h: snaps24,
	}
	writeJSON(w, map[string]any{"stats": stats, "apps": cards})
}

func (s *Server) crashLoops(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	threshold := 3
	if n, err := strconv.Atoi(r.URL.Query().Get("threshold")); err == nil && n > 0 {
		threshold = n
	}
	snaps, err := s.st.ListSnapshots(ctx, r.URL.Query().Get("app"), 0)
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
