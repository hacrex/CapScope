# CapScope

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00add8.svg)](https://golang.org/)
[![Docker Image](https://img.shields.io/badge/docker-capscope-2496ed?logo=docker)](https://hub.docker.com/r/hacrex/capscope)
[![Go Reference](https://pkg.go.dev/badge/github.com/hacrex/CapScope.svg)](https://pkg.go.dev/github.com/hacrex/CapScope)
[![Website](https://img.shields.io/badge/web-capscope.live-f89c1c)](https://capscope.live)

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
| **Alerts** | Dead-man (silence), restart loop, metric threshold (with duration), and log-pattern rules; webhook, Telegram, Slack, and Discord channels |
| **Secret redaction** | Masks JWTs, bearer tokens, `password=`/`token=` values, URL credentials, AWS keys, and PEM keys before storage |
| **Auth** | Basic auth required by default; refuses to start without a password unless you explicitly opt out |

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
| `ALERTS_INTERVAL` | `1m` | Alert evaluation interval |
| `ALERT_WEBHOOK_URL` | none | Generic JSON webhook |
| `ALERT_TELEGRAM_TOKEN` / `ALERT_TELEGRAM_CHAT_ID` | none | Telegram Bot API |
| `ALERT_SLACK_URL` | none | Slack incoming webhook |
| `ALERT_DISCORD_URL` | none | Discord webhook |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

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
    { "name": "api-5xx", "kind": "log_pattern", "apps": ["api"], "pattern": "status=5\\d\\d", "hits": 5, "window": "5m" }
  ]
}
```

Rule kinds: `deadman`, `restart_loop`, `metric` (cpu / mem / mem_pct), `log_pattern`.  
Channels: webhook, Telegram, Slack, Discord. Use the **Send test alert** button in the UI to verify before an incident.

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

## Limits to know

- **Single node.** You see containers on the node CapScope runs on. Multi-node agent mode is on the roadmap.
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
- **v0.3** — Row normalization, per-app budgets, login screen UI
- **v0.4** — Prometheus `/metrics` endpoint, agent mode for multi-node
- **v1.0** — Per-app permissions, OIDC, access-log app attribution + GeoIP

---

## License

[MIT](LICENSE).

---

**Star this repo** if you run containers at home and ever wished you could see what a container printed before it was replaced.
