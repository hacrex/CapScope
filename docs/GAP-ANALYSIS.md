# CapScope gap analysis vs. Dozzle (amir20/dozzle v11.4)

> Comparison of CapScope against Dozzle as of 2026-11-03.
> Execution plan: `docs/ROADMAP-DOZZLE-PARITY.md`. Status as of that plan:
> Phases 0, 1, 2, 3.1, 3.2–3.3, 4.1–4.5, 6.4 (MCP server) **complete**;
> v0.8–v0.14 all landed.
> Dozzle is a live-only log viewer (stores nothing). CapScope is storage-first:
> SQLite history, crash forensics, retention, access logs. Several Dozzle
> features are deliberate non-goals for us; several CapScope features are things
> Dozzle does not have. This document records both sides, and what remains.

## Where CapScope is already ahead

| CapScope has (Dozzle doesn't) | Notes |
|---|---|
| Persisted log history searchable after container death | FTS5 + raw SQL | 
| Crash snapshots | exit code, OOM detection, uptime, rule-based summary, Markdown export |
| Access-log dashboard | reverse-proxy logs, GoAccess-style panels |
| Deploy markers | on charts + inline in the log timeline |
| Restart-loop detection | sliding-window death counting |
| Secret redaction | before storage |
| Retention budgets | global, per-app, hard DB cap |

Feature areas where Dozzle v11 catches up partially: alerts (v11 added rule
editing, delivery status, persistent history), level parsing (OTel
`severityText`/`severityNumber`, Pino numeric levels — verify `internal/logproc`
covers these).

---

## 1. Multi-node / platforms — biggest remaining gap

Dozzle: agent mode (TLS + protobuf, healthchecks, host groups with merge-all
views), remote hosts (`tcp://` + Docker TLS), native Swarm global mode,
Kubernetes, Podman docs. This is the core of its scale story.

CapScope (`internal/docker/client.go`, `internal/collector/manager.go`): one
host from `DOCKER_HOST`, hand-rolled GET-only client, no SDK.

| Work item | Ref | Status |
|---|---|---|
| Agent mode (lightweight TLS agent streaming logs/stats) | PRD C10, v1.0 | ❌ |
| Swarm: cross-node visibility (parse labels today only) | | ❌ |
| Kubernetes collector path | | ❌ |
| Podman: verify + document, `remoteSocket` fallback | | ❌ untested/undocumented |
| Host-level metrics (host CPU/mem/disk in UI) | | ❌ |

## 2. Auth & multi-user

Dozzle: `users.yml` multi-user with `generate` command, per-user roles +
filters, GitHub sign-in, OIDC, forward-proxy header trust (`Remote-User`),
signed persistent session secrets, server-persisted user settings.

CapScope (`internal/auth/session.go`): one shared credential, in-memory
sessions, no proxy-header trust, no OIDC, no identity.

| Work item | Ref | Status |
|---|---|---|
| ✅ Forward-proxy header auth (`Remote-User` etc., `AUTH_PROVIDER=proxy`) | landed v0.9 | done |
| OIDC / GitHub OAuth | PRD Z5 | ❌ |
| Multi-user + per-user audit | PRD Z4 | ❌ |
| Persistent signed session secret (survive restarts) | | ❌ (in-memory today) |
| ✅ Cookie session fallback for streaming clients (EventSource) | landed v0.8 | done |

## 3. Alerts — parity reached (v0.11), one extension open

CapScope engine (deadman, restart_loop, metric+duration/smoothing,
log_pattern, **event**; webhook/Telegram/Slack/Discord/**ntfy**; history with
delivery status; UI rule builder incl. event rules) matches Dozzle's alerting.
Exception list only:

| Work item | Status |
|---|---|
| ✅ Event rules on Docker events: `die` (+ `exclude` exit codes, default [0,130,143,137]), `oom`, `health_status` (`status` value filter) | landed v0.11 |
| ✅ Update-available alerts: daily jittered registry manifest-digest check per tracked image → `update_available` pseudo-event (`UPDATE_CHECK`, `UPDATE_CHECK_INTERVAL`) | landed v0.11 |
| ✅ Metric smoothing (`window:` mean-of-window mode) + `cooldown` (Dozzle-compatible alias of `repeat`) | landed v0.11 |
| ✅ ntfy channel (`ALERT_NTFY_URL`, optional `ALERT_NTFY_TOKEN`) | landed v0.11 |
| ✅ `${ENV}` placeholders in every channel endpoint/token, expanded at send time; `AWS_`/`DOCKER_`/`KUBERNETES_`/`DOZZLE_` prefixes refused | landed v0.11 |
| Templated webhook payload bodies (Go `text/template`) | ❌ deliberately skipped — one fixed JSON shape is enough for the webhook targets we serve |

## 4. Live-log UX — polish tier

Verified against `internal/web/static/index.html`:

| Work item | Status |
|---|---|
| ✅ Multi-line / stack-trace merging (v0.8 `Group` toggle, display-level) | landed |
| ✅ Sidebar inline filter box (v0.8 `#appFilter`) | landed |
| ✅ SSE gapless resume: `id:` per row + `Last-Event-ID`, `since_id` alias, cookie auth for EventSource (`capscope_token`), manual backoff reconnect + SQLite catch-up | landed v0.8 |
| ✅ OTel/Pino numeric level parsing (`internal/logproc`) | landed v0.10 |
| ✅ Raw text/log-file download (`/api/logs/raw`) | landed v0.10 |
| ✅ Timezone display override (Local/UTC toggle) | landed v0.10 (UTC only; IANA zones on request) |
| Healthcheck state surfaced in UI | ❌ (PRD X8) |
| Named color schemes | ❌ deliberately skipped |

Notes:
- Auth + SSE were mutually exclusive before v0.8 — `EventSource` cannot set an
  Authorization header, so the live tail 401'd whenever `AUTH_PASS` was set.
  Login now mirrors the bearer token into an HttpOnly `capscope_token` cookie
  (SameSite=Strict) which the middleware accepts as a fallback.
- The UI's `since_id` catch-up parameter was silently ignored before v0.8
  (`parseLogQuery` only read `after_id`); both names work now, and store
  `LogQuery.Match` enforces the cursor, so hub delivery deduplicates on resume.

## 5. Deliberate gaps — decide, don't copy

- **Container actions / shell access.** Dozzle can start/stop/recreate and exec.
  CapScope is read-only end to end (`internal/docker/client.go` is GET-only).
  Keep read-only as a differentiator ("evidence-grade, no write path"); at most
  an opt-in `ACTIONS=true` flag later.
- **Dozzle Cloud, dtop, anonymous analytics, 4-language i18n.** Skip.
- **MCP server over CapScope's SQLite.** Unique opportunity: AI incident
  analysis over *persisted* logs + crash snapshots (Dozzle's cloud rail only has
  live context).

## 6. Hygiene — fix from our own roadmap

- ☑ `GET /metrics` — implemented (`internal/api/metrics.go`); PRD still says open — update PRD
- ✅ Self-contained HTML incident export (`/api/snapshots/{id}/incident.html`, v0.12)
- ✅ Any-range JSON export (`/api/logs/export?fmt=json`, v0.12)
- ✅ Expiring permalinks (PRD V10: `POST /api/share`, `/s/<token>`, v0.12)
- ❌ Health-check-failure snapshots (PRD X8)
- ❌ Access-log app attribution + GeoIP (PRD C9.7)
- ❌ Acceptance items: real-CapRover verification, image deployed on CapRover, soak test
- ⚠️ README roadmap table stale (says v0.4 = Prometheus + agent mode)
- ⚠️ UI is a single ~1,700-line embedded HTML file; adequate now but a component
  split is worth revisiting if v1.0 lands

---

## Plan

| Phase | Items |
|---|---|
| 1. Live logs | ✅ multi-line merging, SSE reconnect backoff + gapless resume, sidebar filter (v0.8) |
| 2. Hygiene | docs sync, acceptance items, soak test |
| 3. Auth | ✅ forward-proxy header auth (`AUTH_PROVIDER=proxy`, v0.9) + persistent sessions across restarts (v0.12) + OIDC SSO + multi-user with roles (v0.14) |
| 4. Alerts | ✅ event rules, ntfy, update check, smoothing/cooldown, `${ENV}` (v0.11) |
| 5. Export & share | ✅ raw TXT (v0.10), JSON any-range export (v0.12), self-contained HTML incident export (v0.12), expiring permalinks (v0.12) |
| 6. Scale | ✅ agent mode + hub ingestion (v0.13), host selector chip + ?host= permalinks (v0.13), host metrics |
| 6. Differentiators | ✅ MCP server over SQLite (v0.15), HTML incident export |
