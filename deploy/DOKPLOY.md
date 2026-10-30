# Deploying CapScope on Dokploy

Dokploy deploys from either a Dockerfile or a Docker Compose file. The compose path is recommended here because it also wires the Docker socket in one go.

## 1. Create the service

1. Project → **Create Service** → **Docker Compose**. Point it at this repository and set the compose path to `deploy/docker-compose.yml`.
   - The compose file builds from the repo root (`build: ..`), so the first deploy compiles the Go binary; the Dokploy server needs outbound internet during the build.
2. Alternatively use a **Dockerfile** service (quick, no compose) and add the volume mounts listed below in the service's **Advanced / Volumes** section.

## 2. Environment variables

Set in the service's **Environment** tab:

| Key | Value |
|---|---|
| `AUTH_USER` | `admin`, or your username |
| `AUTH_PASS` | a long random password |
| `MAX_DB_MB` | size cap for live data, for example `2048` |
| `EXCLUDE` | optional; hide Dokploy's own containers, for example `^dkp-` |

The compose file defaults `AUTH_PASS` to `change-me` — **change it** before exposing the app.

## 3. Domain and TLS

1. In the service, add a **Domain** (Dokploy routes it through its Traefik), bind to container port `8080`.
2. Enable HTTPS / Let's Encrypt. Do not expose the app on plain HTTP — Basic auth credentials would travel in clear text.
3. SSE works behind Traefik as-is; CapScope also sends `X-Accel-Buffering: no` for Nginx-style proxies.

## 4. Health check

In the service settings set the HTTP health check path to `/healthz` (auth-free by design). The image also carries a Docker `HEALTHCHECK` with the same probe.

## 5. Notes and caveats

- **Single node only.** Dokploy runs on Docker Swarm. CapScope reads the host's Docker socket, so in a multi-node Swarm it only sees containers on the node its task is scheduled on. Pin it to the manager/swarm node (or wherever your workloads run) if you have more than one node.
- **One replica.** Never scale the capscope service beyond 1 — instances would write the same SQLite file.
- **Storage**: the compose file already declares a named volume `capscope-data` mounted at `/data`. Keep it; the SQLite database lives there.
- The container runs as `root` on purpose (`docker.sock` needs it). Use the socket-proxy variant from [docker-compose.yml](docker-compose.yml) to drop the socket mount.
