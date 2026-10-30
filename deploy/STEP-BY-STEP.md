# CapScope — Step-by-Step Deployment Guide (all platforms)

One guide for every supported platform: **CapRover, Coolify, Dokploy, Dokku, and Podman**
(plus plain Docker / Portainer). Detailed per-platform docs live next to this file
([CAPROVER](../../docs/CAPROVER.md) · [COOLIFY](COOLIFY.md) · [DOKPLOY](DOKPLOY.md) ·
[DOKKU](DOKKU.md) · [OTHERS](OTHERS.md)); this is the ordered checklist version.

## What every platform needs (read once)

| Requirement | Why |
|---|---|
| `AUTH_PASS` set to a long random password | The server **refuses to start** without it |
| Persistent volume at `/data` | SQLite DB (`capscope.db`), alert state, snapshots live there |
| Docker Engine API access | Mount `/var/run/docker.sock`, or (safer) point `DOCKER_HOST=tcp://…` at a read-only socket proxy |
| Exactly **one** replica | Two instances writing the same SQLite file corrupts data |
| HTTPS enforced | Basic-auth credentials travel in clear text on plain HTTP |
| Container port `8080` | `LISTEN=:8080` is baked into the image; health path `/healthz` (auth-free) |

Generate a password first (used everywhere below):

```bash
openssl rand -base64 24
```

The image builds from the repo's multi-stage `Dockerfile` (Go 1.24 → static scratch binary),
so any platform that can build/pull a Docker image works. First build downloads Go modules —
the host needs outbound internet during the build.

---

## 1. CapRover

**Prereq:** CapRover ≥ 1.10, CLI optional.

1. Dashboard → **Apps → Create New App**, name `capscope`.
2. **App Configs** tab:
   - *Environment variables*: `AUTH_USER=admin`, `AUTH_PASS=<generated>`, optionally
     `MAX_DB_MB=2048`, `EXCLUDE=^captain-`.
   - *Container HTTP Port*: `8080`.
   - *Persistent Directories* (add both):
     | Path in container | Host/volume |
     |---|---|
     | `/data` | named volume `capscope-data` |
     | `/var/run/docker.sock` | host path `/var/run/docker.sock` |
   - *Instance count*: `1`.
   - Enable **HTTPS** + **Force HTTPS**.
3. Deploy either way:
   ```bash
   npm i -g caprover && caprover deploy   # reads captain-definition -> Dockerfile
   ```
   or zip the repo (without `.git`) and upload in the **Deployment** tab.
4. Verify: open `https://capscope.<root-domain>`, log in, header chip shows >0 containers.
   Trigger a throwaway crashing container and check the **Crashes** tab.

**Harden (recommended):** create an app `docker-proxy` from `tecnativa/docker-socket-proxy`
(env `CONTAINERS=1 EVENTS=1 PING=1 POST=0`, no public port), remove CapScope's socket mount,
set `DOCKER_HOST=tcp://srv-captain--docker-proxy:2375`. WebSocket support not needed (SSE).

## 2. Coolify

1. Project → **New Resource → Dockerfile**, point at this repo, build context `.`.
2. Set the container port to `8080`; assign a **Domain**; enable HTTPS.
3. Environment: `AUTH_USER`, `AUTH_PASS`, optionally `MAX_DB_MB`, `EXCLUDE=^coolify-`.
4. **Persistent Storage**: add volume → `/data` (label `capscope-data`). Without it history
   is wiped on each redeploy.
5. Docker access, pick one:
   - *Socket proxy (recommended)*: deploy `tecnativa/docker-socket-proxy` as a second
     resource/compose service (`CONTAINERS=1 EVENTS=1 PING=1 POST=0`), then set
     `DOCKER_HOST=tcp://<proxy-service>:2375` on CapScope.
   - *Socket mount*: bind `/var/run/docker.sock` in the resource's run-time volumes
     (root-equivalent — single-user hosts only).
6. Health check path: `/healthz`. Keep replicas at 1. SSE streams fine behind Traefik/Caddy;
   if live tail stalls behind Nginx, disable buffering for `/api/logs/stream`
   (CapScope already sends `X-Accel-Buffering: no`).

## 3. Dokploy

1. Project → **Create Service → Docker Compose**, repo URL as above, compose path
   `deploy/docker-compose.yml` (it builds from repo root and wires the socket + volume).
   *Or* use a **Dockerfile** service and add the mounts manually in Advanced → Volumes:
   `capscope-data:/data` and `/var/run/docker.sock:/var/run/docker.sock`.
2. Environment tab: `AUTH_USER`, `AUTH_PASS` (**change the compose default `change-me`**),
   optionally `EXCLUDE=^dkp-`, `MAX_DB_MB`.
3. Add a **Domain** bound to container port `8080`; enable Let's Encrypt/HTTPS.
4. Health check path: `/healthz` (image also carries a Docker HEALTHCHECK).
5. Caveats: Dokploy runs on Swarm — CapScope only sees containers on the node its task lands
   on; pin placement to your workload node. Never scale beyond 1 replica. For the safer
   socket-proxy variant, uncomment the proxy block in `deploy/docker-compose.yml` and set
   `DOCKER_HOST=tcp://socket-proxy:2375`.

## 4. Dokku

Run these on the Dokku host (or via ssh):

```bash
dokku apps:create capscope
dokku storage:ensure-directory capscope --chown-on-add
dokku storage:mount capscope /var/lib/dokku/data/storage/capscope:/data
dokku storage:mount capscope /var/run/docker.sock:/var/run/docker.sock   # or skip, see proxy
dokku config:set capscope AUTH_USER=admin AUTH_PASS='<generated>'
dokku config:set capscope MAX_DB_MB=2048 EXCLUDE='^nginx|traefik'        # optional
git remote add dokku dokku@<dokku-host>:capscope
git push dokku main                       # CHECKS file gates routing on /healthz
dokku domains:set capscope capscope.example.com
dokku letsencrypt:set --global email you@example.com
dokku letsencrypt capscope
```

Notes: Dokku auto-maps the first exposed port (8080); no `ports:set` needed unless you skip
the web proxy (`dokku ports:set capscope http:80:8080`). Keep `web=1` — do **not**
`ps:scale web=2`. Socket-proxy alternative: run tecnativa proxy on the app network, drop the
socket mount, `dokku config:set capscope DOCKER_HOST=tcp://<proxy-name>:2375`, redeploy.

## 5. Podman

Podman runs the same image; two differences from Docker:

```bash
# Build (podman build == docker build; use the alias or explicit podman)
git clone <repo-url> capscope && cd capscope
podman build -t capscope .

# Run against the system (rootful) Podman API socket
podman run -d --name capscope -p 8080:8080 \
  -e AUTH_USER=admin -e AUTH_PASS='<generated>' \
  -v capscope-data:/data \
  -v /run/podman/podman.sock:/var/run/docker.sock \
  --restart unless-stopped \
  localhost/capscope
```

1. **Socket path.** Rootful: `/run/podman/podman.sock`. Rootless:
   `/run/user/$UID/podman/podman.sock` (ensure `podman.socket` / `podman system service`
   is active). Either mount it *at* `/var/run/docker.sock` (as above — no config change) or
   skip the mount and set `-e DOCKER_HOST=unix:///run/podman/podman.sock`.
2. **Rootless caveat.** Under rootless, CapScope sees only that user's containers; if the
   sidebar populates but live tail/snapshots stay silent, switch to the rootful socket.
3. Compose path: `podman-compose -f deploy/docker-compose.yml up -d --build` works, but
   edit the socket line to the podman path first. The scratch image itself runs fine
   (static binary, no daemon deps).
4. Put a TLS reverse proxy (nginx/caddy/podman-based) in front of `:8080`; health `/healthz`.

## 6. Plain Docker / Portainer (bonus)

```bash
cd deploy && docker compose up -d --build    # change AUTH_PASS in the compose file first
```
Portainer: Stacks → paste `deploy/docker-compose.yml`. Exclude noise with `EXCLUDE=^portainer`.

---

## Post-deploy verification checklist (any platform)

1. `curl -k https://<host>/healthz` → `200` (no auth required by design).
2. Log in with Basic auth; sidebar lists containers within ~10 s of first deploy.
3. Open a container → **Live tail** streams (SSE; if silent, check proxy buffering).
4. Search a known log string (FTS5) → results with timestamps.
5. Crash a throwaway container → **Crashes** tab shows a snapshot; export markdown includes
   the “Last deploy” line (V9 fix).
6. Alerts tab → **Send test alert** exercises every configured webhook channel.
7. Restart the CapScope container → logs/metrics/snapshots survive (proves `/data` persisted).

## Troubleshooting quick table

| Symptom | Cause / fix |
|---|---|
| Container exits instantly with “AUTH_PASS …” | Password not set — set it in the platform env UI |
| Sidebar empty | Socket not mounted / wrong path (Podman!) / `EXCLUDE` too greedy |
| Live tail stalls | Reverse proxy buffering SSE — honor `X-Accel-Buffering: no` or disable buffering on `/api/logs/stream` |
| Data gone after redeploy | `/data` not on a persistent volume |
| Corrupt DB / locked errors | More than one replica running against the same volume — scale to 1 |
| Only some containers visible (Swarm/Podman rootless) | Per-node/per-user API scope — pin placement or use rootful socket |

Not supported: Kubernetes (Docker Engine API only), multi-replica HA.
