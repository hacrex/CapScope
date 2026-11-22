package collector

import (
	"context"
	"sync"
	"time"

	"capscope/internal/access"
	"capscope/internal/store"
)

// tracker holds the live state of one running container: identity, in-memory
// ring buffers of recent log lines and metric samples (used for crash
// snapshots), and flags learned from Docker events.
type tracker struct {
	id, name, app, image, replica, hostID, gitCommit, health string
	tty                                   bool
	created                               time.Time // when CapScope started tracking this incarnation
	started                               time.Time // the container's real start time per Docker (falls back to created)
	cancel                                context.CancelFunc
	logDone                               chan struct{} // closed when the log follower has finished
	accessOnly                            bool          // excluded container kept only for access-log parsing
	acc                                   *access.Stream

	mu       sync.Mutex
	logs     []store.LogRow
	logBytes int // sum of logRowBytes(logs); guards the byte budget below
	metrics  []store.MetricPoint
	oom      bool
	signal   string
	exited   bool
	maxLogs  int
	maxMets  int
	// Writable-layer size from inspect?size=true; -1 until the first
	// measurement (see Manager.sizeLoop). Guarded by mu like the rest.
	sizeRW int64
}

// parseStartedAt extracts the container's real start time from an inspect's
// State.StartedAt (RFC3339Nano). Zero/unparseable values fall back to now so
// callers always get a usable time.
func parseStartedAt(startedAt string, now time.Time) time.Time {
	if st, err := time.Parse(time.RFC3339Nano, startedAt); err == nil && !st.IsZero() {
		return st
	}
	return now
}

// maxLogRingBytes is the per-container byte budget for the in-memory log
// ring. Rows are counted too (maxLogs), but each row carries its own line
// text (up to cfg.MaxLineBytes), so a count-only cap lets a container that
// logs long lines retain hundreds of megabytes across many trackers.
const maxLogRingBytes = 2 << 20 // 2 MiB per container

// logRowBytes estimates what one retained LogRow costs: the line text (the
// only per-row allocation; identity strings are shared across rows) plus the
// struct and string headers.
func logRowBytes(r store.LogRow) int { return len(r.Line) + 128 }

func (t *tracker) pushLog(r store.LogRow) {
	t.mu.Lock()
	t.logs = append(t.logs, r)
	t.logBytes += logRowBytes(r)
	// Trim when either budget is exceeded. The row budget overshoots by 25%
	// so a steady stream does not re-copy the slice on every line.
	if len(t.logs) > t.maxLogs+t.maxLogs/4 || t.logBytes > maxLogRingBytes {
		t.trimLogs()
	}
	t.mu.Unlock()
}

// trimLogs drops the oldest rows until both the row count and the byte
// budget hold. The byte target is half the budget so the copy below is
// amortized over many lines rather than paid on every one.
func (t *tracker) trimLogs() {
	start := 0
	if over := len(t.logs) - t.maxLogs; over > start {
		start = over
	}
	budget := t.logBytes
	for i := 0; i < start; i++ {
		budget -= logRowBytes(t.logs[i])
	}
	for start < len(t.logs) && budget > maxLogRingBytes/2 {
		budget -= logRowBytes(t.logs[start])
		start++
	}
	if start <= 0 {
		return
	}
	t.logBytes = budget
	t.logs = append([]store.LogRow(nil), t.logs[start:]...)
}

func (t *tracker) pushMetric(p store.MetricPoint) {
	t.mu.Lock()
	t.metrics = append(t.metrics, p)
	if len(t.metrics) > 2*t.maxMets {
		t.metrics = append([]store.MetricPoint(nil), t.metrics[len(t.metrics)-t.maxMets:]...)
	}
	t.mu.Unlock()
}

func (t *tracker) recentLogs() []store.LogRow {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.logs
	if len(s) > t.maxLogs {
		s = s[len(s)-t.maxLogs:]
	}
	return append([]store.LogRow(nil), s...)
}

func (t *tracker) recentMetrics() []store.MetricPoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.metrics
	if len(s) > t.maxMets {
		s = s[len(s)-t.maxMets:]
	}
	return append([]store.MetricPoint(nil), s...)
}

// aggregate walks the metric ring once, under the lock, feeding both
// accumulators and collecting the window bounds / sums / newest sample that
// the overview needs. It replaces recentMetrics() plus three separate
// passes over the copy, which allocated ~7 KB per container per poll.
//
// The accumulators are owned by the caller and reset before each call, so
// they are safe to reuse across trackers.
func (t *tracker) aggregate(windowStart, nowMs int64, cpu, mem *acc) winAgg {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.metrics
	if len(s) > t.maxMets {
		s = s[len(s)-t.maxMets:]
	}
	var wa winAgg
	for i, p := range s {
		cpu.add(p.TS, p.CPU)
		mem.add(p.TS, float64(p.Mem))
		if p.TS >= windowStart {
			if wa.n == 0 {
				wa.first = p
			}
			wa.last = p
			wa.n++
			wa.sumCPU += p.CPU
			wa.sumMem += float64(p.Mem)
		}
		if i == len(s)-1 {
			wa.newest, wa.hasNewest = p, true
		}
	}
	return wa
}

// info renders the live view of this container: identity, start time and the
// newest metric sample (zero-valued until the first sample arrives). Shared
// by /api/containers and the /api/overview aggregation.
func (t *tracker) info() ContainerInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Prefer the container's real Docker start time; fall back to when
	// tracking began (e.g. rebuilt trackers for already-exited containers).
	startedMS := t.created.UnixMilli()
	if !t.started.IsZero() && t.started.Before(t.created) {
		startedMS = t.started.UnixMilli()
	}
	ci := ContainerInfo{
		ID: t.id, Name: t.name, App: t.app, Replica: t.replica, Image: t.image,
		HostID: t.hostID, Started: startedMS, SizeRW: t.sizeRW,
		GitCommit: t.gitCommit, Health: t.health,
	}
	if n := len(t.metrics); n > 0 {
		p := t.metrics[n-1]
		ci.CPU, ci.Mem, ci.MemLimit, ci.SampleTS = p.CPU, p.Mem, p.MemLimit, p.TS
	}
	return ci
}

// setSizeRW records the measured writable-layer size.
func (t *tracker) setSizeRW(n int64) {
	t.mu.Lock()
	t.sizeRW = n
	t.mu.Unlock()
}

func (t *tracker) setOOM() {
	t.mu.Lock()
	t.oom = true
	t.mu.Unlock()
}

func (t *tracker) setSignal(s string) {
	t.mu.Lock()
	t.signal = s
	t.mu.Unlock()
}

func (t *tracker) setHealth(h string) {
	t.mu.Lock()
	t.health = h
	t.mu.Unlock()
}

func (t *tracker) markExited() {
	t.mu.Lock()
	t.exited = true
	t.mu.Unlock()
}

func (t *tracker) isExited() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exited
}

func (t *tracker) flags() (oom bool, signal string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.oom, t.signal
}
