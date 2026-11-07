package hub

import (
	"sync"
	"testing"
	"time"

	"capscope/internal/store"
)

func TestConcurrentPublishFanoutDoesNotBlock(t *testing.T) {
	h := New()
	const subscribers = 96
	const publishers = 8
	const rowsPerPublisher = 4000
	done := make(chan struct{})
	subs := make([]*Sub, 0, subscribers)
	slow := h.Subscribe(store.LogQuery{App: "web"})
	subs = append(subs, slow)

	var readers sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		s := h.Subscribe(store.LogQuery{App: "web"})
		subs = append(subs, s)
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-s.C:
				case <-done:
					return
				}
			}
		}()
	}

	start := time.Now()
	var writers sync.WaitGroup
	for p := 0; p < publishers; p++ {
		writers.Add(1)
		go func(p int) {
			defer writers.Done()
			for i := 0; i < rowsPerPublisher; i++ {
				app := "web"
				if i%5 == 0 {
					app = "ignored"
				}
				h.Publish([]store.LogRow{{ID: int64(p*rowsPerPublisher + i + 1), App: app, Line: "heavy traffic"}})
			}
		}(p)
	}
	writers.Wait()
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("concurrent fan-out took too long: %s", elapsed)
	}

	var dropped int64
	for _, s := range subs {
		dropped += s.Dropped.Load()
		h.Unsubscribe(s)
	}
	if dropped == 0 {
		t.Fatal("stress test did not exercise slow-subscriber backpressure")
	}
	close(done)
	readers.Wait()
}
