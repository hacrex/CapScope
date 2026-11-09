# CapScope Implementation Plan

Inspired by OpenObserve (logs + metrics + traces + RUM, Parquet/S3 storage, single binary, OTel-native).

**Goal:** Keep CapScope's single-binary, Docker-socket simplicity while adding the observability signals (traces, metrics pipelines, rich dashboards) that make it a real platform — not just a log viewer.

---

## Phase 0 — Foundation (prerequisites for everything else)

Establish the shared pieces that later phases depend on.

| Item | Description | Files |
|------|-------------|-------|
| **Telemetry schema** | Common internal event model (log, metric, trace span, event) with `trace_id`, `span_id`, `parent_span_id`, `service.name`, `host_id`, `app`, `container_id`. | `internal/collector/types.go`, `internal/store/schema.go` |
| **OTel proto bindings** | Generate Go from OTLP protobufs (logs, metrics, traces) for the receiver. | `api/opentelemetry/proto/*.go` |
| **Config additions** | `OTEL_LISTEN`, `OTEL_PATH`, `PIPELINES_FILE`, `ARCHIVE_BUCKET`, `ARCHIVE_PREFIX`, `RETENTION_HOT_DAYS`. | `internal/config/config.go` |
| **Feature gates** | `capscope features` subcommand / env flags so new signals can ship behind gates and graduate to GA. | `cmd/capscope/features.go` |

**Exit criteria:** `go build ./...` clean, new config loads, no runtime change to existing behavior.

---

## Phase 1 — OTLP ingestion (open the funnel beyond Docker)

Let any OTel-instrumented workload (or host agent) send logs/metrics/traces to CapScope, not just Docker containers.

1. **OTLP HTTP/gRPC receiver** (`internal/opentelemetry/receiver.go`)
   - HTTP endpoint `/v1/logs`, `/v1/metrics`, `/v1/traces` (and optional gRPC).
   - Accept OTLP JSON/protobuf payloads.
   - Map OTel resource attributes (`service.name`, `host.name`, `container.id`) → CapScope `app` / `host_id` / `container_id`.
   - Reuse the existing `Store.InsertLogs` / `InsertMetric` write paths so storage stays unified.

2. **Docker agent → OTel bridge** (optional but nice)
   - The existing Docker agent already emits LogBatch/MetricBatch/EventBatch over the hub-agent mTLS link. Add an internal OTLP exporter so the agent can forward to an OTel collector if the user already runs one, without duplicating the CapScope agent.

3. **Auth & RBAC**
   - OTLP endpoints honor the same session token / proxy header as the REST API.
   - `viewerScope` app filtering applies to OTLP log/metric queries.

**Exit criteria:** `curl` OTLP logs into CapScope and see them in `/api/logs` and live tail.

---

## Phase 2 — Logs → Metrics pipelines (ingest-time rollups)

OpenObserve's pipeline editor converts logs into metrics at ingest. CapScope gets a lightweight code-driven version.

1. **Pipeline definition** (`internal/pipeline/pipeline.go`)
   - YAML file: `pipelines.yaml` with rules like:
     ```yaml
     - name: error_rate
       match: level == "error"
       metric: app_errors_total
       type: counter
       group_by: [app, container]
     ```
   - Hot-reload on file change (mirror `alerts.Engine.reload`).

2. **Transform engine**
   - Match log lines by level/regex/app/container.
   - Emit `MetricPoint` rows into the same `metrics` table used by dashboards and alert rules.
   - Backfill: optional one-time scan of existing logs to populate the new metric.

3. **Dashboard consumption**
   - Pipeline-derived metrics appear in `/api/metrics` and the dashboard sparkline cards automatically.

**Exit criteria:** A `pipelines.yaml` rule creates a queryable metric visible in the metrics explorer.

---

## Phase 3 — Distributed tracing (the biggest gap)

CapScope has no trace signal today. Add it end-to-end.

1. **OTLP traces receiver** (part of Phase 1 receiver)
   - Store spans in SQLite: `spans(trace_id, span_id, parent_span_id, service, name, start_ts, end_ms, status, kind, attributes JSON)`.
   - Index on `trace_id` and `service`.

2. **Trace query API**
   - `GET /api/traces?trace_id=…` → full trace with span tree.
   - `GET /api/traces?service=…&min_duration=…&error=true` → trace list (Dapper-style).

3. **Trace UI views** (`internal/web/static/index.html`)
   - Trace list table.
   - Trace waterfall / flame graph (span timeline by start time).
   - Click a log line → show its trace (inject `trace_id` into log records when present).

4. **Service graph**
   - Infer edges from span parent/child relationships or from container network logs.
   - Render per-edge request count + error rate + latency, colored by health.

**Exit criteria:** Send a trace via OTLP, view the span waterfall in the UI.

---

## Phase 4 — Storage tiering (scale past SQLite)

SQLite is perfect for hot data on one node. Add cheap long-term retention without rebuilding in Rust.

1. **Parquet archive** (`internal/store/archive.go`)
   - Background job rolls `logs` / `metrics` / `spans` older than `RETENTION_HOT_DAYS` to Parquet files in `ARCHIVE_BUCKET` (S3 / MinIO / local disk).
   - Partition by day + app/host for pruning efficiency.
   - Keep SQLite index (time range + location) so queries know which Parquet files to read.

2. **Tiered query path**
   - `QueryLogs` / `QueryMetrics` read SQLite first, then fall back to Parquet for older ranges.
   - `MAX_DB_MB` enforcement now only applies to the hot SQLite DB; archived data is exempt.

3. **Retention policies**
   - Per-app or global: hot days, archive days, delete after.

**Exit criteria:** 30-day-old logs readable via the same API, stored as Parquet off the hot disk.

---

## Phase 5 — Incident management & service map

Turn raw alerts into operational workflow.

1. **Incident model** (`internal/incidents/incident.go`)
   - Incident: `id, title, severity, state(open/ack/resolved), started_at, ended_at, related_alert_ids, dimensions(app, host)`.
   - Auto-create from alert rule firings; auto-resolve when the rule stops firing for `N` consecutive evaluations.
   - Manual open/ack/resolve via API and UI.

2. **Incident UI**
   - Incident list, detail page with timeline (alerts → snapshot links → log links).
   - Incident-to-trace and incident-to-log navigation.

3. **Service graph** (from Phase 3)
   - Nodes = services/containers; edges = inferred calls with health coloring.
   - Click edge → related logs/metrics/traces.

**Exit criteria:** An alert firing creates an incident; resolving it closes the incident; incident detail links to logs/traces.

---

## Phase 6 — Dashboard richness & AI/LLM observability

Make the UI feel like a platform, and lean into CapScope's existing MCP server.

1. **Dashboard builder** (`internal/web/static/index.html`)
   - Template variables for `app` / `host` / `service`.
   - More panel types: time series, stat, list, heatmap (metrics + logs + traces).
   - Drag-and-drop panel reorder (simple grid layout).

2. **AI/LLM observability** (niche differentiator)
   - Track LLM-specific attributes on spans/logs: `llm.model`, `llm.tokens.in`, `llm.tokens.out`, `llm.latency`, `llm.eval_score`.
   - Dashboard panels: cost per model, token rate, error rate, eval quality over time.
   - Since CapScope already ships an MCP server (`run_sql`, `query_logs`), document how an LLM assistant can query CapScope traces/logs via MCP.

3. **Query language breadth**
   - Add PromQL-compatible metric queries alongside existing SQL (`GET /api/metrics/promql?query=…`).

**Exit criteria:** Dashboard with template variables + a trace waterfall panel; LLM span attributes visible in UI; MCP assistant can query traces.

---

## Phase 7 — Security & enterprise readiness

| Item | Description |
|------|-------------|
| **Audit log** | Append-only `audit_log` table: actor, action, resource, target, timestamp. Every `/api/sql`, `/api/alerts/rules`, role change is recorded. |
| **Fine-grained RBAC** | Per-app / per-host scopes beyond the current viewer/admin split. |
| **Sensitive data redaction** | Ingest-time regex redaction (credit cards, tokens) — reuse `logproc.Redact`. |
| **Query QoS** | Timeout + row caps per user role (admins get higher limits). |
| **SSO/SAML/LDAP** | Extend OIDC support to SAML/LDAP for enterprise deployments. |

**Exit criteria:** Audit trail visible in UI; redaction rules configurable; query limits enforced per role.

---

## Release cadence suggestion

| Release | Content |
|---------|---------|
| **v0.15** | Phase 0 + Phase 1 (OTLP receiver) |
| **v0.16** | Phase 2 (pipelines) + Phase 3a (trace storage + API) |
| **v0.17** | Phase 3b (trace UI + service graph) |
| **v0.18** | Phase 4 (Parquet archive) |
| **v0.19** | Phase 5 (incidents) |
| **v1.0**  | Phase 6 + Phase 7 (rich dashboards, LLM obs, audit/RBAC) |

---

## Risks & mitigations

| Risk | Mitigation |
|------|------------|
| **SQLite concurrency** | Writes already serialized via `s.w` (`SetMaxOpenConns(1)`). Archive writes happen on a single background goroutine. |
| **Parquet dependency** | Start with local-file Parquet via `github.com/apache/arrow/go` or even newline-delimited JSON in object storage; upgrade to real Parquet when needed. |
| **Trace storage size** | Index only `trace_id` + `service`; store span attributes as JSON blob. Cap trace retention separately from logs. |
| **UI complexity** | Keep the existing single-file `index.html` model; add new views incrementally behind feature gates. |

---

## OpenObserve comparison cheat sheet

| Capability | CapScope today | After this plan | OpenObserve |
|------------|----------------|-----------------|-------------|
| Logs | ✅ SQLite | ✅ + archive tier | ✅ Parquet/S3 |
| Metrics | ✅ Docker stats | ✅ + pipelines + PromQL | ✅ SQL + PromQL |
| Traces | ❌ | ✅ OTLP + waterfall | ✅ OTLP + flame graphs |
| RUM | ❌ | ❌ (future) | ✅ |
| Pipelines | ❌ | ✅ log→metric | ✅ VRL visual editor |
| Dashboards | Basic cards | ✅ template vars + panels | ✅ 19+ chart types |
| Incidents | ❌ | ✅ | ✅ |
| Service graph | ❌ | ✅ | ✅ |
| AI/LLM obs | MCP server only | ✅ LLM attributes + dashboards | ✅ AI assistant |
| Deployment | Single binary | Single binary | Single binary / cluster |
| Storage | SQLite | SQLite + Parquet/S3 | Parquet + S3 |

---

**Next step:** Pick Phase 0 (telemetry schema + config) and Phase 1 (OTLP receiver) as the first sprint. Everything else builds on that foundation.
