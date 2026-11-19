package collector

import (
	"sort"
	"time"

	"capscope/internal/store"
)

// The /api/overview windows. Aggregates (avg + counter rates) cover the last
// OverviewWindowMS; the chart series cover the last 10 minutes bucketed into
// at most overviewHistBuckets buckets of overviewHistStepMS each — the shape
// the published overview contract promises. With the default 10s stats
// interval the metric ring holds exactly ~10 minutes of samples, matching
// the chart span.
const (
	OverviewWindowMS    = 300000 // 5 minutes: the avg/rate window
	overviewHistSpanMS  = int64(10 * time.Minute / time.Millisecond)
	overviewHistBuckets = 60
	OverviewHistStepMS  = overviewHistSpanMS / overviewHistBuckets // 10000 (10 s)
)

// Series is one bucketed chart series: bucket i covers
// [T0+i*StepMS, T0+(i+1)*StepMS) and holds the average of the samples that
// fell into it. V is an empty (never nil) slice when the window holds no
// samples, so it always marshals as a JSON array.
type Series struct {
	T0     int64     `json:"t0"`
	StepMS int64     `json:"step_ms"`
	V      []float64 `json:"v"`
}

// OverviewContainer is one row of /api/overview's containers[]: the identity
// of ContainerInfo plus aggregates over the in-memory metric ring — window
// averages, counter-derived rates and the CPU chart.
type OverviewContainer struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	App       string  `json:"app"`
	Replica   string  `json:"replica"`
	Image     string  `json:"image"`
	HostID    string  `json:"host_id"`
	Started   int64   `json:"started"`
	CPU       float64 `json:"cpu"`
	Mem       int64   `json:"mem"`
	MemLimit  int64   `json:"mem_limit"`
	AvgCPU    float64 `json:"avg_cpu"`
	AvgMem    float64 `json:"avg_mem"`
	CPUHist   Series  `json:"cpu_hist"`
	NetRxRate float64 `json:"net_rx_rate"`
	NetTxRate float64 `json:"net_tx_rate"`
	BlkRRate  float64 `json:"blk_r_rate"`
	BlkWRate  float64 `json:"blk_w_rate"`
	SizeRW    int64   `json:"size_rw"`
}

// Overview is the tracker half of GET /api/overview. The host series hold
// raw sums over the same (viewer-scoped) containers as Containers: the API
// divides HostCPU by the daemon's core count to turn it into percent of
// total host capacity.
type Overview struct {
	Containers []OverviewContainer
	HostCPU    Series  // summed container core-percent per bucket
	HostMem    Series  // summed container bytes per bucket
	CPUValue   float64 // sum of each container's newest sample, core-percent
	MemValue   int64   // sum of each container's newest sample, bytes
}

// Overview aggregates the tracked containers for /api/overview. hostID
// filters by tracked host ("" = all); keep, when non-nil, filters by app —
// the same viewer-scope restriction /api/containers applies, applied before
// aggregation so the host totals only include apps the caller may see.
// Containers are returned sorted by app then name for stable rendering.
func (m *Manager) Overview(now time.Time, hostID string, keep func(app string) bool) Overview {
	nowMs := now.UnixMilli()
	windowStart := nowMs - OverviewWindowMS

	m.mu.Lock()
	trackers := make([]*tracker, 0, len(m.tracked))
	for _, t := range m.tracked {
		if hostID != "" && t.hostID != hostID {
			continue
		}
		if keep != nil && !keep(t.app) {
			continue
		}
		trackers = append(trackers, t)
	}
	m.mu.Unlock()

	out := Overview{Containers: make([]OverviewContainer, 0, len(trackers))}
	// Host charts: for each bucket, the sum over containers of that
	// container's bucket mean — NOT one accumulator fed every sample, which
	// would average across containers instead of adding them.
	var hostCPU, hostMem []float64
	hostFirst := -1 // leading buckets before any container reported
	// Reused across iterations: both accumulators are the same fixed size
	// for every container, so allocating a pair per row was ~2 KB of garbage
	// per container per poll.
	cpuAcc, memAcc := newAcc(nowMs), newAcc(nowMs)
	for _, t := range trackers {
		ci := t.info()
		row := OverviewContainer{
			ID: ci.ID, Name: ci.Name, App: ci.App, Replica: ci.Replica, Image: ci.Image,
			HostID: ci.HostID, Started: ci.Started,
			CPU: ci.CPU, Mem: ci.Mem, MemLimit: ci.MemLimit, SizeRW: ci.SizeRW,
		}
		// One walk of the ring under the tracker's lock, feeding both
		// accumulators and collecting the window bounds the rates need.
		// recentMetrics() copied the whole ring out first (~7 KB per
		// container per request) and then walked it three times.
		cpuAcc.reset(nowMs)
		memAcc.reset(nowMs)
		wa := t.aggregate(windowStart, nowMs, cpuAcc, memAcc)
		if wa.n > 0 {
			row.AvgCPU = wa.sumCPU / float64(wa.n)
			row.AvgMem = wa.sumMem / float64(wa.n)
			row.NetRxRate = counterRate(wa.n, wa.first, wa.last, func(p store.MetricPoint) int64 { return p.NetRx })
			row.NetTxRate = counterRate(wa.n, wa.first, wa.last, func(p store.MetricPoint) int64 { return p.NetTx })
			row.BlkRRate = counterRate(wa.n, wa.first, wa.last, func(p store.MetricPoint) int64 { return p.BlkR })
			row.BlkWRate = counterRate(wa.n, wa.first, wa.last, func(p store.MetricPoint) int64 { return p.BlkW })
		}
		// cm is retained by the row's chart, so it stays a fresh allocation;
		// mm is only summed into the host charts below.
		cm := cpuAcc.means()
		row.CPUHist = cpuAcc.series(cm)
		out.Containers = append(out.Containers, row)

		// Fold this container's bucket means into the host charts. Sums of
		// per-container means (not one shared accumulator: that would average
		// the containers together instead of adding them). A container with
		// no samples yet contributes nothing and does not move hostFirst.
		if first := cpuAcc.firstNonEmpty(); first >= 0 {
			if hostCPU == nil {
				hostCPU, hostMem = make([]float64, overviewHistBuckets), make([]float64, overviewHistBuckets)
				hostFirst = first
			} else if first < hostFirst {
				hostFirst = first
			}
			mm := memAcc.means()
			for i := range cm {
				hostCPU[i] += cm[i]
				hostMem[i] += mm[i]
			}
		}

		if wa.hasNewest {
			out.CPUValue += wa.newest.CPU
			out.MemValue += wa.newest.Mem
		}
	}
	out.HostCPU = hostSeries(hostCPU, hostFirst, nowMs)
	out.HostMem = hostSeries(hostMem, hostFirst, nowMs)
	sort.Slice(out.Containers, func(i, j int) bool {
		a, b := out.Containers[i], out.Containers[j]
		if a.App != b.App {
			return a.App < b.App
		}
		return a.Name < b.Name
	})
	return out
}

func lastOf(pts []store.MetricPoint) (store.MetricPoint, bool) {
	if len(pts) == 0 {
		return store.MetricPoint{}, false
	}
	return pts[len(pts)-1], true
}

// winAgg is one tracker's contribution to /api/overview, collected during the
// single ring walk in aggregate: the window bounds the counter rates need,
// the window sums for the averages, and the newest sample for the host
// totals. n is the number of samples inside the window.
type winAgg struct {
	first, last store.MetricPoint // first/last sample inside the window
	n           int               // samples inside the window
	sumCPU      float64
	sumMem      float64
	newest      store.MetricPoint // newest sample in the ring, regardless of window
	hasNewest   bool
}

// counterRate turns a cumulative counter into units/sec across the window:
// (last - first) / elapsed seconds. Fewer than two points or a non-positive
// span yields 0; a negative delta — the counter reset when a container
// restarts — is clamped to 0.
func counterRate(n int, first, last store.MetricPoint, sel func(store.MetricPoint) int64) float64 {
	if n < 2 {
		return 0
	}
	span := float64(last.TS-first.TS) / 1000
	if span <= 0 {
		return 0
	}
	d := float64(sel(last) - sel(first))
	if d < 0 {
		d = 0
	}
	return d / span
}

// acc buckets samples onto the fixed overview grid: the last
// overviewHistBuckets steps of overviewHistStepMS ending with the current
// (partial) bucket, so the grid always contains "now".
type acc struct {
	t0, step int64
	sum      []float64
	n        []int
}

func newAcc(nowMs int64) *acc {
	a := &acc{
		step: OverviewHistStepMS,
		sum:  make([]float64, overviewHistBuckets),
		n:    make([]int, overviewHistBuckets),
	}
	a.reset(nowMs)
	return a
}

// reset re-arms the accumulator onto the grid ending at nowMs, zeroing the
// bucket tallies in place. Reusing one accumulator across containers (they
// are all the same fixed size) turns two allocations per container per poll
// into two per request.
func (a *acc) reset(nowMs int64) {
	end := nowMs - nowMs%OverviewHistStepMS // start of the current bucket
	a.t0 = end - (overviewHistBuckets-1)*OverviewHistStepMS
	for i := range a.sum {
		a.sum[i] = 0
	}
	for i := range a.n {
		a.n[i] = 0
	}
}

func (a *acc) add(ts int64, v float64) {
	if ts < a.t0 {
		return // older than the 10-minute grid
	}
	if i := (ts - a.t0) / a.step; i < int64(len(a.sum)) {
		a.sum[i] += v
		a.n[i]++
	}
}

// firstNonEmpty is the index of the first bucket holding a sample, or -1
// when the grid has none.
func (a *acc) firstNonEmpty() int {
	for i, n := range a.n {
		if n > 0 {
			return i
		}
	}
	return -1
}

// means is the full-grid per-bucket average: 0 before the first sample, then
// the bucket mean carried forward across gaps — the same values series()
// emits, untrimmed, so the host charts can sum containers bucket by bucket.
func (a *acc) means() []float64 {
	v := make([]float64, len(a.sum))
	last, started := 0.0, false
	for i, n := range a.n {
		if n > 0 {
			last = a.sum[i] / float64(n)
			started = true
		}
		if started {
			v[i] = last
		}
	}
	return v
}

// series shapes the bucketed chart from an already-computed means grid: it
// drops the leading buckets before the first sample (a container that
// started a minute ago gets ~6 bars, not 54 zeros). The caller owns means,
// so passing it in avoids a second 480-byte grid allocation per container.
func (a *acc) series(means []float64) Series {
	first := a.firstNonEmpty()
	if first < 0 {
		return Series{StepMS: a.step, V: []float64{}}
	}
	return Series{T0: a.t0 + int64(first)*a.step, StepMS: a.step, V: means[first:]}
}

// hostSeries shapes the summed host grid: trim the leading buckets no
// container reported for (same trim as the row charts) and stamp the grid's
// t0. sum is empty/first < 0 when nothing was measured — an empty (never
// null) history.
func hostSeries(sum []float64, first int, nowMs int64) Series {
	if first < 0 || len(sum) == 0 {
		return Series{StepMS: OverviewHistStepMS, V: []float64{}}
	}
	end := nowMs - nowMs%OverviewHistStepMS
	t0 := end - (overviewHistBuckets-1)*OverviewHistStepMS
	return Series{T0: t0 + int64(first)*OverviewHistStepMS, StepMS: OverviewHistStepMS, V: sum[first:]}
}
