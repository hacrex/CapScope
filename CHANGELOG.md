# CapScope Changelog

## Unreleased

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
