# CapScope — Remaining Work (from docs review)

Source docs reviewed: `README.md`, `CHANGELOG.md`, `PRD.md`, `GAP-ANALYSIS.md`, `ROADMAP-DOZZLE-PARITY.md`, `ARCHITECTURE.md`, `API.md`, `UI-UX-DESIGN.md`, `TEAM-ENHANCEMENT-SUMMARY.md`, `SECURITY.md`, `CONTRIBUTING.md`, `IMPLEMENTATION_PLAN.md`, `deploy/*`, `docs/CAPROVER.md`.

> Status key: `☐` not started · `▶` in progress · `✅` done · `🔒` externally gated (needs real Docker/CapRover host)

---

## 0. Docs sync ✅ DONE

- [x] `README.md` roadmap table reconciled
- [x] `GAP-ANALYSIS.md` statuses flipped
- [x] `CHANGELOG.md` Unreleased notes bug fix + audit log

---

## 1. Acceptance / verification (externally gated) — PRD §11

- [ ] Verify against a real Docker daemon + CapRover Swarm (staging deploy) 🔒
- [ ] Build & run Docker image on CapRover 🔒
- [ ] One-week soak test with memory/disk measurements 🔒
- [ ] Redeploy latest source to live env (`127.0.0.1:18080` serves an older binary) 🔒

---

## 2. Multi-node / platforms (biggest gap vs Dozzle) — GAP-ANALYSIS §1

- [x] Agent mode (lightweight TLS agent streaming logs/stats) — PRD C10 — already implemented (`capscope agent`, hubagent, mTLS, generate-certs)
- [ ] Swarm cross-node visibility (node grouping by `com.docker.swarm.node.id`)
- [ ] Kubernetes collector path
- [ ] Podman: verify + document, `remoteSocket` fallback in `docker.New`
- [ ] Host-level metrics (host CPU/mem/disk in UI)

---

## 3. Auth & multi-user ✅ MOSTLY DONE

- [x] OIDC / GitHub OAuth — PRD Z5 — already implemented (routes, handlers, middleware, config validation)
- [x] Multi-user + per-user audit trail — PRD Z4 — users/user_apps schema + seeding + API; audit log just wired (GET /api/audit + writes on login/logout/SQL/alerts/deploys/users/incidents)
- [x] Persistent signed session secret — already implemented (persistent session manager next to DB)

---

## 4. Crash / health features

- [x] Snapshot on health-check failure — PRD X8 — collector code done (`snapshotUnhealthy`, `SNAPSHOT_KINDS` includes unhealthy)
- [x] Healthcheck state surfaced in UI — health badges on sidebar + dashboard (committed)
- [x] Verify unhealthy snapshots appear in crashes UI with kind="unhealthy" — /api/snapshots returns all kinds, crashes UI renders all with kind badge

---

## 5. Access logs ✅ DONE

- [x] GeoIP country labels on client IPs — collector + store + country column done
- [x] Per-request app attribution (routing/host map) — AccessRow.app field set from container app; done

---

## 6. Storage (deferred per PRD S6 rationale)

- [ ] Row normalization (S6) — revisit only with real-world evidence

---

## 7. IMPLEMENTATION_PLAN.md — Phases 0 → 7 (v0.15 → v1.0)

- [x] Phase 0 — Foundation config additions (`OTEL_LISTEN`, `PIPELINES_FILE`, `ARCHIVE_BUCKET`, `RETENTION_HOT_DAYS`) — already in config
- [x] Phase 1 — OTLP ingestion — already implemented (receiver at /v1/logs,/v1/metrics,/v1/traces)
- [x] Phase 2 — Logs → Metrics pipelines — already implemented (pipeline.Engine, hot-reload)
- [x] Phase 3a — Trace storage + API (`/api/traces`) — spans table, InsertSpan, QuerySpans, OTLP traces endpoint done
- [x] Phase 3b — Trace UI: trace list, waterfall/flame graph, click log → trace, service graph — DONE (Traces tab, waterfall SVG, span table, service filter)
- [x] Phase 4 — Storage tiering — ArchiveManager + Rollup done
- [x] Phase 5 — Incident management: incident UI (model + API done; Incidents tab with list/detail/state transitions just added)
- [x] Phase 6 — PromQL — `/api/metrics/promql` done
- [ ] Phase 6 — Dashboard builder (template vars, panel types, drag-drop)
- [x] Phase 7 — Audit log — just wired
- [ ] Phase 7 — Fine-grained RBAC, SAML/LDAP, query QoS

---

## 8. UI/UX design proposals (UI-UX-DESIGN.md — v0.3/v1.0)

- [ ] Login screen + session token (P0)
- [ ] Design tokens + brand colors (Docker orange) (P0)
- [ ] Global dashboard with stat row + container grid (P1)
- [ ] Live streaming metrics charts with SSE + tooltips + PNG export (P1–P2)
- [ ] Sidebar search + filters + pin/star (P2)
- [x] Settings UI page (P3) - GET /api/settings endpoint + Settings tab with Server/Auth/Storage/Collection/Alerts/Agent sections
- [ ] Alert rule builder UI + channel tester (P3)
- [x] Log bookmarks (P4) - bookmark toggle in line hover actions, .bk highlight, localStorage persistence
- [ ] Split-pane diff (P4) - compare lines across panes, highlight differences
- [ ] Crash timeline + one-click restart (P4) — conflicts with read-only stance; needs decision

---

## How to consume

Work top-to-bottom. Each item: implement → test (`go vet ./...`, `go test ./...`) → commit → push → move to next.
