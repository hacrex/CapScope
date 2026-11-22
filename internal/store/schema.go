// Package store extends the SQLite schema with distributed-trace span storage
// (Phase 3 of the implementation plan). Spans live alongside logs and metrics
// in the same hot SQLite DB so trace-to-log correlation is a single JOIN away.
//
// Schema note: the spans table is applied by migration 15 in store.go
// (PRAGMA user_version). This file holds the Span type and the InsertSpan /
// QuerySpans accessors used by the OTLP receiver and trace query API.
package store

import (
	"context"
	"encoding/json"
	"time"
)

// Span is a stored trace span.
//
// Columns map to the Dapper/OTel span model:
//   - TraceID / SpanID / ParentSpanID identify the span in its trace tree.
//   - Service is the OTel service.name.
//   - Name is the span operation name.
//   - Kind is the span kind (server, client, producer, consumer, internal).
//   - StartTs / EndTs are unix nanoseconds; EndTs = 0 means still open.
//   - Status is ok / error / unset.
//   - Attributes is a JSON blob of span attributes.
type Span struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind,omitempty"`
	StartTs      int64             `json:"start_ts"` // unix nanoseconds
	EndTs        int64             `json:"end_ts"`   // unix nanoseconds (0 = still open)
	Status       string            `json:"status"`   // ok | error | unset
	Attributes   map[string]string `json:"attributes,omitempty"`
	HostID       string            `json:"host_id,omitempty"`
}

// InsertSpan stores a single span. Returns the new row id or an error.
// Span attributes are stored as a JSON blob in the attributes column.
func (s *Store) InsertSpan(ctx context.Context, sp Span) (int64, error) {
	attrs, err := json.Marshal(sp.Attributes)
	if err != nil {
		return 0, err
	}
	res, err := s.w.ExecContext(ctx, `
		INSERT INTO spans(trace_id, span_id, parent_span_id, service, name, kind,
		                  start_ts, end_ts, status, attributes, host_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sp.TraceID, sp.SpanID, sp.ParentSpanID, sp.Service, sp.Name, sp.Kind,
		sp.StartTs, sp.EndTs, sp.Status, string(attrs), sp.HostID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// QuerySpansByTrace returns all spans of a trace ordered by start time, used
// to render the trace waterfall / span tree in the UI.
func (s *Store) QuerySpansByTrace(ctx context.Context, traceID string) ([]Span, error) {
	return s.querySpans(ctx, `WHERE trace_id = ? ORDER BY start_ts`, traceID)
}

// QuerySpansByService returns spans for a service, newest first, limited to
// `limit` rows. Used for the trace list (Dapper-style) view.
func (s *Store) QuerySpansByService(ctx context.Context, service string, limit int) ([]Span, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.querySpans(ctx, `WHERE service = ? ORDER BY start_ts DESC LIMIT ?`, service, limit)
}

func (s *Store) querySpans(ctx context.Context, where string, args ...any) ([]Span, error) {
	q := `SELECT trace_id, span_id, parent_span_id, service, name, kind,
	             start_ts, end_ts, status, attributes, host_id
	      FROM spans ` + where
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Span
	for rows.Next() {
		var sp Span
		var attrsJSON string
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.Service,
			&sp.Name, &sp.Kind, &sp.StartTs, &sp.EndTs, &sp.Status,
			&attrsJSON, &sp.HostID); err != nil {
			return nil, err
		}
		if attrsJSON != "" {
			if err := json.Unmarshal([]byte(attrsJSON), &sp.Attributes); err != nil {
				sp.Attributes = nil
			}
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SpanDurationMs returns the span duration in milliseconds, or -1 when the
// span has no end time.
func (s Span) SpanDurationMs() int64 {
	if s.EndTs <= 0 || s.StartTs <= 0 {
		return -1
	}
	return (s.EndTs - s.StartTs) / 1_000_000
}

// ensureMilli normalizes a unix-nanosecond timestamp to unix milliseconds for
// JSON output where callers need ms precision.
func ensureMilli(ns int64) int64 {
	return ns / 1_000_000
}

// nowNs returns the current time in unix nanoseconds.
func nowNs() int64 {
	return time.Now().UnixNano()
}
