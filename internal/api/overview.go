// overview.go — Dozzle-style dashboard payload:
//
//	GET /api/overview[?host=<id>]  host metrics + per-container window aggregates
//
// The host half comes from Docker /info + /system/df (cached: see hostCache),
// the container half from the collector's in-memory metric rings.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"capscope/internal/collector"
	"capscope/internal/docker"
)

const (
	// hostInfoTTL / hostDFTTL: how long each half of the host block stays
	// fresh. /info is one small JSON document and refreshes inline; /system/df
	// walks every image and volume, so it only ever refreshes in the
	// background (see hostCache).
	hostInfoTTL = 5 * time.Minute
	hostDFTTL   = 2 * time.Minute
)

// overviewHost is the host half of the payload. Zero-valued fields mean
// "unknown" — the UI hides what it can't know.
type overviewHost struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	NCPU          int    `json:"ncpu"`
	MemTotal      int64  `json:"mem_total"`
	DockerVersion string `json:"docker_version"`
	DiskTotal     int64  `json:"disk_total"`
	DiskFree      int64  `json:"disk_free"`
	Reclaimable   int64  `json:"reclaimable"`
	UptimeS       int64  `json:"uptime_s"`
}

// overviewHostEntry is one card of the dashboard's host grid: the host block
// plus that host's own gauges and container count. Identity fields stay zero
// for remote hosts this process cannot probe — the UI falls back to the id.
type overviewHostEntry struct {
	overviewHost
	Containers int            `json:"containers"`
	CPU        overviewGauge  `json:"cpu"`
	Mem        overviewGauge  `json:"mem"`
}

// overviewGauge is one host metric: the current reading plus its 10-minute
// chart and the average/peak over the chart's buckets.
type overviewGauge struct {
	Value float64          `json:"value"`
	Avg   float64          `json:"avg"`
	Pk    float64          `json:"pk"`
	Hist  collector.Series `json:"hist"`
}

// overviewResponse is the body of GET /api/overview.
type overviewResponse struct {
	Host       overviewHost                  `json:"host"`
	Hosts      []overviewHostEntry           `json:"hosts"`
	WindowMS   int64                         `json:"window_ms"`
	CPU        overviewGauge                 `json:"cpu"`
	Mem        overviewGauge                 `json:"mem"`
	Containers []collector.OverviewContainer `json:"containers"`
}

// hostInfo is one snapshot of the local daemon's hardware and identity.
type hostInfo struct {
	name          string
	ncpu          int
	memTotal      int64
	dockerVersion string
	diskTotal     int64
	diskFree      int64
	uptimeS       int64
}

// hostCache holds the two Docker calls behind the host block. /info (plus
// statfs) refreshes inline at most every hostInfoTTL under a short deadline;
// /system/df can take seconds on a busy daemon, so it only ever refreshes
// from a background goroutine and the handler always serves the last known
// value (0 until the first refresh lands).
type hostCache struct {
	mu        sync.Mutex
	info      *hostInfo // nil until the first successful /info
	infoAt    time.Time // last /info attempt (success or failure: throttles retries)
	reclaim   int64     // last known reclaimable bytes
	reclaimAt time.Time // last /system/df attempt (success or failure)
	dfBusy    bool      // a background refresh is in flight
}

// WithDocker attaches the Docker client /api/overview reads the host block
// from. Without it the host block stays zeroed while the container half
// still works.
func WithDocker(dk *docker.Client) func(*Server) { return func(s *Server) { s.dk = dk } }

// overview serves the dashboard payload: host metrics plus per-container
// aggregates over the last window_ms (5 min), with 10-minute CPU/mem charts.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	hostFilter := r.URL.Query().Get("host")
	allowed, restricted := s.viewerScope(r)
	var keep func(string) bool
	if restricted {
		// Same viewer-scope restriction as /api/containers, applied before
		// aggregation so the host totals only include visible apps too.
		keep = func(app string) bool { return appInScope(app, allowed) }
	}
	ov := s.mgr.Overview(time.Now(), hostFilter, keep)

	resp := overviewResponse{
		Host:       s.overviewHost(r.Context(), hostFilter),
		WindowMS:   collector.OverviewWindowMS,
		Mem:        gauge(ov.HostMem, float64(ov.MemValue)),
		Containers: ov.Containers,
	}
	if resp.Containers == nil {
		resp.Containers = []collector.OverviewContainer{}
	}
	// Host CPU is percent of total capacity: the tracker series sums container
	// core-percent, so divide by the daemon's core count. Without a known
	// core count the unit would be meaningless — report no data rather than a
	// wrong percentage (ncpu 0 also marks the whole host block as unknown).
	if resp.Host.NCPU > 0 {
		f := 1 / float64(resp.Host.NCPU)
		resp.CPU = gauge(scale(ov.HostCPU, f), ov.CPUValue*f)
	} else {
		resp.CPU = gauge(collector.Series{V: []float64{}}, 0)
	}
	// Host grid: one card per host with containers in the window. Only
	// rendered when the UI has more than one host to show; the assembly is
	// cheap (identity comes from the same cache as resp.Host, and remote ids
	// short-circuit before any Docker call).
	resp.Hosts = make([]overviewHostEntry, 0, len(ov.Hosts))
	for _, h := range ov.Hosts {
		id := h.HostID
		if id == "" {
			id = s.mgr.HostID()
		}
		e := overviewHostEntry{
			overviewHost: s.overviewHost(r.Context(), id),
			Containers:   h.Containers,
			Mem:          gauge(h.Mem, float64(h.MemValue)),
		}
		if e.NCPU > 0 {
			f := 1 / float64(e.NCPU)
			e.CPU = gauge(scale(h.CPU, f), h.CPUValue*f)
		} else {
			e.CPU = gauge(collector.Series{V: []float64{}}, 0)
		}
		resp.Hosts = append(resp.Hosts, e)
	}
	writeJSON(w, resp)
}

// overviewHost assembles the host block. It is fully filled only for the
// local daemon — an empty filter or this instance's own host id. Any other
// id is a remote host whose hardware this process cannot probe: only the
// id/name are reported (the name falls back to the id, the one label we do
// know) and everything else stays zero so the UI hides it.
func (s *Server) overviewHost(ctx context.Context, filter string) overviewHost {
	h := overviewHost{ID: filter}
	if s.dk == nil {
		return h
	}
	if filter != "" && filter != s.mgr.HostID() {
		h.Name = filter
		return h
	}
	info := s.host.cachedInfo(ctx, s.dk)
	if info == nil {
		return h // Docker unreachable and nothing cached: report zeros
	}
	h.Name = info.name
	h.NCPU = info.ncpu
	h.MemTotal = info.memTotal
	h.DockerVersion = info.dockerVersion
	h.DiskTotal = info.diskTotal
	h.DiskFree = info.diskFree
	h.UptimeS = info.uptimeS
	h.Reclaimable = s.host.reclaimable(s.dk, s.log)
	return h
}

// gauge folds a chart series into the value/avg/pk triple: value is the
// "now" reading, avg/pk are the mean and peak of the chart's buckets.
func gauge(s collector.Series, value float64) overviewGauge {
	if s.V == nil {
		s.V = []float64{} // always a JSON array, never null
	}
	g := overviewGauge{Value: value, Hist: s}
	if len(s.V) == 0 {
		return g
	}
	var sum float64
	pk := s.V[0]
	for _, v := range s.V {
		sum += v
		if v > pk {
			pk = v
		}
	}
	g.Avg, g.Pk = sum/float64(len(s.V)), pk
	return g
}

// scale multiplies every bucket of a series — turns summed container
// core-percent into percent of host capacity.
func scale(s collector.Series, f float64) collector.Series {
	v := make([]float64, len(s.V))
	for i, x := range s.V {
		v[i] = x * f
	}
	s.V = v
	return s
}

// info returns the cached /info snapshot, refreshing it inline when the TTL
// has passed. A failed attempt keeps the previous value (serve stale) and
// still advances the TTL so an unreachable daemon is not re-probed on every
// request. The deadline bounds how long a hung daemon can hold the handler.
func (c *hostCache) cachedInfo(ctx context.Context, dk *docker.Client) *hostInfo {
	c.mu.Lock()
	if time.Since(c.infoAt) < hostInfoTTL {
		snap := c.info
		c.mu.Unlock()
		return snap
	}
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	inf, err := dk.Info(rctx)
	if err != nil {
		c.mu.Lock()
		c.infoAt = time.Now()
		stale := c.info
		c.mu.Unlock()
		return stale // may be nil on the very first failure
	}
	snap := &hostInfo{name: inf.Name, ncpu: inf.NCPU, memTotal: inf.MemTotal, dockerVersion: inf.ServerVersion}
	// Minimal daemons may omit ServerVersion from /info; /version has it.
	if snap.dockerVersion == "" {
		if v, verr := dk.Version(rctx); verr == nil {
			snap.dockerVersion = v
		}
	}
	if inf.DockerRootDir != "" {
		// 0/0 when the path cannot be stat'd from here (unknown to this OS
		// or hidden inside a container) — the UI hides the disk gauges.
		snap.diskTotal, snap.diskFree, _ = diskUsage(inf.DockerRootDir)
	}
	snap.uptimeS = hostUptime() // 0 where /proc/uptime is unreadable; UI hides it
	c.mu.Lock()
	c.info, c.infoAt = snap, time.Now()
	c.mu.Unlock()
	return snap
}

// reclaimable returns the last known reclaimable-bytes estimate. When it is
// older than hostDFTTL it kicks off a background refresh — docker system df
// walks every image and volume and must never stall this handler — and
// serves whatever it has right now (0 until the first refresh lands).
// Errors keep the previous value and throttle the next attempt.
func (c *hostCache) reclaimable(dk *docker.Client, log *slog.Logger) int64 {
	c.mu.Lock()
	if time.Since(c.reclaimAt) >= hostDFTTL && !c.dfBusy {
		c.dfBusy = true
		// Detached from the request: a cancelled poll must not abort the
		// refresh every other request is waiting on.
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var v int64
			df, err := dk.SystemDF(rctx)
			if err == nil {
				v = df.Reclaimable()
			}
			c.mu.Lock()
			if err == nil {
				c.reclaim = v
			}
			c.reclaimAt = time.Now()
			c.dfBusy = false
			c.mu.Unlock()
			if err != nil {
				log.Debug("overview: system df failed", "err", err)
			}
		}()
	}
	v := c.reclaim
	c.mu.Unlock()
	return v
}
