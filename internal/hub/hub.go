// Package hub fans committed log rows out to live subscribers (SSE clients).
package hub

import (
	"sync"
	"sync/atomic"

	"capscope/internal/store"
)

const (
	// subBuffer is the per-subscriber queue. It only has to absorb the gap
	// between two browser polls, not the whole session: rows beyond it are
	// dropped and counted (the UI surfaces the count as "dropped"). Keeping
	// it small matters because the slot array is allocated eagerly, so the
	// cost is subBuffer × sizeof(LogRow) per connected client whether or not
	// the queue ever fills. It stays above one publish batch (1000 rows) so
	// a caught-up consumer never loses rows to a burst it has not read yet.
	subBuffer = 1024
	// maxSubs caps concurrent subscribers. Publish is O(rows × subs), and
	// each sub owns a buffer plus a goroutine in the SSE handler, so an
	// unbounded count is the one part of the fan-out that can grow without
	// limit (connection floods, reconnect storms).
	maxSubs = 1024
)

type Sub struct {
	C       chan store.LogRow
	Dropped atomic.Int64
	q       store.LogQuery
}

type Hub struct {
	mu   sync.RWMutex
	subs map[*Sub]struct{}
}

func New() *Hub { return &Hub{subs: map[*Sub]struct{}{}} }

// Subscribe registers a live log stream. ok is false once maxSubs concurrent
// subscribers exist; callers should reject the request rather than pin a
// buffer for a client that will only ever see a degraded stream.
func (h *Hub) Subscribe(q store.LogQuery) (*Sub, bool) {
	s := &Sub{C: make(chan store.LogRow, subBuffer), q: q}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxSubs {
		return nil, false
	}
	h.subs[s] = struct{}{}
	return s, true
}

func (h *Hub) Unsubscribe(s *Sub) {
	if s == nil {
		return
	}
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Count reports the number of live subscribers.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// DroppedTotal returns the total number of rows dropped across all
// subscribers (prometheus: capscope_sse_dropped_rows_total).
func (h *Hub) DroppedTotal() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var total int64
	for s := range h.subs {
		total += s.Dropped.Load()
	}
	return total
}

// Publish never blocks: a slow subscriber loses rows and its Dropped counter grows.
func (h *Hub) Publish(rows []store.LogRow) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.subs) == 0 {
		return
	}
	for _, r := range rows {
		for s := range h.subs {
			if !s.q.Match(r) {
				continue
			}
			select {
			case s.C <- r:
			default:
				s.Dropped.Add(1)
			}
		}
	}
}
