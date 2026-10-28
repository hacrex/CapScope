// Package hub fans committed log rows out to live subscribers (SSE clients).
package hub

import (
	"sync"
	"sync/atomic"

	"capscope/internal/store"
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

func (h *Hub) Subscribe(q store.LogQuery) *Sub {
	s := &Sub{C: make(chan store.LogRow, 2048), q: q}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
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
