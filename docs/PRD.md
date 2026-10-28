# CapScope: Product Requirements Document

Status: MVP implemented (v0.1). Owner: Kashvit. Last updated: 2026-10-28.

## 1. Summary

CapScope is a self-hosted container log and stats viewer for CapRover and plain Docker hosts. It does what Dozzle does for live viewing and adds two things Dozzle leaves out on purpose: **persistent, searchable history** and **crash snapshots** that preserve the evidence when a container dies.

## 2. Problem

Running apps on CapRover means Docker Swarm services that are replaced on every deploy. Today:

1. `ctop` and `htop` show the present only. When a container is OOM killed and replaced, the memory trend and the last log lines are gone with it.
2. Dozzle is excellent for live logs but stateless. Once a container is removed, its logs cannot be searched.
3. Prometheus, Grafana and Loki answer these questions but are heavy to run and tune for a handful of apps on one or two servers.
4. Operators answering "what happened before it died?" or "did the deploy break it?" have no single place to look.

## 3. Goals

| # | Goal | Measure |
|---|---|---|
| G1 | Live log tailing and search that feels as light as Dozzle | Open the UI, see the last 500 lines of an app in under 1 second on a typical server |
| G2 | Keep history across redeploys | Logs for a removed container are searchable for the configured retention |
| G3 | Preserve crash evidence automatically | Every OOM or non-zero exit of a tracked container yields a snapshot with exit code, summary, last lines and memory trend |
| G4 | Run with almost no operations | One container, one volume, a handful of environment variables, no external database |
| G5 | Be safe by default | Auth required, secrets masked before storage, read-only Docker access possible |

## 4. Non-goals (MVP)

- Replacing Prometheus, Grafana or Loki for large fleets.
- Multi-node Swarm aggregation (agent mode is roadmap).
- Alerting and notifications (roadmap, phase 2).
- Full-text indexing, log pattern clustering, AI summaries (roadmap).
- Multi-user accounts and per-app permissions (roadmap).
- Acting on containers (restart, scale, exec). CapScope is read-only toward Docker.

## 5. Users and scenarios

**Primary user**: a developer or DevOps engineer who runs several apps on one CapRover server and is also the person paged when one breaks.

| Scenario | Today | With CapScope |
|---|---|---|
| App restarted at 03:12 and nobody knows why | Logs gone, only "restarted" in CapRover | Crash snapshot shows exit 137, OOM flag, memory climbing to the limit, last error lines |
| Customer reports an error from yesterday | Old container is gone | Search stored logs by text or regex, filter by level and replica |
| Which app is eating memory? | `ctop` over SSH, present only | Metrics tab, per app or per replica, 15 minutes to 7 days |
| Hand an incident to a teammate | Copy and paste from a terminal | Download the snapshot as a Markdown report |
| Is a secret leaking into logs? | Unknown | Common secret shapes are masked before they are stored or shown |

## 6. Functional requirements

Priority: **M** = MVP (implemented), **P1** = next release, **P2** = later.

### 6.1 Collection

| ID | Requirement | Pri | Status |
|---|---|---|---|
| C1 | Discover running containers at startup and on `start` events; reconcile every 30 s | M | Done |
| C2 | Follow stdout and stderr per container, including TTY containers | M | Done |
| C3 | Resume after stream drops or restarts without duplicating lines | M | Done |
| C4 | Group containers into apps; unwrap CapRover `srv-captain--` names and replica ids | M | Done |
| C5 | Sample CPU, memory, network, block I/O and pids at a configurable interval | M | Done |
| C6 | Include and exclude filters by regex | M | Done |
| C7 | Detect log level from JSON fields and level words | M | Done (heuristic) |
| C8 | Capture exits that happened while CapScope was down | P1 | Not done |
| C9 | Nginx access log parsing (status codes, latency, top paths) | P1 | Not done |
| C10 | Agent mode for multi-node Swarm | P2 | Not done |

### 6.2 Storage and retention

| ID | Requirement | Pri | Status |
|---|---|---|---|
| S1 | Persist logs, metrics, container metadata and snapshots in SQLite | M | Done |
| S2 | Retention by age per data type | M | Done |
| S3 | Hard cap on database size; drop oldest logs first | M | Done |
| S4 | Redact secrets before storage | M | Done (best effort) |
| S5 | FTS5 full-text index for fast search at scale | P1 | Not done |
| S6 | Normalize repeated container fields to cut bytes per row | P1 | Not done |
| S7 | Per-app retention and size budgets | P1 | Not done |

### 6.3 Viewing

| ID | Requirement | Pri | Status |
|---|---|---|---|
| V1 | App sidebar with running replicas, CPU, memory and error and warning counts for the last hour | M | Done |
| V2 | Live tail with follow, pause and load older | M | Done |
| V3 | Search by substring or regex; filter by level, stream and replica | M | Done |
| V4 | CPU and memory charts from 15 minutes to 7 days, per app or replica | M | Done |
| V5 | Crash list and detail view with summary, facts, memory trend and last lines | M | Done |
| V6 | Download a crash as Markdown | M | Done |
| V7 | Merge several apps into one timeline; search by request id across apps | P1 | Not done |
| V8 | Pretty-print JSON log lines | P1 | Not done |
| V9 | Deploy markers on charts and log timeline | P1 | Not done |
| V10 | Shareable expiring permalinks to a time range | P2 | Not done |

### 6.4 Crash snapshots

| ID | Requirement | Pri | Status |
|---|---|---|---|
| X1 | Create a snapshot on `die` for configurable kinds (`crash`, `oom`, `killed`, `clean_exit`) | M | Done |
| X2 | Include exit code, OOM flag, Docker error, restart count, uptime, memory limit | M | Done |
| X3 | Include the last N log lines and last N metric samples from memory, so the final lines are never missing | M | Done |
| X4 | Rule-based summary of the likely meaning (137, 139, 143, 126, 127, 1, OOM) | M | Done |
| X5 | Never store environment variables or labels | M | Done |
| X6 | Rebuild a snapshot when a container died before it was tracked | M | Done (no metrics) |
| X7 | Restart-loop detection and counters | P1 | Not done |
| X8 | Snapshot on health check failure | P2 | Not done |

### 6.5 Alerts (phase 2)

| ID | Requirement | Pri |
|---|---|---|
| A1 | Dead-man alert: no logs from an app for N minutes | P1 |
| A2 | Restart loop: more than N restarts in M minutes | P1 |
| A3 | Metric threshold with duration (memory above 85 percent for 5 minutes) | P1 |
| A4 | Log pattern rule (regex match count in a window) | P1 |
| A5 | Channels: webhook, Telegram, Slack, Discord | P1 |

### 6.6 Security and access

| ID | Requirement | Pri | Status |
|---|---|---|---|
| Z1 | Basic auth, refuse to start without a password unless explicitly allowed | M | Done |
| Z2 | Read-only use of the Docker API; works through a socket proxy over TCP | M | Done |
| Z3 | Single shared credential | M | Done |
| Z4 | Per-app permissions and an audit trail of who viewed or downloaded what | P2 | Not done |
| Z5 | OIDC single sign-on | P2 | Not done |

## 7. Non-functional requirements

| Area | Target | Evidence so far |
|---|---|---|
| Footprint | Small enough to run beside the apps it watches | Not measured against a real workload yet. Treat as a goal to verify. |
| Ingest | Handle bursts from a busy app without dropping Docker reads | Store inserts about 156k rows per second in a 2 vCPU sandbox (micro benchmark, upper bound) |
| Storage | Predictable | About 400 bytes per 130 byte log line, so `lines per second x 86400 x 400` bytes per day |
| Reliability | Survive Docker stream drops and its own restarts | Reconnect logic covered by design and partially by tests; not soak tested |
| Compatibility | CapRover on single node Swarm, plain Docker | Tested against a fake Docker API only |
| Browser support | Current Chrome, Firefox, Safari | Exercised in headless Chromium |
| Safety of data | No secrets in snapshots or stored logs | Redaction covered by unit tests for common shapes. Not a guarantee. |

## 8. Success metrics

- Time to answer "why did it restart?" drops from minutes of SSH and guessing to under one minute in the Crashes tab.
- 100 percent of OOM kills and non-zero exits on tracked containers produce a snapshot during a one week trial.
- Zero plaintext secrets found in a manual review of one week of stored logs from the trial apps.
- CapScope uses less memory than the app it is watching with the smallest footprint. (To be measured.)

## 9. Risks and open questions

| Risk or question | Mitigation or next step |
|---|---|
| Docker socket is root equivalent | Auth, HTTPS, socket proxy guide, GET-only client |
| Redaction misses a secret shape | Document it as best effort, add patterns from real traces, keep logs access authenticated |
| Heavy logging apps fill the cap quickly | Document capacity formula, add per-app budgets (S7), normalize rows (S6) |
| `LIKE` and regex search slow at large volume | FTS5 (S5) before recommending large retention |
| cgo build in Docker | Documented swap to a pure-Go SQLite driver if the toolchain is a problem |
| Not yet run on a real CapRover Swarm | First deploy on a staging server, compare with `docker stats` and `docker logs` |
| Does CapRover rewrite or rotate json-file logs in a way that loses lines? | Check on first deploy with a noisy test app |
| Should `killed` snapshots default on when a stop follows a failed health check? | Decide after one week of real data |

## 10. Roadmap

| Phase | Scope |
|---|---|
| **v0.1 (this MVP)** | Collection, SQLite history, live view, search, metrics, crash snapshots, auth, redaction, Docker and CapRover packaging |
| **v0.2** | Alerts (A1 to A5), restart-loop detection (X7), deploy markers (V9), catch-up of exits missed while down (C8) |
| **v0.3** | FTS5 search (S5), row normalization (S6), per-app budgets (S7), JSON pretty-print (V8), cross-app timeline (V7) |
| **v0.4** | Nginx access log dashboard (C9), Prometheus `/metrics`, Markdown and JSON export of any log range |
| **v1.0** | Agent mode for multi-node (C10), per-app permissions and audit (Z4), OIDC (Z5) |

## 11. Acceptance checklist for v0.1

- [x] Starts only with a password or an explicit opt out
- [x] Follows logs and stats for running containers and resumes after stream drops
- [x] Stores logs and metrics and enforces age and size retention
- [x] Writes a snapshot on OOM and on non-zero exit, with the final log lines and a memory trend
- [x] UI for live logs, search, metrics and crashes with no console errors in headless Chromium
- [x] Unit and end-to-end tests pass with the race detector
- [ ] Verified against a real Docker daemon and a CapRover Swarm
- [ ] Docker image built and run on CapRover
- [ ] One week soak test with memory and disk measurements
