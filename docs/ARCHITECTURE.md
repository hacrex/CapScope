# CapScope architecture

## 1. Overview

CapScope is one Go process with four jobs: watch Docker, store what it sees, serve it, and snapshot crashes. There is no external service. State lives in one SQLite file plus small in-memory ring buffers.

```
                    +------------------------------ CapScope process ------------------------------+
                    |                                                                                |
 Docker Engine      |   collector.Manager                                    store (SQLite, WAL)     |
 (socket or proxy)  |   +---------------------+                              +-------------------+   |
                    |   | eventLoop           |-- start/die/oom/kill ------->| containers        |   |
 GET /events -------+-->| reconcile (30s)     |                              | logs              |   |
                    |   |                     |                              | metrics           |   |
 GET /containers/   |   | per container:      |   rows (chan, batched 250ms) | snapshots         |   |
   {id}/logs -------+-->|  logLoop  -> redact |----------------------------->|                   |   |
 GET /containers/   |   |           -> level  |        |                     +---------^---------+   |
   {id}/stats ------+-->|  statsLoop          |--------+---- metrics -------------------+             |
                    |   |  ring: last N lines |                                        |             |
                    |   |  ring: last N stats |-- on die: build snapshot --------------+             |
                    |   +---------------------+                                                      |
                    |            | committed rows (with ids)                                         |
                    |            v                                                                   |
                    |   hub.Hub (fan-out) ---> api.Server ---> browser (SSE, JSON, embedded UI)      |
                    +--------------------------------------------------------------------------------+
```

## 2. Components

| Package | Responsibility |
|---|---|
| `internal/docker` | Thin client for the Docker Engine HTTP API over `unix://` or `tcp://`. Decodes the multiplexed log framing (8-byte header per frame), the TTY raw stream, stats JSON and events JSON. No Docker SDK dependency. |
| `internal/collector` | `Manager` owns one `tracker` per running container. Runs discovery, log and stats followers, the events loop, snapshot creation and the retention loop. |
| `internal/store` | Schema, migrations (`PRAGMA user_version`), batched inserts, filtered queries, metric bucketing, pruning and size cap. One writer connection, a small read-only pool. |
| `internal/hub` | Non-blocking fan-out of committed log rows to SSE subscribers, with per-subscriber filters and a drop counter. |
| `internal/logproc` | `Redact` (secret masking) and `Level` (level heuristic). |
| `internal/api` | JSON endpoints, SSE stream, Basic auth, Markdown export, embedded UI. |
| `internal/alerts` | Rule loading and validation (JSON file, hot-reloaded), evaluation engine over the store, notification channels (webhook, Telegram, Slack, Discord). |
| `internal/config` | Environment parsing and validation. Fails fast on a missing password. |

## 3. Data flow

### 3.1 Log ingestion

1. `ensure(id)` inspects the container, derives the app name (see 3.4) and starts `logLoop` and `statsLoop`.
2. `logLoop` calls `GET /containers/{id}/logs?follow=1&timestamps=1`. First connect uses `tail=INITIAL_TAIL`, later connects use `since=lastTS+1ns`, so a dropped stream resumes without duplicates.
3. Each line is timestamp-parsed, de-duplicated against `lastTS[id]`, redacted, level-tagged, pushed into the tracker's ring buffer, and sent on a buffered channel.
4. `writer` drains the channel, batching up to 1000 rows or 250 ms, inserts them in one transaction, then publishes the committed rows (now with ids) to the hub.
5. SSE clients receive rows only after commit, so every row they see has a stable id and the UI can reconcile with a history query using `id`.

Back-pressure: if SQLite stalls, the channel fills; once full, further log lines from the Docker streams are dropped (counted and reported by a periodic `log lines dropped (write pressure)` warning) so that one stuck writer cannot freeze every container's stream. The hub never blocks either: slow browsers lose rows and are told via a `dropped` event.

### 3.2 Metrics

`statsLoop` reads the stats stream (about 1 frame per second), skips the first frame (no CPU baseline), and stores one sample every `STATS_INTERVAL`. CPU uses the same formula as `docker stats` (`cpuDelta / systemDelta * cpus * 100`). Memory subtracts the page cache (`inactive_file`) so the number matches what the OOM killer cares about more closely. Samples go to SQLite and to the ring buffer.

### 3.3 Crash snapshot sequence

```
Docker            eventLoop             Manager                     store
  | oom event --->  | setOOM() on tracker, or remember the flag        |
  | kill (sig) -->  | setSignal() on tracker, or remember the flag     |
  | die (exit=137)->| detach tracker from map (synchronous)            |
  |                 | go finishDie():                                  |
  |                 |   tracked: mark exited, wait <=2s for logLoop    |
  |                 |            to drain, cancel stats follower       |
  |                 |   no tracker (down / between reconciles):        |
  |                 |     rebuild from inspect, last lines from        |
  |                 |     SQLite or the container's log file           |
  |                 |   inspect (OOMKilled, RestartCount, limits,      |
  |                 |            Started/FinishedAt)                   |
  |                 |   classify -> oom | crash | killed | clean_exit  |
  |                 |   skip when (container, die_ts) already stored   |
  |                 |   count deaths in the restart-loop window        |
  |                 |   if kind enabled: build snapshot from           |
  |                 |     ring logs + ring metrics + inspect summary   |
  |                 |   summarize() rule-based text -------------------> INSERT snapshots
  |                 |   save event cursor (die/oom force a write) ----> state
```

Design points:

- The tracker is detached from the map **inside the event goroutine** before any async work. With a restart policy, Docker reuses the container id, so the next `start` event must create a fresh tracker rather than find the dying one.
- Snapshot evidence comes from **in-memory ring buffers**, not from SQLite, because the database write is batched and can lag the final lines by up to 250 ms.
- If a `die` arrives for a container with no tracker (CapScope was down, or it started and died between reconcile passes), the manager rebuilds a tracker from `inspect`, takes the last lines from SQLite when any were stored, and otherwise reads the tail of the container's Docker log file — which still holds the final lines of a stopped container. Metrics are empty in that case.
- **Catch-up after downtime**: the newest processed event time is persisted in `state`. On startup the event stream starts from that cursor (up to 24 h back, as far as the daemon's buffer goes), so deaths that happened while CapScope was off are replayed and snapshotted like live ones. `(container_id, die_ts)` deduplication makes a replayed death harmless. A `die` whose container id has since been recreated is snapshotted without disturbing the live tracker of the new incarnation.
- **Restart-loop detection**: the deaths of one container inside `RESTART_LOOP_WINDOW` (counted from stored snapshots of that container, which keeps rollout noise out by default) are stored per snapshot as `restarts_window`; at `RESTART_LOOP_COUNT` the summary calls it out and a warning is logged.
- Classification: OOM flag wins (from the tracker, a remembered pending flag, or the inspect flag of a stopped container). Exit 0 is `clean_exit`. A kill or stop signal seen earlier means `killed`. Everything else is `crash`.
- `summarize` states what the data shows and what the exit code usually means (137, 139, 143, 126, 127, 1). It is explicitly rule-based, not a diagnosis.
- `inspect` is stored in a trimmed form. Environment variables and labels are deliberately left out because they often hold secrets.

### 3.4 App naming

CapRover runs each app as a Swarm service named `srv-captain--<app>`, with tasks named `srv-captain--<app>.<slot>.<taskid>`. The app name is taken from the label `com.docker.swarm.service.name` and the replica from the task name. Fallbacks: Docker Compose labels, then the container name. Replicas of one app appear together in the UI.

### 3.5 Alert evaluation

```
ALERTS_FILE (JSON)              store                     channels
  | load + validate (on mtime     |                          |
  |  change, no restart)          |                          |
  |                               |                          |
  engine.Run ticker (ALERTS_INTERVAL, default 1m)             |
    | for each rule, for each app (rule.apps, or all known)   |
    |   evaluate kind:                                       |
    |     deadman      -> LastLogTS(app) vs now-for           |
    |     restart_loop -> CountAppDeathsSince(app, now-window) |
    |     metric       -> MetricSamples(app, now-for): every  |
    |                     sample must satisfy op/value, and   |
    |                     the newest sample must be fresh     |
    |     log_pattern  -> CountLogsMatching(app, re, window)  |
    |   if firing AND LastAlertTS(rule,app) older than repeat |
    |     InsertAlert -----------------------------------------> notify each channel
    |                                                          (10s timeout each,
    |                                                          one failure never
    |                                                          blocks the rest)
```

Design points:

- The rules file is re-read when its mtime changes, so editing it needs no restart. A malformed file keeps the previous rules and logs the error.
- The `alerts` table doubles as the silence mechanism: `LastAlertTS(rule, app)` is the last firing, and a new notification is suppressed until `repeat` has passed. This survives restarts without extra state.
- The metric rule requires **every** sample in the `for` window to satisfy the threshold, so one cool sample inside it stops the alert, and it refuses to fire on stale data (newest sample older than twice the window) so a stopped app cannot page forever.
- Channels are best-effort: a failing webhook is logged and the next channel still runs.

### 3.6 Deploy markers

A deploy is a plain row in `deploys` (`app`, `version`, optional `note`, timestamp). It is recorded by a CI step (`POST /api/deploys`) or the UI's **Record deploy** button. The UI reads them back for three views: dashed vertical lines on the CPU and memory charts, inline marker rows interleaved into the log timeline by timestamp, and — on the crash detail view — the newest marker before the death, so "did a release just land?" is answered at a glance. The Markdown crash export carries the same "Last deploy" line for incident handoff.

## 4. Storage

SQLite via `mattn/go-sqlite3` (cgo). WAL mode, `synchronous=NORMAL`, `busy_timeout=5000`. A single writer connection serializes all writes; reads use a separate read-only pool of 4, so UI queries do not wait for ingestion.

| Table | Key columns | Notes |
|---|---|---|
| `logs` | `id`, `ts` (ns), `container_id`, `container`, `app`, `stream`, `level`, `line` | Indexes: `(app,id)`, `(container_id,id)`, `(ts)`. About 400 bytes per row measured for 130 byte lines. |
| `metrics` | `ts` (ms), `container_id`, `app`, cpu, mem, mem_limit, net, blk, pids | Indexes: `(app,ts)`, `(container_id,ts)`. App charts average per container inside a bucket, then sum across containers. |
| `containers` | `id`, `name`, `app`, `image`, `replica`, `state`, first and last seen | Drives the sidebar and history for removed containers. |
| `snapshots` | metadata columns (`die_ts`, `restarts_window` included) plus `logs_json`, `metrics_json`, `inspect_json` | Heavy columns are only read for the detail view. `(container_id, die_ts)` deduplicates replayed deaths. |
| `alerts` | `id`, `ts` (ms), `rule`, `app`, `kind`, `message` | One row per firing. `(rule, app, ts)` is read for the silence check, `(app, ts)` for the history feed. |
| `deploys` | `id`, `ts` (ms), `app`, `version`, `note` | Release markers (V9), written by CI or the UI. `(app, ts)` serves the chart and crash views. |
| `state` | `key`, integer `value` | Tiny checkpoints: currently only `event_cursor_ns`, the Docker event replay cursor. |

Schema versioning uses `PRAGMA user_version` with an ordered list of migrations. `auto_vacuum=INCREMENTAL` is set before the first table is created so retention can return space to the OS.

**Retention** runs 30 seconds after start and then every 10 minutes: delete by age in batches of 5000 rows (short write locks), then enforce `MAX_DB_MB` against live bytes (pages minus free pages) by dropping the oldest 10 percent of logs per round, then metrics, then access, then snapshots, alerts and deploys, then `incremental_vacuum`. Per-app budgets (`APP_RETENTION`, `APP_MAX_MB`) run between the age prune and the global size cap, so a single noisy app is trimmed against its own retention and byte budget before the whole database is squeezed.

**Measured throughput** (sandbox with 2 vCPUs, batches of 1000 rows, WAL, `synchronous=NORMAL`): about 156,000 rows per second inserted. This is a store-level micro benchmark. It does not include Docker stream decoding, redaction or level detection, so treat it as an upper bound, not an end-to-end rate.

## 5. Concurrency model

| Goroutine | Count | Lifetime |
|---|---|---|
| `eventLoop` | 1 | process |
| `reconcile` ticker | 1 | process |
| `writer` | 1 | process |
| `maintenance` | 1 | process |
| `alerts.Run` | 1 | process |
| `logLoop` + `statsLoop` | 2 per running container | until the container stops |
| `finishDie` | 1 per death | short |
| SSE handler | 1 per open browser stream | until disconnect |

Shared state is a `tracked` map, a `lastTS` map and a `pending` map (oom/kill flags of untracked containers) guarded by one mutex in `Manager`, plus a small mutex inside each `tracker`. `ensure` sets the tracker's cancel function **before** publishing it in the map so a fast `die` event can never see a nil cancel.

## 6. API

See [API.md](API.md). The UI is a static page that only uses that public API.

## 7. Security model

| Concern | Approach |
|---|---|
| Access to the UI and API | HTTP Basic auth, constant-time comparison, required unless `ALLOW_NO_AUTH=true`. `/healthz` is open. |
| Transport | CapScope speaks plain HTTP. Put it behind CapRover HTTPS. |
| Docker access | The socket is root equivalent. Prefer `DOCKER_HOST=tcp://` to a read-only socket proxy that exposes only `GET /containers/*`, `/events` and `/_ping`. CapScope only issues GET requests. |
| Secrets in logs | Best-effort masking before storage and before any display. Patterns: JWT, Bearer/Basic, `key=value` for password, secret, token, api key, authorization, URL credentials, AWS key ids, PEM private keys. |
| Secrets in snapshots | Environment and labels are never stored. Logs go through the same redaction. |
| Multi-user | Not in the MVP. One shared credential. Per-app permissions and audit log are on the roadmap. |

## 8. Failure modes

| Failure | Behavior |
|---|---|
| Docker event stream drops | Reconnect after 2 s with `since=lastEventTime`, then a full reconcile. Duplicate events are harmless: a death already snapshotted is skipped by `(container_id, die_ts)`. |
| Log stream drops | Reconnect after 1 s from `lastTS+1ns` if the container is still running. |
| Container vanishes without a `die` event | Reconcile drops its tracker after 20 s. No snapshot is created. |
| CapScope restarts | Followers resume from stored timestamps. Deaths that happened while it was down are replayed from the persisted event cursor (gaps up to 24 h) and snapshotted, as long as the container still exists. |
| SQLite write error | Logged, the batch is dropped. Following continues. |
| Disk full | Writes fail and are logged. Lower `MAX_DB_MB` so the size cap triggers before the volume fills. |
| Browser too slow | Hub drops rows for that subscriber and emits a `dropped` event. |

## 9. Decisions and trade-offs

| Decision | Why | Cost |
|---|---|---|
| Raw Docker HTTP instead of the Docker SDK | Fewer dependencies, easy to run against a socket proxy, only the four endpoints needed | We maintain the small decoder ourselves |
| `mattn/go-sqlite3` (cgo) | Mature, fast, the module was available offline when this was built | Needs a C toolchain in the build stage. `modernc.org/sqlite` is a pure-Go swap in `internal/store` only: change the import, driver name `sqlite`, and the DSN pragma syntax |
| SSE instead of WebSocket | One-way data, works through nginx with one header, automatic reconnect | No client to server channel (not needed yet) |
| Ring buffer plus SQLite | Snapshots need the final lines even if the DB batch has not flushed | Some memory per container (about 200 lines and 60 samples) |
| `LIKE` search first | Predictable substring semantics, no extra index | Slower than FTS at tens of millions of rows. FTS5 is a planned addition |
| Redact on ingest | Secrets never reach disk or the browser | Original lines are not recoverable. A false positive hides a harmless value |
| Snapshot kinds default to `crash,oom` | Rollouts generate many `killed` stops | A crash caused by an external SIGKILL with no preceding kill event is classified as `crash` |
| Store container name, id and app on every log row | Simple, fast to filter | Roughly 100 bytes per row of repeated text. Normalizing into a lookup table is a known optimization |

## 10. Extension points

- **Alerts**: subscribe to the hub (log rules) and to a new metrics channel (thresholds), evaluate rules, send webhooks.
- **Deploy markers**: record a row when a new image id appears for an app, draw it on charts.
- **Agent mode**: run the same collector on each node and have it push rows to a central CapScope. The `docker`, `collector` and `store` boundaries already separate collection from serving.
- **Prometheus endpoint**: read the latest metric per container from the trackers and expose `/metrics`.
