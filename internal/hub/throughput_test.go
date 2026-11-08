package hub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"capscope/internal/store"
)

// TestHighVolumeFanout500 exercises the same in-process fan-out used by the
// HTTP SSE handler: 500 subscribers receive 10,000 committed rows per second.
// It intentionally keeps consumers draining so the result measures hub CPU and
// delivery latency rather than artificial slow-client backpressure.
func TestHighVolumeFanout500(t *testing.T) {
	const subscribers = 500
	const rowsPerSecond = 10_000
	const duration = 3 * time.Second
	h := New()
	subs := make([]*Sub, 0, subscribers)
	for i := 0; i < subscribers; i++ {
		subs = append(subs, h.Subscribe(store.LogQuery{App: "load"}))
	}
	var received atomic.Int64
	var dropped atomic.Int64
	var maxLatency atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub *Sub) {
			defer wg.Done()
			for {
				select {
				case row := <-sub.C:
					received.Add(1)
					if i == 0 {
						latency := time.Since(time.Unix(0, row.TS)).Microseconds()
						for {
							old := maxLatency.Load()
							if latency <= old || maxLatency.CompareAndSwap(old, latency) {
								break
							}
						}
					}
				case <-stop:
					return
				}
			}
		}(i, sub)
	}
	start := time.Now()
	var published int64
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for time.Since(start) < duration {
		<-ticker.C
		rows := make([]store.LogRow, 1000)
		for i := range rows {
			rows[i] = store.LogRow{ID: published + int64(i) + 1, TS: time.Now().UnixNano(), App: "load", Line: "high-volume throughput test"}
		}
		h.Publish(rows)
		published += int64(len(rows))
	}
	want := published * subscribers
	deadline := time.Now().Add(5 * time.Second)
	for received.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	for _, sub := range subs {
		dropped.Add(sub.Dropped.Load())
		h.Unsubscribe(sub)
	}
	wg.Wait()
	if got := received.Load(); got != want {
		t.Fatalf("fanout dropped rows: published=%d subscribers=%d received=%d want=%d dropped=%d", published, subscribers, got, want, dropped.Load())
	}
	t.Logf("published=%d rows (%.0f rows/s), fanout=%d deliveries, max_latency=%dµs", published, float64(published)/time.Since(start).Seconds(), received.Load(), maxLatency.Load())
}
