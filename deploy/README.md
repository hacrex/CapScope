# Deploying CapScope on other platforms

CapScope ships everything a PaaS needs: a plain `Dockerfile` (builds on any build system), `EXPOSE 8080`, a `VOLUME /data`, a built-in `HEALTHCHECK`, and an unauthenticated `GET /healthz` endpoint for HTTP-based health checks.

| Platform | Guide | What it needs on top |
|---|---|---|
| CapRover | [docs/CAPROVER.md](../docs/CAPROVER.md) | nothing, `captain-definition` is in the repo |
| Coolify | [COOLIFY.md](COOLIFY.md) | Dockerfile resource + socket proxy volume |
| Dokploy | [DOKPLOY.md](DOKPLOY.md) | compose service, Swarm single-node caveat |
| Dokku | [DOKKU.md](DOKKU.md) | `git push` + `storage:mount` overrides |
| Portainer / plain Docker / others | [OTHERS.md](OTHERS.md) | paste the compose stack |
| Podman | [OTHERS.md](OTHERS.md#podman) | same image; mount the podman.sock API socket instead of docker.sock |

Common to all platforms:

- Set `AUTH_PASS` (a long random password). Without it the server refuses to start.
- Persist `/data` on a volume — the SQLite database lives there.
- Give CapScope Docker API access: mount `/var/run/docker.sock`, or (safer, no socket in CapScope) point `DOCKER_HOST` at a read-only socket proxy from [deploy/docker-compose.yml](docker-compose.yml).
- Never scale to more than one replica; each instance would write the same SQLite file.
- Kubernetes is not supported: CapScope talks to the Docker Engine API, not the kubelet.
