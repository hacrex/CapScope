# Deploying CapScope on Dokku

Dokku builds the repo's `Dockerfile` on `git push`, no buildpack needed. The `CHECKS` file at the repo root makes Dokku wait for `/healthz` before routing traffic.

## 1. Create the app

```bash
dokku apps:create capscope
```

Persistent storage (the SQLite database survives redeploys):

```bash
dokku storage:ensure-directory capscope --chown-on-add
dokku storage:mount capscope /var/lib/dokku/data/storage/capscope:/data
```

Docker socket access:

```bash
dokku storage:mount capscope /var/run/docker.sock:/var/run/docker.sock
```

The `storage:mount` commands apply on the next deploy.

## 2. Configure it

```bash
dokku config:set capscope AUTH_USER=admin AUTH_PASS='a long random password'
dokku config:set capscope MAX_DB_MB=2048           # optional size cap
dokku config:set capscope EXCLUDE='^nginx|traefik'  # optional, hide Dokku's own proxy containers
```

Worth excluding: the per-app `nginx` sidecars Dokku adds and the `traefik`/`caddy` ingress containers, they are noisy.

## 3. Deploy

```bash
git remote add dokku dokku@<your-dokku-host>:capscope
git push dokku main
```

The first build downloads Go modules, so the Dokku host needs outbound internet during the build.

**HTTPS**: assign a domain and run the letsencrypt plugin:

```bash
dokku domains:set capscope capscope.example.com
dokku letsencrypt:set --global email you@example.com
dokku letsencrypt capscope
```

Do not expose CapScope on plain HTTP — Basic auth credentials would travel in clear text.

**Scaling**: keep it at 1 process (the Dockerfile build defaults to a single `web` process; do not run `dokku ps:scale capscope web=2` or higher) — instances would write the same SQLite file.

## 4. Socket proxy (recommended)

Mounting `docker.sock` is root equivalent on the host. To limit what CapScope can do:

1. Run the tecnativa/docker-socket-proxy container with `CONTAINERS=1`, `EVENTS=1`, `PING=1`, `POST=0`, no public port, on the same Docker network as CapScope (see [docker-compose.yml](docker-compose.yml) for the equivalent snippet).
2. Remove the socket mount and instead: `dokku config:set capscope DOCKER_HOST=tcp://<proxy-container-name>:2375`.
3. Re-deploy.

## 5. Notes

- **Ports.** The `Dockerfile` exposes `8080`, and Dokku maps the first exposed port to the app's proxy automatically, so no `ports:set` is needed. If you skip the web proxy, map it explicitly: `dokku ports:set capscope http:80:8080`.
- **Live tail (SSE)**: CapScope sends `X-Accel-Buffering: no`, and Dokku's default Nginx proxy respects it. If the live view stalls behind another reverse proxy, disable response buffering for `/api/logs/stream`.
- The container runs as `root` on purpose (`docker.sock` needs it).
