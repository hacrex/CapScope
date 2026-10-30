# CapScope

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small, self-hosted container log and stats viewer for CapRover (and any Docker host), in the spirit of Dozzle, with two things Dozzle does not do by design:

1. **Logs and metrics are stored in SQLite**, so you can search history after a redeploy removed the container.
2. **Crash snapshots**: when a container dies with a non-zero exit code or is OOM killed, CapScope saves the last log lines, the memory and CPU trend leading up to it, the exit code, and a plain-language summary.

One Go binary (cgo, SQLite), no external database, embedded web UI.

## CapScope vs Dozzle side by side

Dozzle ([dozzle.dev](https://dozzle.dev), `amir20/dozzle`) is the reference for this project — it is the best-in-class *live* container log viewer, and it does that job very well. The honest framing: Dozzle deliberately stores nothing; CapScope deliberately stores logs, metrics and crash forensics in SQLite.

| Capability | CapScope | Dozzle |
|---|---|---|
| Live log tail (SSE) | Yes — virtual-scrolled, 100k lines in memory | Yes |
| Log storage | SQLite, retention + size cap (`7d` / `1 GB` default) | **None by design** — live view only, no offline search |
| Search history after redeploy/restart | **Yes** — FTS5 token/prefix index with `LIKE` fallback + regex | No (by design; regex over the live buffer only) |
| SQL queries over logs | Yes — single validated `SELECT` (or `WITH … SELECT`) over `logs`, `metrics`, `containers`, `snapshots`, `access` | Yes — embedded SQL engine |
| Split screen | Yes — up to 4 replicas side by side | Yes |
| CPU / memory charts | Yes — plus persisted history in SQLite | Yes — live stats with historical visualizations |
| Crash / OOM snapshots | **Yes** — exit code, OOM flag, uptime, restarts, memory trend, last N log lines, plain-language summary, Markdown export; restart-loop detection, and deaths missed while CapScope was down are rebuilt on startup | No |
| Secret redaction before storage | Yes (JWT, bearer, AWS keys, PEM, URL creds, `password=`/`token=`) | No (nothing is stored) |
| Authentication | Basic auth, refuses to start without a password | Multi-user file auth + forward-proxy auth (Authelia etc.) |
| Multi-host | Single node (per-node agent on roadmap) | **Yes** — agent mode |
| Orchestration | Single Docker Engine (Docker API v1.24+; also reachable over TCP via a socket proxy) | **Yes** — Docker Swarm, Kubernetes, Podman, Colima |
| Container list | Per-app sidebar, CapRover `srv-captain--` names unwrapped | Fuzzy container search, multi-host tree, app icons |
| Log download | Crash snapshot Markdown export | Yes — download logs |
| Themes | Auto / dark / light | Dark mode |
| Frontend stack | Single embedded HTML file, no build step, no JS deps | Vue SPA (Bun + Vite) |
| Image size | Single static musl binary on `FROM scratch` — no OS layer, no shell (a few MB compressed) | ~7 MB compressed, built `FROM scratch` |
| Analytics | None | Google Analytics (opt-out via `--no-analytics`) |
| Maturity | Early — single maintainer, not yet run on a real CapRover swarm | 8+ years, hundreds of containers tested, Docker-sponsored |

**Pick Dozzle** if you want a polished multi-host live viewer with auth options and you do not need history — it will feel more finished.

**Pick CapScope** if you want to answer "what did the container print before it died, and was it OOM?" after the fact: persistence, FTS search over old logs, and crash snapshots are the entire point of this project.

## What you get

| Area | Features |
|---|---|
| Live view | Per-app sidebar (CapRover `srv-captain--` names are unwrapped, plus an **All apps** entry merging every timeline), virtual-scrolled live log tail over SSE (100k lines in memory), pause, follow, load older, split view for comparing up to 4 replicas side by side, click a JSON log line to pretty-print it |
| Search | FTS5 token/prefix search over persisted logs with substring fallback, regex, or **SQL** (`SELECT` over `logs`, `metrics`, `containers`, `snapshots`, `access` via `/api/sql`); filter by level, stream (stdout/stderr) and replica |
| History | Logs and CPU/memory samples persisted in SQLite with age and size retention |
| Access dashboard | GoAccess-style panels built on the reverse proxy's access logs: status codes, top paths, client IPs (anonymized by default), referrers, requests/errors timeline, CSV export |
| Crash snapshots | Exit code, OOM flag, restart count, uptime, last N log lines, last N metric samples, rule-based summary, Markdown export |
| Safety | Basic auth required by default, secret redaction before storage, read-only Docker access possible via socket proxy |

## Quick start

```bash
# local, against your Docker daemon, no auth (local testing only)
make run                 # http://127.0.0.1:8080

# try the UI with fake data first
make demo

# tests
make test
```

Docker:

```bash
docker build -t capscope .
docker run -d --name capscope -p 8080:8080 \
  -e AUTH_PASS=change-me \
  -v capscope-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  capscope
```

CapRover: see [docs/CAPROVER.md](docs/CAPROVER.md).

Other platforms: see [deploy/README.md](deploy/README.md) for Coolify, Dokploy, Dokku, Portainer, Podman and any generic Docker host.

## Configuration

All settings are environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN` | `:8080` | HTTP listen address |
| `DB_PATH` | `/data/capscope.db` | SQLite file. Must be on a persistent volume |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | `unix://` socket or `tcp://host:port` (for a socket proxy) |
| `AUTH_USER` / `AUTH_PASS` | `admin` / none | Basic auth. The server refuses to start without `AUTH_PASS` |
| `ALLOW_NO_AUTH` | `false` | Set `true` to run without auth (local testing only) |
| `LOG_RETENTION` | `7d` | Delete logs older than this. Accepts `d`, `h`, `m` |
| `METRICS_RETENTION` | `7d` | Same for CPU/memory samples |
| `SNAPSHOT_RETENTION` | `30d` | Same for crash snapshots |
| `MAX_DB_MB` | `1024` | Cap on live data. Oldest logs, then metrics, access, snapshots, alerts and deploys are dropped when exceeded |
| `STATS_INTERVAL` | `10s` | How often a metric sample is stored per container (minimum 1s) |
| `INITIAL_TAIL` | `200` | Lines to backfill the first time a container is seen |
| `MAX_LINE_BYTES` | `16384` | Longer lines are truncated |
| `REDACT` | `true` | Mask JWTs, bearer tokens, `password=`/`token=` style values, URL credentials, AWS keys, PEM keys |
| `ACCESS_LOG` | `auto` | Web-server access-log parsing: `auto` (proxy-named apps always on, others probed), `all`, `off`. Parses nginx combined/common (CapRover's `captain-nginx`) |
| `ANON_IPS` | `true` | Zero the last octet of stored access-log client IPs (IPv6 cut to /48). `false` keeps the original IP |
| `SNAPSHOT_KINDS` | `crash,oom` | Which stops create a snapshot: `crash`, `oom`, `killed`, `clean_exit` |
| `SNAPSHOT_LOG_LINES` | `200` | Log lines kept in each snapshot |
| `SNAPSHOT_METRIC_SAMPLES` | `60` | Metric samples kept in each snapshot (10 minutes at the default interval) |
| `RESTART_LOOP_COUNT` | `3` | Deaths of one container inside the window that flag a restart loop (minimum 2; the summary says so and the UI shows a `↻` badge) |
| `RESTART_LOOP_WINDOW` | `5m` | The window counted by `RESTART_LOOP_COUNT` (minimum `1m`) |
| `INCLUDE` / `EXCLUDE` | none | Regexes matched against container name and app name |
| `APP_RETENTION` | none | Per-app log retention, tighter than `LOG_RETENTION`: comma-separated `app=duration`, e.g. `web=3d,api=1d`. The global retention is still the upper bound |
| `APP_MAX_MB` | none | Per-app log-size budget: comma-separated `app=megabytes`, e.g. `web=500,api=200`. The oldest logs of an app over budget are dropped first, before the global `MAX_DB_MB` cap |
| `ALERTS_FILE` | `/data/alerts.json` | JSON alert-rules file, hot-reloaded (no restart needed). Missing file disables alerting |
| `ALERTS_INTERVAL` | `1m` | How often alert rules are evaluated (minimum `10s`) |
| `ALERT_WEBHOOK_URL` | none | Generic JSON webhook: receives `POST {"text": "..."}` |
| `ALERT_TELEGRAM_TOKEN` / `ALERT_TELEGRAM_CHAT_ID` | none | Telegram Bot API (must be set together) |
| `ALERT_SLACK_URL` | none | Slack incoming-webhook URL |
| `ALERT_DISCORD_URL` | none | Discord webhook URL |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Alert rules

Alerts are driven by a JSON file (`ALERTS_FILE`), edited in place and picked up on the next evaluation tick. Each rule has a `name`, a `kind`, and an optional `apps` list (empty means every app). `repeat` (default `30m`) is the silence between two notifications of the same rule and app.

```json
{
  "rules": [
    { "name": "api-silent", "kind": "deadman", "apps": ["api"], "for": "5m" },
    { "name": "api-looping", "kind": "restart_loop", "apps": ["api"], "count": 3, "window": "10m" },
    { "name": "api-mem", "kind": "metric", "apps": ["api"], "metric": "mem_pct", "op": "gt", "value": 85, "for": "5m" },
    { "name": "api-5xx", "kind": "log_pattern", "apps": ["api"], "pattern": "status=5\\d\\d", "hits": 5, "window": "5m" }
  ]
}
```

| Kind | Meaning | Key fields |
|---|---|---|
| `deadman` | No logs from the app for `for` (A1) | `for` |
| `restart_loop` | More than `count` deaths in `window` (A2) | `count`, `window` |
| `metric` | `metric` past `value` for the whole `for` window (A3). `metric` is `cpu` (%), `mem` (bytes) or `mem_pct` (% of limit); `op` is `gt` or `lt` | `metric`, `op`, `value`, `for` |
| `log_pattern` | Regex `pattern` matched `hits` or more lines in `window` (A4) | `pattern`, `hits`, `window` |

Channels (A5) are configured with the `ALERT_*` env vars above and receive a one-line message. The **Alerts** tab lists loaded rules and recent firings, and its **Send test alert** button pushes a test message through every configured channel so you can verify a webhook before an incident.

## Snapshot kinds

| Kind | Meaning | Stored by default |
|---|---|---|
| `oom` | Kernel OOM killer fired (Docker `oom` event or `OOMKilled` flag) | yes |
| `crash` | Non-zero exit with no stop signal sent first | yes |
| `killed` | A kill or stop signal was sent first (deploy, restart, `docker stop`) | no, it is noise during rollouts |
| `clean_exit` | Exit code 0 | no |

## Project layout

```
cmd/capscope        main: wiring, graceful shutdown
cmd/seed-demo       fake data generator for `make demo`
internal/access     web-server access-log parser, auto-detection, IP anonymization
internal/config     env parsing and validation
internal/docker     minimal Docker Engine API client (unix and tcp), stream decoding
internal/collector  container discovery, log and stats followers, events, snapshots, retention loop
internal/store      SQLite schema, queries, pruning
internal/hub        live fan-out to SSE clients
internal/logproc    secret redaction and log level detection
internal/alerts     alert rules, evaluation engine, notification channels
internal/api        HTTP API, SSE, auth, Markdown export
internal/web        embedded single-page UI
deploy/             platform guides: Coolify, Dokploy, Dokku, Portainer, generic docker-compose
docs/               PRD, architecture, CapRover guide, API reference
```

## Limits to know about

- **Single node.** On a multi-node Swarm you only see containers on the node CapScope runs on. A per-node agent is on the roadmap.
- **Crashes while CapScope is down are caught only if the container still exists.** On startup it replays Docker events from the last processed event (up to 24 hours back, as far as the daemon's event buffer goes) and rebuilds the missing snapshots, reading the final log lines straight from the container's log file. A container that was removed while it was down (`docker run --rm`, swarm task cleanup) leaves nothing to rebuild from, and crashes older than the daemon's event buffer are invisible.
- **Log driver.** The Docker logs API only works for the `json-file`, `local` and `journald` drivers. CapRover uses `json-file` by default.
- **Search.** Plain-text search uses an FTS5 index (token and prefix matching) with a `LIKE` fallback for mid-word substrings. Regex search scans up to the newest 200,000 matching candidates and is not indexed. SQL mode accepts one read statement per call (5 s deadline, 5000-row cap) — see docs/API.md.
- **Level detection and redaction are heuristics**, not guarantees. Do not rely on redaction as your only protection for secrets in logs.
- **Docker socket access is root equivalent.** Use auth, HTTPS, and ideally a socket proxy. See docs/CAPROVER.md.

## What was verified

Unit tests cover log stream decoding, stats math, redaction, level detection, SQLite queries (including FTS5 search and its LIKE fallback), read-only SQL validation (writes and stacked statements rejected, table unharmed) and the `/api/sql` endpoint, retention and size cap (including the parsed `access` table and the per-app retention and byte budgets), access-log parsing and container auto-detection, the `/api/access`, `/api/logs/export` and `/api/access/export` endpoints, alert rule parsing and validation, all four alert kinds (deadman, restart loop, metric threshold with duration, log pattern) including their negative cases (fresh logs, below-count, a cool sample breaking the window, stale data, no-limit `mem_pct`), the silence window, rules-file hot reload, all four channel payload shapes against local HTTP servers, the `/api/alerts`, `/api/alerts/rules` and `/api/alerts/test` endpoints, and deploy recording and lookup (`/api/deploys`, the preceding-deploy attach on a snapshot, and its Markdown export line). End-to-end tests run the collector against fake Docker daemons: one emits an OOM and a die event and checks the saved snapshot, another proves that a death replayed after downtime is rebuilt from Docker's log file, deduplicated when the event arrives twice, and counted into the restart-loop window. A two-container fake proves excluded (access-only) containers feed the access table without leaking into the logs table. The whole suite (`go test -tags fts5 ./...`, cgo build) was run and passes; `go vet` is clean. That run caught two real bugs (the `/api/access` aggregates were missing `FROM access`, and the access timeline query bound its bucket arguments in the wrong order), so treat any feature that has never had a failing test as untested. The earlier UI was exercised in headless Chromium against seeded data; the virtual-scroll/split-pane/theme/SQL rewrite and the new Access, Alerts and deploy-marker features have not yet been re-verified in a browser. The alert channels have only been exercised against local test servers, not real Telegram/Slack/Discord endpoints. It has **not** yet been run against a real Docker daemon or a CapRover Swarm, and the static-scratch Docker image has not yet been built, so expect to shake out small issues on first deploy.

## License

[MIT](LICENSE).
