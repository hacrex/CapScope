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
