// Package forward forwards collected log rows to external systems
// (Loki, Elasticsearch) for long-term retention beyond CapScope's
// SQLite size cap.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"capscope/internal/hub"
	"capscope/internal/store"
)

// Forwarder subscribes to the CapScope hub and forwards log rows to
// external systems (Loki, Elasticsearch). It batches rows and flushes
// either when the batch is full or on a timer, whichever comes first.
type Forwarder struct {
	hub       *hub.Hub
	lokiURL   string
	esURL     string
	esIndex   string
	log       *slog.Logger

	mu      sync.Mutex
	batch   []store.LogRow
	batchMu sync.Mutex

	batchSize int
	flushInt  time.Duration

	sub *hub.Sub
}

// New creates a forwarder. If both lokiURL and esURL are empty, the
// forwarder is a no-op.
func New(h *hub.Hub, lokiURL, esURL, esIndex string, batchSize int, flushInt time.Duration, log *slog.Logger) *Forwarder {
	if lokiURL == "" && esURL == "" {
		return nil
	}
	f := &Forwarder{
		hub:       h,
		lokiURL:   strings.TrimSuffix(lokiURL, "/"),
		esURL:     strings.TrimSuffix(esURL, "/"),
		esIndex:   esIndex,
		log:       log,
		batchSize: batchSize,
		flushInt:  flushInt,
	}
	return f
}

// Start subscribes to the hub and begins forwarding. Blocks until ctx is
// cancelled. Should be called in a goroutine.
func (f *Forwarder) Start(ctx context.Context) {
	if f == nil {
		return
	}

	sub, ok := f.hub.Subscribe(store.LogQuery{})
	if !ok {
		f.log.Error("forward: hub at subscriber cap, log forwarding disabled")
		return
	}
	defer f.hub.Unsubscribe(sub)
	f.sub = sub

	flushTicker := time.NewTicker(f.flushInt)
	defer flushTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			f.flush(ctx)
			return
		case row := <-sub.C:
			f.batchMu.Lock()
			f.batch = append(f.batch, row)
			if len(f.batch) >= f.batchSize {
				batch := f.batch
				f.batch = nil
				f.batchMu.Unlock()
				go f.flushBatch(context.Background(), batch)
			} else {
				f.batchMu.Unlock()
			}
		case <-flushTicker.C:
			f.flush(ctx)
		}
	}
}

// flush sends any buffered rows to the configured backends.
func (f *Forwarder) flush(ctx context.Context) {
	f.batchMu.Lock()
	if len(f.batch) == 0 {
		f.batchMu.Unlock()
		return
	}
	batch := f.batch
	f.batch = nil
	f.batchMu.Unlock()
	f.flushBatch(ctx, batch)
}

// flushBatch sends a batch of rows to Loki and/or Elasticsearch.
func (f *Forwarder) flushBatch(ctx context.Context, rows []store.LogRow) {
	if f.lokiURL != "" {
		if err := f.pushLoki(ctx, rows); err != nil {
			f.log.Error("forward: loki push failed", "rows", len(rows), "err", err)
		}
	}
	if f.esURL != "" {
		if err := f.pushES(ctx, rows); err != nil {
			f.log.Error("forward: es push failed", "rows", len(rows), "err", err)
		}
	}
}

// pushLoki sends rows to the Loki push API as a single stream.
func (f *Forwarder) pushLoki(ctx context.Context, rows []store.LogRow) error {
	// Group rows into a single stream with labels from the first row.
	if len(rows) == 0 {
		return nil
	}

	var entries strings.Builder
	for _, r := range rows {
		tsNano := r.TS
		line := strings.ReplaceAll(r.Line, "\n", "\\n")
		line = strings.ReplaceAll(line, `"`, `\"`)
		fmt.Fprintf(&entries, "%d \"%s\"\n", tsNano, line)
	}

	payload := map[string]any{
		"streams": []map[string]any{
			{
				"stream": map[string]string{
					"app":      rows[0].App,
					"container": rows[0].Container,
					"container_id": rows[0].ContainerID,
					"host_id": rows[0].HostID,
				},
				"entries": strings.Split(entries.String(), "\n"),
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.lokiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.hubClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.ReadAll(resp.Body)
		return fmt.Errorf("loki: HTTP %d", resp.StatusCode)
	}
	return nil
}

// pushES sends rows to Elasticsearch using the Bulk API.
func (f *Forwarder) pushES(ctx context.Context, rows []store.LogRow) error {
	if len(rows) == 0 {
		return nil
	}

	var buf bytes.Buffer
	index := f.esIndex
	if index == "" {
		index = "capscope-logs"
	}

	for _, r := range rows {
		// Index action metadata
		meta := map[string]any{
			"index": map[string]any{
				"_index": index,
				"_id":    fmt.Sprintf("%d", r.ID),
			},
		}
		metaBytes, _ := json.Marshal(meta)
		buf.Write(metaBytes)
		buf.WriteByte('\n')

		// Document
		doc := map[string]any{
			"@timestamp":     r.TS / 1_000_000, // ms
			"ts":             r.TS,
			"container_id":   r.ContainerID,
			"container":      r.Container,
			"app":            r.App,
			"host_id":        r.HostID,
			"stream":         r.Stream,
			"level":          r.Level,
			"line":           r.Line,
		}
		docBytes, _ := json.Marshal(doc)
		buf.Write(docBytes)
		buf.WriteByte('\n')
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.esURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := f.hubClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.ReadAll(resp.Body)
		return fmt.Errorf("elasticsearch: HTTP %d", resp.StatusCode)
	}
	return nil
}

// hubClient returns a shared HTTP client.
func (f *Forwarder) hubClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}
