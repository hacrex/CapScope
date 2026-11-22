# CapScope — Remaining Work (from docs review)

Source docs reviewed: `README.md`, `CHANGELOG.md`, `PRD.md`, `GAP-ANALYSIS.md`, `ROADMAP-DOZZLE-PARITY.md`, `ARCHITECTURE.md`, `API.md`, `UI-UX-DESIGN.md`, `TEAM-ENHANCEMENT-SUMMARY.md`, `SECURITY.md`, `CONTRIBUTING.md`, `IMPLEMENTATION_PLAN.md`, `deploy/*`, `docs/CAPROVER.md`.

> Status key: `☐` not started · `▶` in progress · `✅` done · `🔒` externally gated (needs real Docker/CapRover host)

---

## 0. Docs sync (this session)

- [ ] `README.md` roadmap table — reconcile "done" items that GAP-ANALYSIS/PRD mark not done
- [ ] `GAP-ANALYSIS.md` — flip statuses to match actual code state
- [ ] `PRD.md` §11 acceptance checklist — mark items pending/verified
- [ ] `CHANGELOG.md` Unreleased — note this bug fix

---

## 1. Acceptance / verification (externally gated) — PRD §11

- [ ] Verify against a real Docker daemon + CapRover Swarm (staging deploy) 🔒
- [ ] Build & run Docker image on CapRover 🔒
- [ ] One-week soak test with memory/disk measurements 🔒
- [ ] Redeploy latest source to live env (`127.0.0.1:18080` serves an older binary) — TEAM-ENHANCEMENT-SUMMARY

---

## 2. Multi-node / platforms (biggest gap vs Dozzle) — GAP-ANALYSIS §1

- [ ] Agent mode (lightweight TLS agent streaming logs/stats) — PRD C10
- [ ] Swarm cross-node visibility (node grouping by `com.docker.swarm.node.id`)
- [ ] Kubernetes collector path
- [ ] Podman: verify + document, `remoteSocket` fallback in `docker.New`
- [ ] Host-level metrics (host CPU/mem/disk in UI)

---

## 3. Auth & multi-user

- [ ] OIDC / GitHub OAuth — PRD Z5
- [ ] Multi-user + per-user audit trail — PRD Z4
- [ ] Persistent signed session secret (survive restarts; today in-memory)

---

## 4. Crash / health features — PRD "Not done"

- [ ] **Snapshot on health-check failure** — PRD X8 (next implementation item)
- [ ] Healthcheck state surfaced in UI — GAP-ANALYSIS §4

---

## 5. Access logs — PRD C9.7

- [ ] Per-request app attribution (routing/host map)
- [ ] GeoIP country labels on client IPs

---

## 6. Storage (deferred per PRD S6 rationale)

- [ ] Row normalization (S6) — revisit only with real-world evidence row size is the limiter

---

## 7. IMPLEMENTATION_PLAN.md — Phases 0 → 7 (v0.15 → v1.0)

- [ ] Phase 0 — Foundation: telemetry schema, OTel proto bindings, config additions (`OTEL_LISTEN`, `PIPELINES_FILE`, `ARCHIVE_BUCKET`, `RETENTION_HOT_DAYS`), feature gates
- [ ] Phase 1 — OTLP ingestion: OTLP HTTP/gRPC receiver, Docker agent → OTel bridge, OTLP auth & RBAC
- [ ] Phase 2 — Logs → Metrics pipelines: `pipelines.yaml`, transform engine, backfill, dashboard consumption
- [ ] Phase 3a — Trace storage + API (`/api/traces`)
- [ ] Phase 3b — Trace UI: trace list, waterfall/flame graph, click log → trace, service graph
- [ ] Phase 4 — Storage tiering: Parquet archive rollup, tiered query path, retention policies
- [ ] Phase 5 — Incident management: incident model, auto-create/auto-resolve, incident UI
- [ ] Phase 6 — Dashboard richness & AI/LLM observability: dashboard builder, LLM span attributes, PromQL, MCP trace queries
- [ ] Phase 7 — Security & enterprise readiness: audit log, fine-grained RBAC, ingest-time redaction, query QoS, SSO/SAML/LDAP

---

## 8. UI/UX design proposals (UI-UX-DESIGN.md — v0.3/v1.0, not yet implemented)

- [ ] Login screen + session token (P0)
- [ ] Design tokens + brand colors (Docker orange) (P0)
- [ ] Global dashboard with stat row + container grid (P1)
- [ ] Live streaming metrics charts with SSE + tooltips + PNG export (P1–P2)
- [ ] Sidebar search + filters + pin/star (P2)
- [ ] Settings UI page (P3)
- [ ] Alert rule builder UI + channel tester (P3)
- [ ] Log bookmarks + split-pane diff (P4)
- [ ] Crash timeline + one-click restart (P4) — conflicts with read-only stance; needs decision

---

## How to consume

Work top-to-bottom. Each item: implement → test (`go vet ./...`, `go test ./...`) → commit → push → move to next. Docs-synced items in §0 are done first so the remaining list reflects reality.
