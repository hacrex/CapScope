# Production Deployment Checklist — CapScope

Use this checklist before deploying CapScope to production. It covers the
PRD §11 acceptance items plus operational hardening.

## 1. Build verification

- [ ] `go build ./...` — clean
- [ ] `go vet ./...` — clean  
- [ ] `go test -race -count=1 ./...` — all green
- [ ] Docker image builds: `docker build -t capscope:prod .` (requires Docker host)
- [ ] Binary starts: `./capscope serve` — listens on `:8080`, `/healthz` returns `ok`

## 2. Authentication & access (required)

- [ ] `AUTH_PASS` set to a strong, unique password (not the default `admin`)
- [ ] `ALLOW_NO_AUTH` NOT set in production
- [ ] If using `AUTH_PROVIDER=proxy`: CapScope port NOT published publicly; only the reverse proxy reaches it
- [ ] If using `AUTH_PROVIDER=oidc`: `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` configured and tested
- [ ] HTTPS terminated at the reverse proxy (CapScope speaks plain HTTP)
- [ ] Session file (`DB_PATH.sessions`) on persistent volume, mode `0600`

## 3. Docker socket hardening (critical)

The Docker socket is root-equivalent on the host. Do NOT mount it directly into CapScope in production without a proxy.

- [ ] Deploy `tecnativa/docker-socket-proxy` as a separate container
- [ ] Proxy exposes only: `CONTAINERS=1`, `EVENTS=1`, `PING=1`, `POST=0`
- [ ] CapScope mounts NO socket; set `DOCKER_HOST=tcp://<proxy>:2375`
- [ ] Proxy not exposed on a public port (internal Docker network only)

See `docs/CAPROVER.md` §4 "Hardening: socket proxy".

## 4. Storage & retention

- [ ] `DB_PATH` on a persistent volume (not ephemeral container storage)
- [ ] `MAX_DB_MB` set appropriately for your log volume (default 1024 MB)
- [ ] `LOG_RETENTION` / `METRICS_RETENTION` / `SNAPSHOT_RETENTION` set per your compliance needs
- [ ] External backup of the SQLite file (`DB_PATH`) scheduled (volume snapshot or file copy)
- [ ] `APP_RETENTION` / `APP_MAX_MB` set for noisy apps if needed

## 5. Networking

- [ ] `LISTEN` bound to the interface you want (default `:8080`)
- [ ] Reverse proxy (nginx/traefik/CapRover) terminates TLS and forwards to CapScope
- [ ] `/api/logs/stream` (SSE) not buffered by the proxy — disable buffering for that path
- [ ] Health check: `GET /healthz` returns `ok` (unauthenticated; put behind proxy if sensitive)

## 6. Alerting (optional but recommended)

- [ ] `ALERTS_FILE` configured with at least deadman + restart_loop rules
- [ ] At least one notification channel configured (webhook / Slack / Discord / Telegram / ntfy / SMTP)
- [ ] `ALERT_WEBHOOK_URL` or other channel URLs use `${ENV}` placeholders for secrets — not committed
- [ ] Test alert delivery: `POST /api/alerts/test` (Admin only)

## 7. Observability of CapScope itself

- [ ] `GET /metrics` (Prometheus) scraped by your monitoring, if you run Prometheus
- [ ] `UPDATE_CHECK=true` (default) so you get notified of new container images
- [ ] Log level `LOG_LEVEL=info` (or `warn` to reduce noise)

## 8. Multi-user / RBAC (optional)

- [ ] If you need per-app access: create users via `POST /api/users` (Admin)
- [ ] Assign viewers to specific apps via `PUT /api/users/{id}/apps`
- [ ] Review audit log: `GET /api/audit` (Admin)

## 9. Post-deploy smoke test

- [ ] Log in, see container count > 0 in header chip
- [ ] Trigger a test crash on a throwaway app (exit code 1), verify Crashes tab shows it
- [ ] Verify live log tail updates in real time
- [ ] Verify `/api/session` returns your user and role

## 10. Ongoing ops

- [ ] Monitor SQLite file size against `MAX_DB_MB`
- [ ] Monitor `capscope_retention_prunes_total` Prometheus metric
- [ ] Rotate / back up the DB file regularly
- [ ] Keep CapScope updated (update check notifies of new images)

---

## External verification (PRD §11 acceptance)

These require a real Docker/CapRover host and cannot be done from a dev workstation:

- [ ] Verify against a real Docker daemon (staging deploy)
- [ ] Build & run Docker image on CapRover
- [ ] One-week soak test with memory/disk measurements
