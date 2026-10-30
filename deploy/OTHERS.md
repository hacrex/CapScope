# CapScope without a PaaS: Portainer, plain Docker, and others

CapScope is one Go binary + a Dockerfile + a compose file. Any platform that can run a Docker image can run it; nothing platform-specific is required.

## Portainer

1. **Stacks** → **Add stack**, paste the contents of [docker-compose.yml](docker-compose.yml).
2. Set `AUTH_PASS` to a long random password before exposing the port.
3. Deploy. Portainer's own container shows up in the sidebar; exclude it with `EXCLUDE=^portainer` if it is noisy.

## Plain Docker Compose

```bash
cd deploy
docker compose up -d --build
# then open http://<host>:8080
```

`AUTH_PASS` defaults to `change-me` in the compose file — change it before exposing the port.

## Plain `docker run`

```bash
docker run -d --name capscope -p 8080:8080 \
  -e AUTH_PASS='a long random password' \
  -v capscope-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --restart unless-stopped \
  capscope
```

## Swarm / `docker stack deploy`

```bash
docker stack deploy -c deploy/docker-compose.yml capscope
```

Swarm caveat: CapScope only sees containers on the node its task is scheduled on (it reads the host socket). Pin it to the node with the most containers via placement constraints if you run several nodes.

## Generic requirements (checklist for "my NAS, my homelab panel, ...")

1. Build the image from the `Dockerfile` at the repo root, or run the binary from any host with a Docker daemon reachable via socket or TCP.
2. Persistent volume on `/data` (SQLite lives there).
3. Mount `/var/run/docker.sock` (or set `DOCKER_HOST=tcp://...` to a socket proxy).
4. Environment `AUTH_PASS` (mandatory), optionally `AUTH_USER`, `LOG_RETENTION`, `MAX_DB_MB`, `EXCLUDE` — full list in the [README](../README.md#configuration).
5. Reverse proxy pointing at port `8080`, HTTPS mandatory (Basic auth credentials travel in clear text on HTTP). Health path: `/healthz`.
6. One instance only. Never run two CapScope containers against the same `/data` volume.

## Not supported

- **Kubernetes**: CapScope talks to the Docker Engine API (containers, logs, stats, events), not the kubelet. Run it as a Docker container on a Docker node instead.
- **TrueNAS SCALE / other k8s-based appliances**: same reason — Docker host only.
