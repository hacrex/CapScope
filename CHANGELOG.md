# CapScope Changelog

## Unreleased

### Per-app RBAC, automated webhooks, and SSE capacity testing
- **Per-app RBAC**: viewer users require explicit assignments in the `user_apps`
  SQLite table. Admins retain access to every app. The Admin panel can edit
  assignments, and logs, exports, SSE, dashboards, metrics, access analytics,
  snapshots, alerts, and deploy markers enforce the scope.
- **Automated webhook alerts**: `ALERT_WEBHOOK_URL` delivers critical
  log-pattern and error-spike rules automatically. The Admin-only alert rule
  editor and test sender protect notification configuration.
- **SSE load testing**: `go run ./cmd/sse-bench -url ... -connections 500`
  opens concurrent live-tail clients in batches and reports failures, events,
  read errors, connection time, and optional target RSS via `-pid`.

### v0.15 — MCP server over SQLite (Phase 6.4)
- **MCP server (`cmd/capscope mcp`)** (Phase 6.4): new `mcp` subcommand runs a
  Model Context Protocol server over stdio using JSON-RPC 2.0. Exposes read-only
  tools over the SQLite store:
  - `query_logs` — query container logs with filters (app, container, host, q,
    level, stream, since, until, limit)
  - `list_snapshots` — list crash snapshots filtered by app/host
  - `get_snapshot` — get a specific crash snapshot with full log lines and
    metrics
  - `run_sql` — run read-only SQL queries (SELECT/WITH only, validated)
  - **Session-based auth**: accepts a session token via the `initialize` request
    `meta.token`; requires `AUTH_PROVIDER=local` with sessions enabled

### v0.14 — Multi-user auth: OIDC + persistent users
- **Multi-user login (Phase 3.3)**: sessions now carry user ID, name and role
  (`admin` or `viewer`). Users live in the SQLite store (`users` table); the
  first user is seeded from `AUTH_USER`/`AUTH_PASS` on first start. Admin users
  can manage additional users via `POST /api/users`, `POST /api/users/{id}/role`
  and `DELETE /api/users/{id}`.
- **OIDC single sign-on (Phase 3.2)**: `AUTH_PROVIDER=oidc` with
  `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`. Standard OIDC Discovery
  + PKCE authorization code flow via `/api/oidc/login` and
  `/api/oidc/callback`. The OIDC `sub` claim is matched against `AUTH_USER` as
  an allowlist.
- **Role-based access control**: `viewer` role is read-only; `admin` can manage
  users and write alert rules/deploys. Role is returned by `GET /api/session`
  and shown in the UI.
- **Audit trail**: `access` rows now record the authenticated `user` for
  download/export accountability.

### v0.13 — Multi-host visibility: host selector, per-host views, ?host= permalinks
- **Host selector (Phase 4.5)**: new `host` chip in the header cycles through
  discovered agent hosts (from `GET /api/hosts`) plus "All hosts". Selecting a
  host filters containers, apps, crash snapshots, metrics and the dashboard to
  that host, and persists as `?host=…` in the URL for permalinks.
- **Host-aware API**: `GET /api/containers?host=`, `GET /api/apps?host=`,
  `GET /api/snapshots?host=`, `GET /api/metrics?host=` and `GET /api/dashboard?host=`
  all filter by `host_id`; new `GET /api/hosts` lists registered agent hosts
  with last-seen timestamps.
- **Host-tagged collection**: the collector tags every tracked container with
  `host_id` (from `AGENT_HOST_ID` or hostname), so agent-ingested rows are
  stored with their origin hub and can be filtered independently.
- UI: host selector chip in the header (click to cycle hosts), crash shelf
  respects the host filter, sync URL/share link includes `host`.

### v0.12 — Persistent sessions, expiring permalinks, JSON export, HTML incidents
- **Persistent login sessions (Phase 3.1)**: sessions are HMAC-signed and
  persisted in a sibling file next to SQLite (`DB_PATH.sessions`, mode 0600).
  The backing file never holds raw bearer tokens — only keyed digests — so a
  restart does not log the browser out, and a stolen session file alone cannot
  mint a login (delete it and everyone logs in again). New deployments use it
  by default; `auth.NewSessionManager` (memory-only) stays available for tests.
- **Expiring share links / permalinks (V10, Phase 6.1)**: `POST /api/share`
  mints a capability token (`/s/<token>`) for the current view (tab/app/range,
  optional `#crash=` hash, TTL default 7 days, min 1 min, max 90 days). The
  link redirects to the saved view until it expires server-side — unknown and
  expired tokens both answer 410 with no oracle between them. The SPA gains a
  Share button (Ctrl+K palette too) and incident cards get a Share 7d button.
- **Any-range JSON export (Phase 6.2)**: `GET /api/logs/export?fmt=json`
  streams matching rows as one JSON document with the same filters, paging and
  200k ceiling as CSV/TXT — ready for jq or BI tools.
- **Self-contained HTML incident export (Phase 6.3)**:
  `GET /api/snapshots/{id}/incident.html` produces a single file with inline
  CSS + inline SVG sparklines (cpu %, memory %), no network fetches, no
  external assets — for dropping into a ticket.

### v0.11 — Alerting to Dozzle parity: event rules, smoothing, ntfy, update check
- **Container-event alert rules (`kind: "event"`)**: rules now fire on live
  Docker lifecycle events fanned from the collector into the alert engine —
  `die` (with `exclude` exit codes, default `[0,130,143,137]` for graceful
  stops and OOM kills; `"exclude": []` disables the filter), `oom`, and
  `health_status` (fires when the event's status equals `status`, default
  `unhealthy`). Event rules evaluate the moment the event arrives, not on the
  ticker, and reuse the same repeat/silence window, history recording and
  delivery-status tracking as the periodic rules.
- **Metric smoothing**: adding `window` to a `metric` rule switches from
  "every sample must hold for `for`" to mean-of-window: the average of all
  samples inside the window is compared, so a single spike or dropout cannot
  flip the alert. Freshness is judged against the same window; the previous
  semantics stay the default for rules without `window`.
- **`cooldown` field**: Dozzle-compatible alias of `repeat` (wins when both
  are set). Nothing else changes about the silence mechanism.
- **ntfy channel**: `ALERT_NTFY_URL` (topic URL) + optional
  `ALERT_NTFY_TOKEN` (sent as Bearer). Title and priority ride in headers,
  message is the raw body — the format ntfy expects.
- **`${ENV}` placeholders** in every channel endpoint and token (webhook,
  Slack, Discord, ntfy, Telegram), expanded at send time so secrets can
  rotate without a restart and committed rules files stay secret-free.
  Variables with `AWS_`/`DOCKER_`/`KUBERNETES_`/`DOZZLE_` prefixes are
  refused, matching Dozzle's guard.
- **Update-available checks (A7)**: on a daily jittered timer CapScope pulls
  each tracked container image's manifest digest from its registry (with the
  standard anonymous token dance for Docker Hub) and compares it with the
  locally stored digest. A difference becomes an `update_available`
  pseudo-event any `event: update_available` rule can alert on. Read-only
  toward Docker (`/images/{id}/json` only); digest-pinned images are skipped;
  `UPDATE_CHECK=false` disables it (default on, cadence
  `UPDATE_CHECK_INTERVAL`, min 1h).
- **Rule-builder UI**: `event` is a new builder kind with event/status/
  ignore-list fields; the API round-trips the new fields
  (`/api/alerts/rules`, `/validate`).
- Tests: event-rule validation/firing/silence, smoothed metrics, ntfy payload,
  `${ENV}` expansion + refusal, rule-builder round-trip, update checker
  against a registry stub (match/miss/challenge flow/dedup/unreachable),
  new config knobs.

### v0.10 — Log parsing breadth, raw export, time display, CI
- **Level parsing parity with Dozzle v11**: OpenTelemetry `severityText` and
  `severityNumber` (1–24 band scale) and Pino numeric levels (`30`/`40`/`50`)
  are now recognized in JSON lines, before the text-word heuristic. A line
  like `{"level":50,"msg":"..."}` classifies as error; `{"severityNumber":13}`
  as warn; non-level numbers (`{"HTTP status":404}`-style payloads) do not
  misdetect. `internal/logproc` gained the two numeric mappers plus tests.
- **Raw log export**: `GET /api/logs/raw` streams the original lines as a plain
  text file (no CSV quoting), same filters, same 200k paging ceiling and FTS
  overlap guard as the CSV export (now shared via `eachLogRow`). UI button
  next to *Download CSV*; identical filter-sync behavior.
- **Time display toggle**: header button (and Ctrl+K palette action) switching
  log/chart timestamps between browser-local and UTC; persisted, repaints the
  visible window only.
- **Phase-0 hygiene (Dozzle-parity plan)**: GitHub Actions workflow
  (`.github/workflows/test.yml`: `go vet`, `-race` test with the SQLite tags,
  Docker build check), `scripts/check-ui.js` inline-script syntax gate wired
  into `make test`, and the stale `CapScope-capscope.tar.gz` from the repo
  root removed (deploy-time generated).

### v0.9 — Forward-proxy authentication (AUTH_PROVIDER=proxy)
- **`AUTH_PROVIDER=proxy`**: CapScope can delegate authentication to the
  reverse proxy in front of it (Authelia, Authentik, Cloudflare Access, ...).
  It trusts `PROXY_USER_HEADER` (`Remote-User` by default) as the caller
  identity: no password, no CapScope-issued sessions, and the UI boots without
  the login overlay. Custom header names (`X-WebAuth-User`) are honored.
- `/api/login` refuses with a hint in this mode; `POST /api/logout` is a
  no-op (the proxy owns logout); `/api/session` reports the header identity.
- **Forged-header surface documented**: the header is trusted from anyone who
  can reach the port directly — publish only the proxy and keep CapScope on an
  internal network in this mode. A startup warning states this explicitly, and
  a colored `AUTH_PASS ignored` warning fires if both are set.
- No password means no `AUTH_PASS` requirement, so deployments that would
  otherwise need `ALLOW_NO_AUTH` can use a real auth story instead.
- Tests: header missing → 401 (Bearer tokens explicitly rejected), identity
  header → authenticated `/api/session` and usable SSE, login refused with
  400, custom header honored; config validation of `PROXY_USER_HEADER` and the
  unknown-provider / password-required regressions.

### v0.8 — Live tail: gapless resume, stack-trace grouping, sidebar filter
- **Gapless live tail over reconnects**: every SSE row now ships an `id:`; on
  any reconnect the browser echoes it as `Last-Event-ID` and the hub only
  forwards rows newer than that cursor — no duplicates and no gap while the
  TCP connection was down. The UI keeps a delivery cursor (`since_id`) and a
  backoff reconnect loop (1s → 15s with jitter) that takes over when
  EventSource gives up, plus a SQLite catch-up replay (`/api/logs?since_id=…&asc=1`)
  for rows published while no subscriber was connected (the hub has no replay
  buffer). Connection chip shows `reconnecting (attempt n)` honestly.
- **Fixed: live logs 401'd with auth on.** `EventSource` cannot send an
  Authorization header, so `/api/logs/stream` was unreachable whenever
  `AUTH_PASS` was set. Login now mirrors the bearer token into an HttpOnly
  `capscope_token` cookie (SameSite=Strict, Secure on TLS), which the auth
  middleware accepts as a fallback; logout clears it.
- **Fixed: `since_id` was dead code.** The UI asked `/api/logs?since_id=…`
  (context modal, crash replay) but `parseLogQuery` only read `after_id`; both
  names now work, and `store.LogQuery.Match` enforces the cursor so hub
  delivery deduplicates on resume.
- **Stack-trace grouping ("Group" toggle)**: display-level multi-line folding —
  indented lines, `at …` frames, `Caused by:`, Python `Traceback`, Go
  `goroutine ` and `...` lines attach to the entry above them with a clickable
  ▸ badge; persisted rows are unchanged. On by default; command palette and
  localStorage-backed toggle.
- **Sidebar app filter**: search box at the top of the Apps list, survives the
  10s rebuild, Esc clears.
- **Virtual-scroll trim fix**: height deltas are anchored by measured pixels
  instead of assuming 1 removed row == 1 removed line (grouping changes the
  ratio); `Load older` scroll anchoring unchanged.
- Tests: SSE `Last-Event-ID`/`since_id` resume dedup + delivery, cookie-session
  EventSource reachability.

### v0.7 — Dashboard, alert rule builder & operations
- **Global Dashboard tab** (default landing view): stat row (healthy containers, restarts/24h, crash loops, DB size), per-app health cards with CPU/mem sparklines, recent-crash strip; cached 15 s server-side and self-healing on collector restarts
- **Alert rule builder UI**: modal form on the Alerts tab with live `alerts.json` preview, server-side validation (`POST /api/alerts/rules/validate`), save-and-reload (`POST /api/alerts/rules`, atomic write) and test-fire
- **Alert history delivery status**: engine records `sent` / `failed` / unset per fired alert after notifying channels; shown as a colored Delivery column in the history table with hover explanation
- **Crash-loop endpoint**: `GET /api/crashloops` groups repeated exits for dashboard badges and the Crashes timeline
- **Prometheus export**: `GET /metrics` exposes container health, restart counts, crash loops, DB size and HTTP metrics for external scraping
- **Shareable incident permalinks**: tab, app, time range, query and crash-snapshot id are synced to the URL; crash detail rows get a copy-link button that deep-links straight into an incident
- **Session fix**: `/api/session` returns 200 with `auth_enabled:false` when running with `ALLOW_NO_AUTH=true`; login overlay starts hidden so it can never block interactions in no-auth mode
- Playwright smoke coverage: rule-builder modal (open → edit → validate → block-invalid-save → close) and dashboard render checks all green

## v0.6

### UI/UX
- Single `All apps` row in sidebar (was duplicated)
- Header container stat: CPU% in green, memory% in blue
- Product footer with name and tagline
- Global `/` keyboard shortcut focuses search
- Replica button shows total container count (`All replicas (n)`)
- Connection chip pulses when live
- Natural replica sorting (`web.2` before `web.10`)
- Follow button more prominent (filled accent state)
- Empty-state hint when no containers found
- Password reveal toggle (eye/eye-off) on login
- Line wrapping toggle (`Wrap` button)
- "Jump to live" floating button (`↓ New logs`)
- Copy line on hover (`⧉` → `✓` confirmation)
- Replica icon chips for apps and panes (nginx🟢 postgres🟣 redis🔴 mysql🔵 etc.)
- Global pretty-print JSON toggle
- Time range chips (5m / 15m / 1h / 24h)
- Access log status filter chips (all / 2xx / 4xx / 5xx)
- Alert rule status dots (firing / resolved) + mute (1h / 24h)
- Log line highlight/bookmark (double-click + ⚑ flag)
- Per-pane follow/pause for split log panes
- Crash timeline markers on metrics chart (red ✕ flag)

### Auth
- Session expiry warning banner (< 60 min remaining)
- `/api/session` returns `expires_at` timestamp

### Deploy / Build
- Dockerfile: `golang:1.26-alpine` to match `go.mod` (`go 1.26.0`)
- CapRover deploy tarball excludes test files, demo assets, and build output
- Root-level `docker-compose.yml` for instant `docker compose up`
- GitHub issue templates, SECURITY.md, CONTRIBUTING.md, CODEOWNERS added

### Core Features
- Crash snapshots with OOM detection, exit codes, restarts, uptime, plain-language summary
- Crash snapshots survive redeploys (persisted in SQLite)
- Per-container CPU/memory metrics charts (15m to 7 days)
- Deploy markers on charts and log timeline
- Restart-loop detection with `↻` badge
- Access-log dashboard (GoAccess-style: status codes, top paths, IPs, referrers)
- Alerts: dead-man, restart loop, metric threshold, log pattern
- Alert channels: webhook, Telegram, Slack, Discord
- Secret redaction before storage (JWTs, bearer tokens, passwords, AWS keys, PEM)
- Basic auth required by default; refuses to start without a password
- FTS5 full-text search, regex mode, raw SQL queries
- CSV export up to 200,000 lines
- Split-view up to 4 replicas side by side
- Multi-architecture Docker build (`scratch` image, no shell)

## v0.1 — MVP

- Collection, SQLite history, live view, search, metrics, crash snapshots, auth, redaction
- Docker and CapRover packaging (`captain-definition`)
