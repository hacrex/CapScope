# HTTP API

All endpoints except `/healthz` require Basic auth when `AUTH_PASS` is set. Responses are JSON unless noted. Timestamps named `ts` are Unix milliseconds.

## Health

`GET /healthz` returns `ok`.

## Containers and apps

`GET /api/containers` returns running containers being followed.

```json
{"containers":[{"id":"...","name":"srv-captain--shop.1.x","app":"shop","replica":"1","image":"shop:1.4",
  "started":1793158700000,"cpu":12.5,"mem":104857600,"mem_limit":536870912,"sample_ts":1793158700000}]}
```

`GET /api/apps` returns apps known from history with a one hour error and warning count.

```json
{"apps":[{"app":"shop","containers":1,"last_seen":1793158700437,"errors_1h":8,"warns_1h":14}]}
```

## Logs

`GET /api/logs` returns rows oldest first.

| Param | Meaning |
|---|---|
| `app` | App name |
| `container` | Container name or id prefix |
| `q` | Search text. Case-insensitive. Uses an FTS5 token/prefix index as a fast path (each word matches as a prefix, so `dat` finds `database`); when the index finds nothing, falls back to a `LIKE` substring match (so mid-word searches still work) |
| `regex` | `1` treats `q` as a case-insensitive regular expression. Invalid patterns return 400 |
| `level` | Comma list of `error`, `warn`, `info`, `debug` |
| `stream` | `stdout` or `stderr` |
| `since` | Relative (`15m`, `2h`, `1d`) or epoch milliseconds |
| `until` | Epoch milliseconds |
| `after_id` | Only rows with a larger id, ascending (use for catching up) |
| `before_id` | Only rows with a smaller id (use for paging back) |
| `limit` | Default 500, max 5000. Without `after_id` you get the newest `limit` rows |

Row shape:

```json
{"id":260,"ts":1793158659437,"container_id":"...","container":"srv-captain--shop.1.x","app":"shop","stream":"o","level":"error","line":"ERROR ..."}
```

`stream` is `o` for stdout and `e` for stderr. `level` may be empty.

`GET /api/logs/stream` is a Server-Sent Events stream of new rows matching the same filters (except paging and time params). Each `data:` line is one row. A `dropped` event reports how many rows this client lost to back-pressure. Comment lines (`: ping`) keep the connection alive.

`GET /api/logs/export[&app=...&q=...&level=...&since=...]` streams the matching rows as CSV (`capscope-logs.csv`), honoring the same filters as `/api/logs`. It paginates internally, so exports reach up to 200,000 lines regardless of the normal 5000-row cap.

## Access logs (GoAccess-style dashboard)

Parsed web-server access-log rows (nginx `combined`/`common`, the default log formats of CapRover's `captain-nginx` and most proxies) are collected automatically:

- containers whose app name looks like a reverse proxy are parsed from the first line;
- any other container is probed and switched on once most of its recent stdout parses as access logs;
- containers matched by `EXCLUDE` are followed *only* for this purpose (accessOnly): they produce no rows in `logs` and do not appear in the app sidebar;
- `ACCESS_LOG` selects `auto` (default), `all` (parse everything) or `off`;
- client IPs are stored anonymized (last octet zeroed for IPv4, IPv6 cut to /48); set `ANON_IPS=false` to store original IPs;
- retention follows `LOG_RETENTION`.

`GET /api/access?range=1h[&app=...][&top=20]` returns aggregated dashboard data:

```json
{"since_ms":..., "until_ms":0, "bucket_ms":12000,
 "totals":{"requests":900,"client_err":41,"server_err":9,"unique_ips":3,"bytes":88401},
 "codes":[{"code":200,"n":600}],
 "paths":[{"key":"/api/items","hits":212,"bytes":9901,"errors":3}],
 "ips":[{"key":"203.0.113.0","hits":400,"bytes":41000,"errors":2}],
 "refs":[{"key":"https://google.com/","hits":12,"errors":0}],
 "timeline":[{"ts":1793157492000,"hits":30,"errors":1,"bytes":2955}]}
```

`refs` excludes empty and `-` referrers.

`GET /api/access/export?range=1h[&app=...][&limit=]` streams the raw parsed rows as CSV (`capscope-access.csv`: ts, ts_ms, container, app, ip, method, path, status, bytes, referer, user_agent). Default range 24 h, 100,000 rows max.

## SQL queries

`GET /api/sql?query=<sql>[&limit=5000]` runs one ad-hoc read statement against the stored tables.

| Param | Meaning |
|---|---|
| `query` | A single `SELECT` (or `WITH … SELECT`) statement |
| `limit` | Default 1000, max 5000. `truncated` is `true` when more rows matched |

```json
{"columns":["level","n"],"rows":[["error",3],["info",41]],"truncated":false,"ms":1}
```

Rules, enforced in this order:

1. The statement must start with `SELECT` or `WITH`, contain no stacked statements (`;`), and carry no write/DDL keywords outside string literals and comments (`WITH … DELETE` is rejected). Violations return 400.
2. The read connection pool opens with `_query_only=1`, so any write that slipped through would still fail at SQLite level.
3. A 5-second deadline and the row cap keep heavy scans from pinning the pool.

Tables available: `logs`, `metrics`, `containers`, `snapshots` (and `logs_fts` for `MATCH` joins). Column notes: `logs.ts` is **unix nanoseconds**; `metrics.ts` and `snapshots.ts` are **unix milliseconds**; `snapshots.die_ts` is the death time in unix milliseconds (0 for rows saved before v0.5); `logs.stream` is `o`/`e`.

```sql
SELECT app, level, count(*) AS n FROM logs
WHERE ts > (unixepoch('now') - 3600) * 1000000000
GROUP BY app, level HAVING n > 5 ORDER BY n DESC
```

The UI exposes this as the `SQL` option in the search-mode select.

## Metrics

`GET /api/metrics?app=shop&range=1h[&container=<id prefix>]` returns bucketed points (about 300 per range, minimum bucket 5 s). For an app, CPU and memory are summed across replicas. `net_rx` and `net_tx` are cumulative counters.

```json
{"bucket_ms":12000,"points":[{"ts":1793157492000,"cpu":10,"mem":104857600,"mem_limit":536870912,"net_rx":0,"net_tx":0,"blk_r":0,"blk_w":0,"pids":0}]}
```

## Crash snapshots

`GET /api/snapshots?app=shop&limit=100` returns light rows (no logs or metrics), newest first.

`GET /api/snapshots/{id}` returns the full snapshot: metadata, `summary`, `logs`, `metrics` and a trimmed `inspect` object.

`GET /api/snapshots/{id}/export` returns a Markdown crash report suitable for pasting into an incident channel.

Snapshot `kind` is one of `oom`, `crash`, `killed`, `clean_exit`.

Snapshot fields of note: `die_ts` is the death time taken from the Docker event (a snapshot rebuilt after CapScope was down keeps that time, not the time it was assembled); `restarts_window` is how many times that container had died inside the restart-loop window at that moment (0 when the detection is disabled), and the UI marks rows at or above the threshold with a `↻` badge. Deaths that happened while CapScope was down are replayed from the persisted event cursor on startup and snapshotted like live ones, as long as the container still exists and the gap is under 24 hours.

## Alerts

Alerting evaluates the rules in `ALERTS_FILE` every `ALERTS_INTERVAL` (default `1m`) and records every firing; the most recent firing per rule and app is also the silence window that prevents re-notifying on every tick. A `repeat` on the rule (default `30m`) sets that silence.

`GET /api/alerts?app=shop&limit=100` returns the firing history, newest first. Each row is `{id, ts, rule, app, kind, message}`; `ts` is unix milliseconds and `kind` is the rule kind (`deadman`, `restart_loop`, `metric`, `log_pattern`).

`GET /api/alerts/rules` returns the armed configuration without secrets: `{rules: [{name, kind, apps, for, count, window, metric, op, value, pattern, hits, repeat}], channels: ["webhook", ...], enabled}`. `enabled` is `false` when no rules file exists.

`POST /api/alerts/test` sends a test message through every configured channel and returns `{results: {webhook: "sent" | "failed: ...", ...}}`. Use it to verify a webhook before an incident. It returns `400` when no channels are configured or alerting is off.

## Deploys

Deploy markers (V9) let you correlate a crash or a metric spike with the release that preceded it. Record them from CI (`POST` on release) or with the **Record deploy** button in the Metrics tab.

`POST /api/deploys` with `{app, version, note?, ts?}` records one marker. `app` is required; `ts` is optional unix milliseconds and defaults to now (a far-future value is clamped to now). Returns `{id, ts, app, version, note}`.

`GET /api/deploys?app=shop&since=1700000000000&until=&limit=200` returns markers newest first. `app`, `since` and `until` (unix ms) are optional; an empty `app` returns every app's markers.

Markers appear as dashed vertical lines with a version label on the CPU and memory charts, and as inline rows in the log timeline. `GET /api/snapshots/{id}` attaches the newest deploy before the crash as a `deploy` object, and the Markdown export includes it as a "Last deploy" line, so the incident handoff shows whether a release preceded the failure.
