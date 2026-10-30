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
	id, name, app, image, replica string
	tty                           bool
	created                       time.Time
	cancel                        context.CancelFunc
	logDone                       chan struct{} // closed when the log follower has finished
	accessOnly                    bool          // excluded container kept only for access-log parsing
	acc                           *access.Stream

	mu      sync.Mutex
	logs    []store.LogRow
	metrics []store.MetricPoint
	oom     bool
	signal  string
	exited  bool
	maxLogs int
	maxMets int
}

func (t *tracker) pushLog(r store.LogRow) {
	t.mu.Lock()
	t.logs = append(t.logs, r)
	if len(t.logs) > 2*t.maxLogs {
		t.logs = append([]store.LogRow(nil), t.logs[len(t.logs)-t.maxLogs:]...)
	}
	t.mu.Unlock()
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

func (t *tracker) lastMetric() (store.MetricPoint, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.metrics) == 0 {
		return store.MetricPoint{}, false
	}
	return t.metrics[len(t.metrics)-1], true
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
