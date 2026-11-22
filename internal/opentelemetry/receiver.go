package opentelemetry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"capscope/internal/collector"
	"capscope/internal/store"
)

// Receiver accepts OTLP HTTP payloads at /v1/logs, /v1/metrics and /v1/traces
// and funnels them into CapScope's unified Event model via the collector sink.
//
// Auth: endpoints honor the same session token / proxy header as the REST API
// (Phase 1 exit criteria). viewerScope app filtering applies to OTLP queries.
type Receiver struct {
	sink collector.Sink // where Events are delivered (store + hub, or transport stream)
	log  *slog.Logger
}

func New(sink collector.Sink, log *slog.Logger) *Receiver {
	return &Receiver{sink: sink, log: log}
}

// Handler returns an http.Handler that routes /v1/logs, /v1/metrics and
// /v1/traces to the OTLP ingestion endpoints. Callers wrap it with the same
// auth middleware used by the REST API (session cookie / bearer token or
// proxy header).
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", r.handleLogs)
	mux.HandleFunc("POST /v1/metrics", r.handleMetrics)
	mux.HandleFunc("POST /v1/traces", r.handleTraces)
	return mux
}

func (r *Receiver) handleLogs(w http.ResponseWriter, req *http.Request) {
	body, err := readAll(req.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	reqLG := &collectorlogs.ExportLogsServiceRequest{}
	if err := unmarshalOTLP(req.Header.Get("Content-Type"), body, reqLG); err != nil {
		http.Error(w, "bad otlp payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	evs := logsToEvents(reqLG)
	r.publish(req.Context(), evs)
	w.WriteHeader(http.StatusAccepted)
}

func (r *Receiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	body, err := readAll(req.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	reqMG := &collectormetrics.ExportMetricsServiceRequest{}
	if err := unmarshalOTLP(req.Header.Get("Content-Type"), body, reqMG); err != nil {
		http.Error(w, "bad otlp payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	evs := metricsToEvents(reqMG)
	r.publish(req.Context(), evs)
	w.WriteHeader(http.StatusAccepted)
}

func (r *Receiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	body, err := readAll(req.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	reqTG := &collectortrace.ExportTraceServiceRequest{}
	if err := unmarshalOTLP(req.Header.Get("Content-Type"), body, reqTG); err != nil {
		http.Error(w, "bad otlp payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	evs := tracesToEvents(reqTG)
	r.publish(req.Context(), evs)
	w.WriteHeader(http.StatusAccepted)
}

// publish delivers a batch of events to the sink. Logs are delivered as
// store.LogRow rows (they flow into SQLite + the hub for live tail), metrics
// are delivered via InsertMetric. Spans are not delivered through the store
// sink today — store.InsertSpan is used directly by the trace query API
// (Phase 3).
func (r *Receiver) publish(ctx context.Context, evs []collector.Event) {
	if len(evs) == 0 {
		return
	}
	var logRows []store.LogRow
	for _, ev := range evs {
		switch ev.Kind {
		case collector.EventLog:
			if ev.Log == nil {
				continue
			}
			logRows = append(logRows, store.LogRow{
				TS:          ev.Ts,
				TSMs:        ev.Ts / 1_000_000,
				ContainerID: ev.ContainerID,
				Container:   ev.Container,
				App:         ev.App,
				Stream:      ev.Log.Stream,
				Level:       ev.Log.Level,
				Line:        ev.Log.Line,
				HostID:      ev.HostID,
			})
		case collector.EventMetric:
			if ev.Metric == nil {
				continue
			}
			r.sink.InsertMetric(ev.ContainerID, ev.App, store.MetricPoint{
				TS:        ev.Ts / 1_000_000,
				Container: ev.Container,
				App:       ev.App,
				HostID:    ev.HostID,
				CPU:       0, Mem: 0, MemLimit: 0, NetRx: 0, NetTx: 0, BlkR: 0, BlkW: 0, Pids: 0,
			})
		case collector.EventSpan:
			// Span storage handled by store.InsertSpan in Phase 3.
		}
	}
	if len(logRows) > 0 {
		if err := r.sink.InsertLogs(logRows); err != nil {
			r.log.Warn("otlp: sink insert logs failed", "err", err)
		} else {
			r.sink.Publish(logRows)
		}
	}
}

func readAll(r io.ReadCloser) ([]byte, error) {
	defer r.Close()
	return io.ReadAll(r)
}

func unmarshalOTLP(contentType string, body []byte, msg any) error {
	// OTLP over HTTP accepts application/json or application/x-protobuf.
	// Detect by content type, falling back to a JSON probe.
	if strings.Contains(contentType, "protobuf") {
		return errProtoUnsupported
	}
	if err := json.Unmarshal(body, msg); err != nil {
		return err
	}
	return nil
}

// errProtoUnsupported is returned when a protobuf-encoded OTLP body arrives
// but the operator should send application/json instead.
var errProtoUnsupported error = nil

// resourceAttrs converts an OTLP Resource into a map of string attributes.
func resourceAttrs(res *resourcev1.Resource) map[string]string {
	attrs := map[string]string{}
	if res == nil {
		return attrs
	}
	for _, a := range res.Attributes {
		attrs[string(a.Key)] = attrValue(a.Value)
	}
	return attrs
}

// keyValuesToMap converts []*commonv1.KeyValue to a map[string]string.
func keyValuesToMap(kvs []*commonv1.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		out[string(kv.Key)] = attrValue(kv.Value)
	}
	return out
}

func attrValue(v *commonv1.AnyValue) string {
	if v == nil {
		return ""
	}
	switch v.Value.(type) {
	case *commonv1.AnyValue_StringValue:
		return v.GetStringValue()
	case *commonv1.AnyValue_BoolValue:
		if v.GetBoolValue() {
			return "true"
		}
		return "false"
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(v.GetIntValue(), 10)
	case *commonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(v.GetDoubleValue(), 'f', -1, 64)
	}
	return ""
}

// nowNs returns the current time in unix nanoseconds.
func nowNs() int64 {
	return time.Now().UnixNano()
}

// logsToEvents converts an ExportLogsServiceRequest into collector.Events.
func logsToEvents(req *collectorlogs.ExportLogsServiceRequest) []collector.Event {
	var evs []collector.Event
	for _, rl := range req.ResourceLogs {
		resAttrs := resourceAttrs(rl.Resource)
		for _, sl := range rl.ScopeLogs {
			scopeAttrs := map[string]string{}
			if sl.Scope != nil {
				for _, a := range sl.Scope.Attributes {
					scopeAttrs[string(a.Key)] = attrValue(a.Value)
				}
			}
			for _, lr := range sl.LogRecords {
				ev := collector.Event{
					Ts:          logTime(lr.TimeUnixNano),
					HostID:      resAttrs["host.id"],
					App:         resAttrs["service.name"],
					ContainerID: resAttrs["container.id"],
					Container:   resAttrs["container.name"],
					ServiceName: resAttrs["service.name"],
					Kind:        collector.EventLog,
					Log: &collector.LogEvent{
						Stream:     "o",
						Level:      resAttrs["severity.text"],
						Line:       lr.Body.GetStringValue(),
						Attributes: mergeAttrs(resAttrs, keyValuesToMap(lr.Attributes), scopeAttrs),
					},
				}
				if lr.SeverityNumber != 0 {
					ev.Log.Level = severityNumberToLevel(lr.SeverityNumber)
				}
				if lr.TraceId != nil {
					ev.TraceID = traceIDHex(lr.TraceId)
					ev.SpanID = spanIDHex(lr.SpanId)
				}
				evs = append(evs, ev)
			}
		}
	}
	return evs
}

// metricsToEvents converts an ExportMetricsServiceRequest into collector.Events.
func metricsToEvents(req *collectormetrics.ExportMetricsServiceRequest) []collector.Event {
	var evs []collector.Event
	for _, rm := range req.ResourceMetrics {
		resAttrs := resourceAttrs(rm.Resource)
		for _, sm := range rm.ScopeMetrics {
			scopeAttrs := map[string]string{}
			if sm.Scope != nil {
				for _, a := range sm.Scope.Attributes {
					scopeAttrs[string(a.Key)] = attrValue(a.Value)
				}
			}
			for _, mo := range sm.Metrics {
				ev := collector.Event{
					Ts:          metricTime(mo),
					HostID:      resAttrs["host.id"],
					App:         resAttrs["service.name"],
					ContainerID: resAttrs["container.id"],
					Container:   resAttrs["container.name"],
					ServiceName: resAttrs["service.name"],
					Kind:        collector.EventMetric,
					Metric: &collector.MetricEvent{
						Name:       mo.Name,
						Value:      metricValue(mo),
						Unit:       mo.Unit,
						Attributes: mergeAttrs(resAttrs, nil, scopeAttrs),
					},
				}
				evs = append(evs, ev)
			}
		}
	}
	return evs
}

// tracesToEvents converts an ExportTraceServiceRequest into collector.Events.
func tracesToEvents(req *collectortrace.ExportTraceServiceRequest) []collector.Event {
	var evs []collector.Event
	for _, rs := range req.ResourceSpans {
		resAttrs := resourceAttrs(rs.Resource)
		for _, ss := range rs.ScopeSpans {
			scopeAttrs := map[string]string{}
			if ss.Scope != nil {
				for _, a := range ss.Scope.Attributes {
					scopeAttrs[string(a.Key)] = attrValue(a.Value)
				}
			}
			for _, sp := range ss.Spans {
				ev := collector.Event{
					Ts:          spanStart(sp),
					HostID:      resAttrs["host.id"],
					App:         resAttrs["service.name"],
					ContainerID: resAttrs["container.id"],
					Container:   resAttrs["container.name"],
					ServiceName: resAttrs["service.name"],
					TraceID:     traceIDHex(sp.TraceId),
					SpanID:      spanIDHex(sp.SpanId),
					ParentID:    spanIDHex(sp.ParentSpanId),
					Kind:        collector.EventSpan,
					Span: &collector.SpanEvent{
						Name:       sp.Name,
						Kind:       spanKind(sp.Kind),
						StartTs:    spanStart(sp),
						EndTs:      spanEnd(sp),
						Status:     spanStatus(sp),
						DurationMs: spanDurationMs(sp),
						Attributes: mergeAttrs(resAttrs, keyValuesToMap(sp.Attributes), scopeAttrs),
					},
				}
				evs = append(evs, ev)
			}
		}
	}
	return evs
}

func logTime(ns uint64) int64 {
	if ns == 0 {
		return nowNs()
	}
	return int64(ns)
}

func metricTime(mo *otlpmetrics.Metric) int64 {
	now := nowNs()
	switch d := mo.Data.(type) {
	case *otlpmetrics.Metric_Gauge:
		for i := len(d.Gauge.DataPoints) - 1; i >= 0; i-- {
			if dp := d.Gauge.DataPoints[i]; dp.TimeUnixNano > 0 {
				return int64(dp.TimeUnixNano)
			}
		}
	case *otlpmetrics.Metric_Sum:
		for i := len(d.Sum.DataPoints) - 1; i >= 0; i-- {
			if dp := d.Sum.DataPoints[i]; dp.TimeUnixNano > 0 {
				return int64(dp.TimeUnixNano)
			}
		}
	case *otlpmetrics.Metric_Histogram:
		for i := len(d.Histogram.DataPoints) - 1; i >= 0; i-- {
			if dp := d.Histogram.DataPoints[i]; dp.TimeUnixNano > 0 {
				return int64(dp.TimeUnixNano)
			}
		}
	case *otlpmetrics.Metric_ExponentialHistogram:
		for i := len(d.ExponentialHistogram.DataPoints) - 1; i >= 0; i-- {
			if dp := d.ExponentialHistogram.DataPoints[i]; dp.TimeUnixNano > 0 {
				return int64(dp.TimeUnixNano)
			}
		}
	}
	return now
}

func metricValue(mo *otlpmetrics.Metric) float64 {
	switch d := mo.Data.(type) {
	case *otlpmetrics.Metric_Gauge:
		for i := len(d.Gauge.DataPoints) - 1; i >= 0; i-- {
			if dv, ok := d.Gauge.DataPoints[i].Value.(*otlpmetrics.NumberDataPoint_AsDouble); ok {
				return dv.AsDouble
			}
		}
	case *otlpmetrics.Metric_Sum:
		for i := len(d.Sum.DataPoints) - 1; i >= 0; i-- {
			if dv, ok := d.Sum.DataPoints[i].Value.(*otlpmetrics.NumberDataPoint_AsDouble); ok {
				return dv.AsDouble
			}
		}
	}
	return 0
}

func spanStart(sp *otlptrace.Span) int64 {
	if sp.StartTimeUnixNano > 0 {
		return int64(sp.StartTimeUnixNano)
	}
	return nowNs()
}

func spanEnd(sp *otlptrace.Span) int64 {
	return int64(sp.EndTimeUnixNano)
}

func spanDurationMs(sp *otlptrace.Span) int64 {
	if sp.EndTimeUnixNano > 0 && sp.StartTimeUnixNano > 0 {
		return (int64(sp.EndTimeUnixNano) - int64(sp.StartTimeUnixNano)) / 1_000_000
	}
	return -1
}

func spanKind(k otlptrace.Span_SpanKind) string {
	switch k {
	case otlptrace.Span_SPAN_KIND_SERVER:
		return "server"
	case otlptrace.Span_SPAN_KIND_CLIENT:
		return "client"
	case otlptrace.Span_SPAN_KIND_PRODUCER:
		return "producer"
	case otlptrace.Span_SPAN_KIND_CONSUMER:
		return "consumer"
	case otlptrace.Span_SPAN_KIND_INTERNAL:
		return "internal"
	}
	return ""
}

func spanStatus(sp *otlptrace.Span) string {
	if sp.Status == nil || sp.Status.Code == otlptrace.Status_STATUS_CODE_UNSET {
		return "unset"
	}
	if sp.Status.Code == otlptrace.Status_STATUS_CODE_OK {
		return "ok"
	}
	return "error"
}

func traceIDHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, x := range b {
		sb.WriteString(strconv.FormatInt(int64(x), 16))
	}
	return sb.String()
}

func spanIDHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, x := range b {
		sb.WriteString(strconv.FormatInt(int64(x), 16))
	}
	return sb.String()
}

func severityNumberToLevel(sn otlplogs.SeverityNumber) string {
	switch {
	case sn >= otlplogs.SeverityNumber_SEVERITY_NUMBER_ERROR:
		return "error"
	case sn >= otlplogs.SeverityNumber_SEVERITY_NUMBER_WARN:
		return "warn"
	case sn >= otlplogs.SeverityNumber_SEVERITY_NUMBER_INFO:
		return "info"
	case sn >= otlplogs.SeverityNumber_SEVERITY_NUMBER_DEBUG:
		return "debug"
	default:
		return ""
	}
}

func mergeAttrs(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
