// Package collector defines the common internal telemetry event model used by
// the OTLP receiver (Phase 1), the logs-to-metrics pipeline engine (Phase 2),
// trace storage (Phase 3), and the Parquet archive (Phase 4).
//
// One Event carries a single signal: a log line, a metric sample, a trace
// span, or a container lifecycle event. All signal types share a common
// envelope so downstream stages can treat them uniformly regardless of origin
// (Docker collector, OTLP receiver, or agent hub).
package collector

import "time"

// EventKind is the type of signal carried by an Event.
type EventKind int

const (
	// EventLog is a log line (from Docker stdout/stderr or OTLP logs).
	EventLog EventKind = iota
	// EventMetric is a metric sample (from Docker stats or OTLP metrics).
	EventMetric
	// EventSpan is a trace span (from OTLP traces).
	EventSpan
	// EventContainerEvent is a Docker lifecycle event (die, oom,
	// health_status, update_available, start, stop).
	EventContainerEvent
)

// Event is the common internal telemetry event model.
//
// Common envelope fields (populated for every signal):
//   - Ts: event time in unix nanoseconds.
//   - HostID: stable host identity (agent mode) or empty in serve mode.
//   - App: application name derived from container labels.
//   - ContainerID / Container: Docker container id and name.
//   - ServiceName: OTel service.name attribute, mapped from app/container.
//   - TraceID / SpanID / ParentSpanID: trace context, present on logs and
//     spans when the source emitted trace propagation headers.
type Event struct {
	Ts          int64
	HostID      string
	App         string
	ContainerID string
	Container   string
	ServiceName string

	TraceID  string
	SpanID   string
	ParentID string

	Kind EventKind

	// Exactly one of Log, Metric, Span, ContainerEvent is non-nil, matching Kind.
	Log          *LogEvent
	Metric       *MetricEvent
	Span         *SpanEvent
	ContainerEvt *ContainerEventData
}

// LogEvent is a log line.
type LogEvent struct {
	Stream     string // "o" stdout, "e" stderr
	Level      string // error, warn, info, debug, or ""
	Line       string
	Attributes map[string]string
}

// MetricEvent is a metric sample.
type MetricEvent struct {
	Name       string
	Value      float64
	Unit       string // e.g. "percent", "bytes", "milliseconds"
	Attributes map[string]string
}

// SpanEvent is a trace span.
type SpanEvent struct {
	Name       string
	Kind       string // server, client, producer, consumer, internal
	StartTs    int64  // unix nanoseconds
	EndTs      int64  // unix nanoseconds (0 = still open)
	Status     string // ok, error, unset
	DurationMs int64  // derived: (EndTs-StartTs)/1e6 when EndTs > 0
	Attributes map[string]string
}

// ContainerEventData is a Docker lifecycle event.
type ContainerEventData struct {
	Event        string // die, oom, health_status, update_available, start, stop
	ExitCode     int
	OOMKilled    bool
	HealthStatus string
	Timestamp    int64 // unix nanoseconds (defaults to Event.Ts when 0)
}

// EventTime returns the event's timestamp as a time.Time.
func (e Event) EventTime() time.Time {
	return time.Unix(0, e.Ts).UTC()
}

// SpanDurationMs returns the span duration in milliseconds, or -1 when the
// span has no end time.
func (s SpanEvent) SpanDurationMs() int64 {
	if s.EndTs <= 0 || s.StartTs <= 0 {
		return -1
	}
	return (s.EndTs - s.StartTs) / 1_000_000
}
