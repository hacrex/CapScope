# CapScope

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00add8.svg)](https://golang.org/)
[![GitHub stars](https://img.shields.io/github/stars/hacrex/CapScope?style=social)](https://github.com/hacrex/CapScope)
[![GitHub last commit](https://img.shields.io/github/last-commit/hacrex/CapScope)](https://github.com/hacrex/CapScope/commits/main)

> **Persistent container logs, crash snapshots, and metrics in one self-hosted binary.**  
> Built for self-hosters, homelab brewers, and DevOps engineers who run Docker on one server and need to know *what happened before it died.*

CapScope watches your Docker daemon, stores logs and CPU/memory samples in SQLite, and keeps a crash snapshot every time a container exits non-zero or is OOM killed. When a container is redeployed and disappears, its logs and memory trend are still searchable — unlike live-only log viewers where the evidence dies with the container.

No external database. No Prometheus stack. One container, one volume, one password.

---

## What CapScope does

| Area | What you get |
|------|--------------|
| **Live logs** | Per-app sidebar, virtual-scrolled live tail over SSE, pause/follow, load older, split view for up to 4 replicas side by side, click any JSON line to pretty-print it |
| **Search history** | FTS5 full-text search over persisted logs, regex mode, or raw SQL (`SELECT` over `logs`, `metrics`, `containers`, `snapshots`, `access` via `/api/sql`); filter by level, stream, and replica |
| **History after redeploy** | Logs and CPU/memory samples persist in SQLite with age and size retention; search an app's logs long after its container is gone |
| **Crash snapshots** | Exit code, OOM flag, restart count, uptime, last N log lines, last N metric samples, rule-based plain-language summary, Markdown export for incident handoff |
| **Restart-loop detection** | Counts deaths per container in a sliding window; flags loops with a `↻` badge in the UI |
| **Access-log dashboard** | GoAccess-style panels from your reverse proxy's access logs: status codes, top paths, client IPs (anonymized by default), referrers, hits/errors timeline, CSV export |
| **Deploy markers** | Record deploys from CI or the UI; markers appear on CPU/memory charts, inline in the log timeline, and on crash details ("Last deploy: v1.4") |
| **Alerts** | Dead-man (silence), restart loop, metric threshold (with duration), log-pattern rules, and live container-event rules (die/OOM/health/update-available); webhook, Telegram, Slack, Discord, and ntfy channels; `${ENV}` placeholders in channel endpoints |
| **Secret redaction** | Masks JWTs, bearer tokens, `password=`/`token=` values, URL credentials, AWS keys, and PEM keys before storage |
| **Auth and RBAC** | Local password auth, OIDC, or reverse-proxy auth; persistent `admin`/`viewer` users; per-app permissions for viewers; session revocation after role/scope changes |

---

## Who it's for

- **Self-hosters** running Docker on a home server or VPS who want observability without operating Grafana + Loki + Prometheus.
- **Homelab brewers** with a dozen apps on one box who need to answer "why did my container restart at 3 AM?"
- **DevOps engineers** managing CapRover, Coolify, Dokploy, Dokku, Portainer, or raw Docker who want crash forensics and log history out of the box.
- **Anyone** who has lost logs after a `docker compose up` or CapRover redeploy replaced a container and took its stdout with it.

---

## Why "CapScope"?

**Cap**tain + **Scope** — a captain who checks his containers.

A ship's captain walks the deck and knows what is aboard: what is running, what is sick, and what went over the side. CapScope does the same job for a Docker host — it follows every container on the node, keeps what they print, and holds on to the evidence after a container is replaced.

That is the whole idea behind the name, and the marketing site lives at **[capscope.live](https://capscope.live)**.

---

## Quick start

### Docker (any host)

```bash
docker run -d --name capscope \
  -p 8080:8080 \
  -e AUTH_USER=admin \
  -e AUTH_PASS=change-me \
  -v capscope-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --restart unless-stopped \
  ghcr.io/hacrex/capscope:latest
```

Open `http://localhost:8080` and log in.

### With Docker Compose

```yaml
services:
  capscope:
    image: ghcr.io/hacrex/capscope:latest
    ports:
      - "8080:8080"
    environment:
      AUTH_USER: admin
      AUTH_PASS: change-me
      MAX_DB_MB: 2048
      EXCLUDE: "^captain-"
    volumes:
      - capscope-data:/data
      - /var/run/docker.sock:/var/run/docker.sock
    restart: unless-stopped

volumes:
  capscope-data:
```

### Build from source

```bash
git clone https://github.com/hacrex/CapScope.git
cd CapScope
make run        # http://127.0.0.1:8080 (local testing, no auth)
make demo       # UI with fake data
make test       # full test suite
```

---

## Configuration (all via environment variables)

| Variable | Default | Meaning |
|----------|---------|---------|
| `LISTEN` | `:8080` | HTTP listen address |
| `DB_PATH` | `/data/capscope.db` | SQLite file — must be on a persistent volume |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | `unix://` socket or `tcp://host:port` (socket proxy) |
| `AUTH_USER` / `AUTH_PASS` | `admin` / none | Basic auth. Server refuses to start without `AUTH_PASS` |
| `ALLOW_NO_AUTH` | `false` | Set `true` to run without auth (local testing only) |
| `AUTH_PROVIDER` | `local` | `local` = shared password above; `proxy` = trust `PROXY_USER_HEADER` set by your reverse proxy (Authelia/Authentik/Cloudflare Access). Never expose CapScope's port directly in this mode |
| `PROXY_USER_HEADER` | `Remote-User` | Which header carries the identity in `proxy` mode (`X-WebAuth-User` works too) |
| `LOG_RETENTION` | `7d` | Delete logs older than this (`d`, `h`, `m`) |
| `METRICS_RETENTION` | `7d` | Same for CPU/memory samples |
| `SNAPSHOT_RETENTION` | `30d` | Same for crash snapshots |
| `MAX_DB_MB` | `1024` | Hard cap on database size; oldest data dropped first |
| `STATS_INTERVAL` | `10s` | Metric sample interval per container (min 1s) |
| `INITIAL_TAIL` | `200` | Lines backfilled the first time a container is seen |
| `MAX_LINE_BYTES` | `16384` | Lines longer than this are truncated |
| `REDACT` | `true` | Mask secrets before storage |
| `ACCESS_LOG` | `auto` | Access-log parsing: `auto`, `all`, `off` |
| `ANON_IPS` | `true` | Anonymize client IPs in stored access logs |
| `SNAPSHOT_KINDS` | `crash,oom` | Which stops create snapshots |
| `SNAPSHOT_LOG_LINES` | `200` | Log lines per crash snapshot |
| `RESTART_LOOP_COUNT` | `3` | Deaths in window to flag a restart loop |
| `RESTART_LOOP_WINDOW` | `5m` | Window for restart-loop detection |
| `INCLUDE` / `EXCLUDE` | none | Regexes matched against container and app names |
| `APP_RETENTION` | none | Per-app retention: `web=3d,api=1d` |
| `APP_MAX_MB` | none | Per-app size budget: `web=500,api=200` |
| `ALERTS_FILE` | `/data/alerts.json` | JSON alert-rules file, hot-reloaded |
| `ALERTS_INTERVAL` | `1m` | Alert evaluation interval (event rules fire immediately instead) |
| `ALERT_WEBHOOK_URL` | none | Generic JSON webhook |
| `ALERT_TELEGRAM_TOKEN` / `ALERT_TELEGRAM_CHAT_ID` | none | Telegram Bot API |
| `ALERT_SLACK_URL` | none | Slack incoming webhook |
| `ALERT_DISCORD_URL` | none | Discord webhook |
| `ALERT_NTFY_URL` | none | ntfy topic URL, e.g. `https://ntfy.sh/capscope` |
| `ALERT_NTFY_TOKEN` | none | Optional ntfy access token (Bearer) |
| `UPDATE_CHECK` | `true` | Daily registry check for newer images; feeds `update_available` alert events |
| `UPDATE_CHECK_INTERVAL` | `24h` | Update-check cadence (min 1h) |
| `AGENT_HOST_ID` | hostname | Stable host identity; tags collected rows with `host_id` for multi-host filtering |
| `AGENT_HUB_LISTEN` | none | Host:port to listen for agent connections (multi-node hub mode) |
| `AGENT_HUB_PINNED_FINGERPRINTS` | none | Comma-separated SHA-256 fingerprints of allowed agent client certs |
| `AGENT_HUB_CERT` / `AGENT_HUB_KEY` | none | Hub server certificate/key PEM for mTLS |
| `AGENT_HUB_CA_CERT` | none | CA certificate PEM used to verify agent client certs |
| `AUTH_PROVIDER` | `local` | `local` = shared password; `proxy` = reverse-proxy header; `oidc` = OpenID Connect |
| `OIDC_ISSUER` | none | OIDC issuer URL (required for `oidc` provider) |
| `OIDC_CLIENT_ID` | none | OAuth 2.0 client ID (required for `oidc` provider) |
| `OIDC_CLIENT_SECRET` | none | OAuth 2.0 client secret (required for `oidc` provider) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

---

## Behind an auth proxy (Authelia, Authentik, Cloudflare Access)

If you already run a forward-auth proxy, let it own authentication:

```yaml
services:
  capscope:
    image: ghcr.io/hacrex/capscope:latest
    environment:
      AUTH_PROVIDER: proxy            # no AUTH_PASS needed
      PROXY_USER_HEADER: Remote-User  # the header your proxy sets after auth
    volumes:
      - capscope-data:/data
      - /var/run/docker.sock:/var/run/docker.sock
    restart: unless-stopped
```

CapScope then trusts `Remote-User` as the caller identity and serves the UI
without a password prompt. Two rules:

- **Never expose CapScope's port directly in this mode.** The header is trusted
  from anyone who connects directly, so publish only the proxy and keep
  CapScope on an internal network (`expose`, not `ports`).
- Fields like `X-WebAuth-User` work too — set `PROXY_USER_HEADER` to match
  whatever your proxy sends. Live log streaming (SSE) works through the proxy
  as usual.

---

## Users and per-app RBAC

Local and OIDC deployments support two persistent roles:

- **Admin** — full access to every application, user management, application
  assignments, alert rules, webhook tests, and deploy-marker writes.
- **Viewer** — read-only access to explicitly assigned applications. A viewer
  with no assignments sees no application data.

Log history, live SSE streams, exports, dashboards, metrics, access analytics,
crash snapshots, incident exports, deploy markers, and alert history all enforce
the viewer's application scope. Scope changes revoke the affected user's
active sessions, so a newly removed application takes effect immediately.

To manage scopes, sign in as an admin, open **Admin**, and use **Edit apps**
on a viewer row. Applications are assigned by name, for example `shop, blog`.
The same operation is available to automation through:

```bash
GET /api/users/{id}/apps
PUT /api/users/{id}/apps   # {"apps":["shop","blog"]}
```

The `PUT` endpoint is Admin-only. Admin users always have access to all apps;
their scope cannot be reduced through this endpoint.

`AUTH_PROVIDER=proxy` is intentionally different: proxy identities are not
stored as CapScope users, so they remain globally read-only. Use local or OIDC
authentication when per-user application scoping is required.

---

## Platform guides

CapScope works on any Docker host. Pick your platform:

- **Docker Compose / generic Docker** — `deploy/docker-compose.yml`
- **CapRover** — `docs/CAPROVER.md` (native app deployment with persistent `/data` and socket mount)
- **Coolify** — `deploy/COOLIFY.md`
- **Dokploy** — `deploy/DOKPLOY.md`
- **Dokku** — `deploy/DOKKU.md`
- **Portainer** — `deploy/OTHERS.md`

---

## Alert rules example

Alerts are driven by a JSON file (`ALERTS_FILE`), hot-reloaded without restart.

```json
{
  "rules": [
    { "name": "api-silent", "kind": "deadman", "apps": ["api"], "for": "5m" },
    { "name": "api-looping", "kind": "restart_loop", "apps": ["api"], "count": 3, "window": "10m" },
    { "name": "api-mem", "kind": "metric", "apps": ["api"], "metric": "mem_pct", "op": "gt", "value": 85, "for": "5m" },
    { "name": "api-5xx", "kind": "log_pattern", "apps": ["api"], "pattern": "status=5\\d\\d", "hits": 5, "window": "5m" },
    { "name": "api-error-spike", "kind": "log_pattern", "apps": ["api"], "pattern": "(?i)(error|fatal|panic)", "hits": 10, "window": "5m" },
    { "name": "api-crash", "kind": "event", "apps": ["api"], "event": "die" },
    { "name": "any-oom", "kind": "event", "event": "oom" },
    { "name": "api-sick", "kind": "event", "event": "health_status", "status": "unhealthy" }
]
}
```

Rule kinds: `deadman`, `restart_loop`, `metric` (cpu / mem / mem_pct),
`log_pattern`, `event` (Docker lifecycle: `die` / `oom` / `health_status` /
`update_available`).

Extras on the rules:

- `event: die` skips graceful/OOM stops by default (`exclude: [0,130,143,137]`;
  override with your own list, or `"exclude": []` to keep everything).
- `event: health_status` fires when the status equals `status` (default
  `unhealthy`).
- `event` rules fire the moment the collector sees the Docker event.
- Adding `window` to a `metric` rule switches to smoothing: the mean of all
  samples in the window is compared, so one spiky or spotty sample cannot
  flip the alert (default `window` absent = every sample must hold).
- `cooldown` is a Dozzle-compatible alias for `repeat` (silence between
  notifications of the same rule+app).
- Channel URLs/tokens accept `${ENV}` placeholders, expanded at send time;
  names starting with `AWS_`, `DOCKER_`, `KUBERNETES_` or `DOZZLE_` are
  refused so a committed rules file cannot leak host secrets.

Channels: webhook, Telegram, Slack, Discord, ntfy. `ALERT_WEBHOOK_URL` receives
an HTTP POST with a JSON alert payload whenever a rule fires. Configure it
alongside `ALERTS_FILE`, then use the Admin-only **Send test alert** button to
verify delivery before an incident. Channel URLs and tokens support
`${ENV_VAR}` expansion at send time; keep the webhook secret in the deployment
environment rather than committing it to `alerts.json`.

Alert rule saves, validation, rule testing, and webhook test sends are
Admin-only. Viewers may see alert history only for applications assigned to
their account.

---

## Architecture at a glance

```
Docker Engine  →  CapScope (one Go binary)  →  SQLite (one file)
                    │
                    ├─ collector: follows logs + stats per container
                    ├─ hub: SSE fan-out to the browser
                    ├─ store: WAL mode, batched inserts, retention
                    └─ api: REST + SSE + embedded single-page UI
```

- **Single writer**, read-only pool — UI queries never block ingestion.
- **WAL mode**, `synchronous=NORMAL`, batched inserts (~156k rows/sec store-level upper bound).
- **Ring buffers** keep the final log lines and metric samples for crash snapshots even if the DB batch hasn't flushed.
- **Static scratch image** — no shell, no OS layer, just a static musl binary.

---

## SSE load testing

The repository includes `cmd/sse-bench`, a connection soak tool for testing a
running CapScope instance. It opens clients in batches, keeps them connected,
counts received SSE events and read errors, and can sample the target process's
RSS:

```bash
go run ./cmd/sse-bench \
  -url 'http://127.0.0.1:8080/api/logs/stream?app=my-app' \
  -connections 500 \
  -step 50 \
  -duration 60s \
  -pid "$(pidof capscope)"
```

Use a session cookie or bearer token when the endpoint is protected. A zero
event count is normal when no new Docker log lines are generated during the
soak. Read errors reported only after the final result are expected when the
benchmark closes its clients; errors during the soak indicate a connection
stability problem.

---

## Limits to know

- **Multi-node requires agent mode.** A hub can ingest tagged logs and metrics
  from agents; without `AGENT_HUB` you see the Docker node where CapScope runs.
- **Crash catch-up** works only if the container still exists when CapScope starts (it replays Docker events from the last cursor, up to 24h back).
- **Log driver:** `json-file`, `local`, and `journald` are supported via the Docker logs API.
- **Redaction and level detection are heuristics** — don't rely on redaction as your only secret protection.
- **Docker socket access is root equivalent.** Use auth, HTTPS, and preferably a read-only socket proxy in production.

---

## Contributing

CapScope is a small Go project with no frontend build step — the UI is a single embedded HTML file.

```bash
# run tests
go test ./...

# vet
go vet ./...

# build
go build -o capscope ./cmd/capscope
```

Issues and pull requests are welcome. See `docs/` for the PRD, architecture notes, and API reference.

---

## Roadmap

- **v0.2** — Alerts (done), deploy markers (done), FTS5 search (done), crash catch-up (done)
- **v0.6 / v0.7** — access-log dashboard, crash history, deploy markers, dashboard + alert rule builder, Prometheus `/metrics` (all done)
- **v0.8** — live-tail hardening: gapless SSE resume (`Last-Event-ID`), stack-trace grouping, sidebar filter, cookie sessions for `EventSource` (done)
- **v0.9** — `AUTH_PROVIDER=proxy` forward-proxy header auth for Authelia / Authentik / Cloudflare Access (done)
- **v0.10** — OTel severityNumber/`severityText` + Pino numeric level parsing, raw `.txt` log export, Local/UTC time display, CI pipeline + UI syntax gate (done)
- **v0.11** — Alerts to Dozzle parity: container-event rules (die/OOM/health/update-available), metric smoothing + `cooldown`, ntfy channel, `${ENV}` channel placeholders, daily update-available registry checks (done)
- **v0.12** — Persistent login sessions across restarts, expiring share links/permalinks (`POST /api/share`, `/s/<token>`), any-range JSON export (`fmt=json`), self-contained HTML incident export (done)
- **v0.13** — Multi-host visibility: host selector chip, per-host views (containers/crashes/metrics/dashboard filtered by `host_id`), `?host=` permalinks, `GET /api/hosts` (done)
- **v0.14** — Multi-user auth: OIDC SSO (`AUTH_PROVIDER=oidc`, `OIDC_ISSUER`/`OIDC_CLIENT_ID`/`OIDC_CLIENT_SECRET`), users table with `admin`/`viewer` roles, user management API, audit trail on `access` rows (done)
- **v0.15** — MCP server over SQLite (Phase 6.4): `cmd/capscope mcp` runs an MCP server over stdio with tools `query_logs`, `list_snapshots`, `get_snapshot`, `run_sql`. Session-token authenticated.
- **v0.16** — Agent hub ingestion, per-app viewer RBAC, Admin application assignments, scoped live SSE/history/export APIs, webhook alert hardening, and `cmd/sse-bench` (done)
- **Next** — Access-log app attribution improvements, optional GeoIP enrichment, and additional Dozzle-compatible operational polish

---

## License

[MIT](LICENSE).

---

**Star this repo** if you run containers at home and ever wished you could see what a container printed before it was replaced.
