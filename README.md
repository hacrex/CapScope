# CapScope

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
| SQL queries over logs | No | Yes — embedded SQL engine |
| Split screen | Yes — up to 4 replicas side by side | Yes |
| CPU / memory charts | Yes — plus persisted history in SQLite | Yes — live stats with historical visualizations |
| Crash / OOM snapshots | **Yes** — exit code, OOM flag, uptime, restarts, memory trend, last N log lines, plain-language summary, Markdown export | No |
| Secret redaction before storage | Yes (JWT, bearer, AWS keys, PEM, URL creds, `password=`/`token=`) | No (nothing is stored) |
| Authentication | Basic auth, refuses to start without a password | Multi-user file auth + forward-proxy auth (Authelia etc.) |
| Multi-host | Single node (per-node agent on roadmap) | **Yes** — agent mode |
| Orchestration | Single Docker Engine | **Yes** — Docker Swarm, Kubernetes, Podman, Colima |
| Container list | Per-app sidebar, CapRover `srv-captain--` names unwrapped | Fuzzy container search, multi-host tree, app icons |
| Log download | Crash snapshot Markdown export | Yes — download logs |
| Themes | Auto / dark / light | Dark mode |
| Frontend stack | Single embedded HTML file, no build step, no JS deps | Vue SPA (Bun + Vite) |
| Image size | One Go binary + cgo SQLite (image not yet built/published) | ~7 MB compressed, built `FROM scratch` |
| Analytics | None | Google Analytics (opt-out via `--no-analytics`) |
| Maturity | Early — single maintainer, not yet run on a real CapRover swarm | 8+ years, hundreds of containers tested, Docker-sponsored |

**Pick Dozzle** if you want a polished multi-host live viewer with auth options and you do not need history — it will feel more finished.

**Pick CapScope** if you want to answer "what did the container print before it died, and was it OOM?" after the fact: persistence, FTS search over old logs, and crash snapshots are the entire point of this project.

## What you get

| Area | Features |
|---|---|
| Live view | Per-app sidebar (CapRover `srv-captain--` names are unwrapped), virtual-scrolled live log tail over SSE (100k lines in memory), pause, follow, load older, split view for comparing up to 4 replicas side by side |
| Search | FTS5 token/prefix search over persisted logs with substring fallback, or regex; filter by level, stream (stdout/stderr) and replica |
| History | Logs and CPU/memory samples persisted in SQLite with age and size retention |
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
| `MAX_DB_MB` | `1024` | Cap on live data. Oldest logs, then metrics, are dropped when exceeded |
| `STATS_INTERVAL` | `10s` | How often a metric sample is stored per container (minimum 1s) |
| `INITIAL_TAIL` | `200` | Lines to backfill the first time a container is seen |
| `MAX_LINE_BYTES` | `16384` | Longer lines are truncated |
| `REDACT` | `true` | Mask JWTs, bearer tokens, `password=`/`token=` style values, URL credentials, AWS keys, PEM keys |
| `SNAPSHOT_KINDS` | `crash,oom` | Which stops create a snapshot: `crash`, `oom`, `killed`, `clean_exit` |
| `SNAPSHOT_LOG_LINES` | `200` | Log lines kept in each snapshot |
| `SNAPSHOT_METRIC_SAMPLES` | `60` | Metric samples kept in each snapshot (10 minutes at the default interval) |
| `INCLUDE` / `EXCLUDE` | none | Regexes matched against container name and app name |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

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
internal/config     env parsing and validation
internal/docker     minimal Docker Engine API client (unix and tcp), stream decoding
internal/collector  container discovery, log and stats followers, events, snapshots, retention loop
internal/store      SQLite schema, queries, pruning
internal/hub        live fan-out to SSE clients
internal/logproc    secret redaction and log level detection
internal/api        HTTP API, SSE, auth, Markdown export
internal/web        embedded single-page UI
docs/               PRD, architecture, CapRover guide, API reference
```

## Limits to know about

- **Single node.** On a multi-node Swarm you only see containers on the node CapScope runs on. A per-node agent is on the roadmap.
- **Crashes while CapScope is down are not captured.** It follows Docker events live. After a restart it resumes logs from the last stored timestamp but does not reconstruct earlier deaths.
- **Log driver.** The Docker logs API only works for the `json-file`, `local` and `journald` drivers. CapRover uses `json-file` by default.
- **Search.** Plain-text search uses an FTS5 index (token and prefix matching) with a `LIKE` fallback for mid-word substrings. Regex search scans up to the newest 200,000 matching candidates and is not indexed.
- **Level detection and redaction are heuristics**, not guarantees. Do not rely on redaction as your only protection for secrets in logs.
- **Docker socket access is root equivalent.** Use auth, HTTPS, and ideally a socket proxy. See docs/CAPROVER.md.

## What was verified

Unit tests cover log stream decoding, stats math, redaction, level detection, SQLite queries (including FTS5 search and its LIKE fallback), retention and size cap, the HTTP API and SSE. An end-to-end test runs the collector against a fake Docker daemon that emits an OOM and a die event and checks the saved snapshot. The earlier UI was exercised in headless Chromium against seeded data; the virtual-scroll/split-pane/theme rewrite has not yet been re-verified in a browser. It has **not** yet been run against a real Docker daemon or a CapRover Swarm, and the Dockerfile has not been built, so expect to shake out small issues on first deploy.

## License

Choose one before publishing. MIT is a reasonable default.
