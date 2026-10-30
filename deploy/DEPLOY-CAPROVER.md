# CapScope on CapRover

Deploy CapScope (Docker observability) on CapRover using the release tarball.

## What you need

- CapRover ≥ 1.10 with dashboard access
- Outbound internet on the CapRover host during the first build (Go module download)
- 2–4 GB RAM for the build
- A password for Basic auth

## Step 1 — Generate a password

```bash
openssl rand -base64 24
```

Save it. You will set it as `AUTH_PASS` below.

## Step 2 — Create the app

1. CapRover dashboard → **Apps → Create New App**
2. Name: `capscope`
3. Leave **"Has Persistent Data"** unticked (we add directories manually)

## Step 3 — Configure the app

Open `capscope` → **App Configs**:

| Setting | Value |
|---------|-------|
| Container HTTP Port | `8080` |
| Instance count | `1` |
| HTTPS | Enabled + Force HTTPS |
| WebSocket Support | Off (SSE is used) |

**Environment variables:**

| Key | Example | Notes |
|-----|---------|-------|
| `AUTH_USER` | `admin` | Basic auth username |
| `AUTH_PASS` | `<from step 1>` | Required; server refuses to start without it |
| `MAX_DB_MB` | `2048` | SQLite size cap in MB |
| `EXCLUDE` | `^captain-` | Hides CapRover's own containers from logs |

**Persistent Directories** — add both:

| Path in container | Value |
|-------------------|-------|
| `/data` | named volume `capscope-data` |
| `/var/run/docker.sock` | host path `/var/run/docker.sock` |

> Mounting `docker.sock` is root-equivalent. For a read-only alternative, see **Hardening** below.

## Step 4 — Deploy

1. `capscope` app → **Deployment** tab
2. Choose **Upload**
3. Select `CapScope-capscope.tar.gz` (built from this repo)
4. Start deployment

CapRover reads `captain-definition` → `./Dockerfile` and builds a static scratch image. The first build downloads Go modules and may take several minutes.

## Step 5 — Verify

- Open `https://capscope.<your-root-domain>` and log in
- Header chip should show container count > 0 within ~10 s
- `curl -k https://capscope.<domain>/healthz` → `200`
- Crash a throwaway container → **Crashes** tab shows a snapshot with "Last deploy" (V9)

## Hardening: read-only socket proxy

Mounting `docker.sock` directly gives the container host root access. Safer setup:

1. Create app `docker-proxy` from image `tecnativa/docker-socket-proxy`
2. Env: `CONTAINERS=1`, `EVENTS=1`, `PING=1`, `POST=0`
3. Mount `/var/run/docker.sock` read-only; no public port; not exposed as web app
4. On `capscope`: remove the socket mount, set `DOCKER_HOST=tcp://srv-captain--docker-proxy:2375`
5. Redeploy `capscope`

## Capacity planning

About 400 DB bytes per stored log line (130 byte JSON line):

| Lines/sec | Per day | Per 7 days |
|-----------|---------|------------|
| 1 | ~35 MB | ~240 MB |
| 10 | ~350 MB | ~2.4 GB |
| 100 | ~3.5 GB | ~24 GB |

Set `MAX_DB_MB` accordingly. Oldest logs are dropped first to stay under the cap.

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `AUTH_PASS is required` at start | Set `AUTH_PASS`, or `ALLOW_NO_AUTH=true` for private tests |
| `cannot reach Docker` at start | Socket not mounted, or `DOCKER_HOST` proxy URL wrong |
| Sidebar empty | `EXCLUDE` too greedy, or no containers on this node |
| Live tail stalls | Reverse proxy buffering SSE — CapScope sends `X-Accel-Buffering: no` |
| Data gone after redeploy | `/data` not on a persistent volume |
| Corrupt DB / locked errors | More than one replica — scale to 1 |

## Notes

- Keep replica count at **1**; two instances writing one SQLite file corrupt it.
- HTTPS is mandatory in production: Basic auth credentials travel in clear text on HTTP.
- CapScope only sees containers on the node its task lands on (single-node CapRover is fine).
