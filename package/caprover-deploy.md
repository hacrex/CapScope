# CapScope CapRover Deployment

## What changed (fast startup like Dozzle)

CapScope now starts the HTTP server **immediately** without waiting for Docker.
The Docker connection happens in the background (retries every 3 seconds), so the
UI is responsive within seconds — just like Dozzle.

The connection chip in the header shows `connecting` until Docker is reachable,
then switches to `live`. Log tailing and crash snapshots start automatically once
Docker is available.

## Prerequisites on CapRover

CapScope needs **read-only access to the Docker socket** to read container logs.
Without it, the UI loads but shows no containers.

## Deploy on CapRover Dashboard

1. **One-Click App** → Deploy new app
2. **App name:** `capscope`
3. **Image:** Build from this package or push your own:
   ```bash
   # On your CapRover host (or any Linux machine):
   tar -xzf capscope-caprover-linux-amd64.tar.gz
   cd package
   docker build -t capscope:latest .
   ```
   Then in CapRover, use image `capscope:latest` (local registry) or push to
   DockerHub/GHCR and use that image reference.

4. **Environment variables:**
   | Variable | Value | Required |
   |----------|-------|----------|
   | `LISTEN` | `0.0.0.0:8080` | Yes |
   | `DB_PATH` | `/data/capscope.db` | Yes |
   | `DOCKER_HOST` | `unix:///var/run/docker.sock` | Yes |
   | `AUTH_PASS` | your-secure-password | Recommended |
   | `ALLOW_NO_AUTH` | `false` | Recommended |
   | `LOG_LEVEL` | `info` | Optional |

5. **Persistent volume:** `/data` → create volume `capscope-data`

6. **Socket mount:** `/var/run/docker.sock` → `/var/run/docker.sock` (read-only)

7. **Port mapping:** 8080 → 8080 (CapRover routes 80/443 automatically)

## Test locally with Docker Compose

```bash
tar -xzf capscope-caprover-linux-amd64.tar.gz
cd package
docker compose up -d
# Open http://localhost:8080
```

## What's in the tarball

| File | Purpose |
|------|---------|
| `capscope-linux-amd64` | Compiled binary (executable, CGO-free) |
| `Dockerfile` | Debian-based container image |
| `docker-compose.yml` | Local test / standalone deploy |
| `caprover-deploy.md` | This file |
| `entrypoint.sh` | Container entrypoint |

## Troubleshooting

**UI shows "connecting" forever:**
- Docker socket not mounted: check that `/var/run/docker.sock` is mounted read-only
- Wrong Docker host: verify `DOCKER_HOST=unix:///var/run/docker.sock`

**401 Unauthorized on log stream (realtime logs not showing):**
- CapScope requires login when `AUTH_PASS` is set. Log in via the login overlay first.
- After logging in, the session cookie enables EventSource live-tail streaming.
- If you see 401 even after logging in, the cookie may not be forwarded by CapRover's reverse proxy. Check browser dev tools → Application → Cookies for `capscope_token`.
- CapScope now sets `Secure` based on `X-Forwarded-Proto` for TLS-terminating proxies like CapRover.

**Container exits immediately:**
- Check `docker logs capscope` for startup errors
- Common cause: missing `/data` volume or unwritable DB path

**Dashboard shows "unexpected data":**
- Usually transient; dashboard auto-refreshes every 15s.
