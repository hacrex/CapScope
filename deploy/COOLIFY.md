# Deploying CapScope on Coolify

Coolify builds the repo's `Dockerfile` directly, so no extra definition file is needed.

## 1. Create the resource

1. Project → **New Resource** → **Dockerfile**, and point it at this repository.
2. Coolify builds the `Dockerfile` at the repo root automatically; set "Build context" to `.` if it asks.

## 2. Configure it

**Environment variables**

| Key | Value |
|---|---|
| `AUTH_USER` | `admin`, or your username |
| `AUTH_PASS` | a long random password |
| `MAX_DB_MB` | size cap for live data, for example `2048` |
| `EXCLUDE` | optional; `^coolify-` hides Coolify's own containers from the sidebar |

**Labels in Coolify** (skip if you mount the socket, see below): `com.traefik.*` not needed, Coolify adds routing itself.

**Ports**: set the container port to `8080`. Assign the app a **Domain** and turn on HTTPS. Do not expose it on plain HTTP — Basic auth credentials would travel in clear text.

**Health check**: URL path `/healthz` (auth-free by design).

## 3. Storage

Add a **Persistent Storage** volume:

| Path in container | Label |
|---|---|
| `/data` | `capscope-data` (SQLite lives here) |

Without it, history, crash snapshots and settings are lost on each redeploy.

## 4. Docker access

CapScope needs to reach the Docker Engine API of the host. Two options:

- **Socket proxy (recommended).** Deploy the `tecnativa/docker-socket-proxy` snippet from [deploy/docker-compose.yml](docker-compose.yml) on the same network (or "Docker Compose" resource), with `CONTAINERS=1`, `EVENTS=1`, `PING=1`, `POST=0`. Add to CapScope: `DOCKER_HOST=tcp://<service-name-of-proxy>:2375`.
- **Socket mount.** In the resource's run-time config, bind `/var/run/docker.sock` from the host. This gives the container root-equivalent access to the host — only use it on single-user hosts.

## 5. Notes

- **Live tail (SSE)**: CapScope sends `X-Accel-Buffering: no`, which Nginx proxies respect. Traefik and Caddy stream it as-is. If the live view stalls, check the proxy in front of the instance for response buffering on `/api/logs/stream`.
- **Instance count**: one. Never scale Coolify to more than one replica of CapScope — each would write the same SQLite file.
- The container runs as `root` on purpose (`docker.sock` needs it).
