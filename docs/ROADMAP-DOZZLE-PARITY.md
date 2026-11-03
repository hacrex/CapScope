# Dozzle-parity execution plan

> Status update (2026-11-03): **Phases 0, 1, 2, 3.1, 3.2–3.3, 4.1–4.5, 6.4 are complete.**
> Cruise control is now `make test` (UI syntax gate + vet + `-race` suite) or
> the new GitHub Actions workflow. All Dozzle-parity phases are done; Phase 7
> (real-host verification) remains externally gated.

Goal-close-the-gap roadmap for the items in `GAP-ANALYSIS.md`. Ordered by
dependency: things that unlock later phases come first, externals gated by a
real Docker host are batched, differentiators are kept last-but-unskippable.
Every phase ends with: full `go test ./...` green, `node --check` on the
embedded UI script, docs updated (README, `docs/API.md`, `CHANGELOG.md`), and
a gap-analysis status flip.

Legend: **S** = session-sized step, 🔒 = needs a real Docker/CapRover host.

## Phase 0 — Baseline & safety (do first, ~half a day)

1. **S** Add `scripts/check-ui.js` (extract `<script>`, run `node --check`) and
   wire it into `make test` so UI edits can never ship a syntax regression.
2. **S** Add `.github/workflows/test.yml`: `go vet ./...`,
   `go test -race -count=1 -tags "sqlite_omit_load_extension fts5" ./...`,
   `make docker` build check. Freeze this as the definition of green.
3. **S** Delete the stale tarball from the repo root (`CapScope-capscope.tar.gz`)
   — it is deploy-time generated; also prune `demo/ui` leftovers.

## Phase 1 — Log-engine completeness (Dozzle v11 log features)

1. **S** `internal/logproc` audit vs Dozzle: support OTel `severityText` and
   `severityNumber` (use the number when the text is not a level name), Pino
   numeric levels (`30`→info, `40`→warn, `50`→error). Add table-driven tests.
2. **S** JSON level extraction: when a line parses as JSON, check common level
   keys before heuristics (Dozzle does this in its parser).
3. **S** Download-as-file: add `GET /api/logs/raw?app=&container=` streaming
   the raw `line` column as a `.log` text file (no CSV quoting), UI link next
   to "Download CSV".
4. **S** Timezone display override: `TIMEZONE=UTC|Local|<IANA>` env plus a UI
   chip that can switch display between browser-local and the configured zone;
   `fmtTime` honors it. Default stays browser-local.
5. Named color schemes — **defer** (cosmetic, no parity value until asked).

## Phase 2 — Alerts to parity+ (phase 4 of the gap plan) ✅ DONE (v0.11)

Status: all landed with one documented deviation — metric smoothing is
opt-in via `window:` (the plan's "default 60s" would change the semantics of
existing alerts.json files); the mean-of-window mode covers Dozzle's
smoothing, and `cooldown` shipped as the Dozzle-compatible alias of `repeat`.

1. **S** ✅ Schema: `internal/alerts` gained kind `event` plus
   `cooldown`, `status` and `exclude` fields; existing alerts.json files
   unchanged.
2. **S** ✅ Event engine — collector fans `die`/`oom`/`health_status`
   into the engine (`Engine.FeedEvent`, non-blocking), app names resolved from
   the tracker or the event attributes.
3. **S** ✅ Smoothing: metric rules with `window:` compare the mean of the
   samples in the window; `cooldown` suppresses re-fire per rule+app.
4. **S** ✅ ntfy channel (`ALERT_NTFY_URL`, optional `ALERT_NTFY_TOKEN`).
5. **S** ✅ `${ENV}` placeholders in webhook/Slack/Discord/ntfy/Telegram
   endpoints and tokens (send-time expansion; `AWS_`/`DOCKER_`/
   `KUBERNETES_`/`DOZZLE_` refused).
6. **S** ✅ Update-available: daily jittered registry manifest-digest
   compare (Docker Hub anonymous token dance included), pseudo-event
   `update_available` fed into the engine; `UPDATE_CHECK`/`UPDATE_CHECK_INTERVAL`.
7. **S** ✅ Tests: per-kind engine tests, channel mock tests incl. refusal,
   updater tests against a registry stub (match/miss/challenge/dedup/unreachable).

## Phase 3 — Sessions & identity (finish the auth list)

1. **S** Persistent signed session secret: store a random key in
   `DB_PATH.spawn_secret` (or a sibling file) on first start; sessions store an
   HMAC of the token so a restart does not (need to) kill them. Update the
   session docs + expiry warning copy.
2. **S** OIDC: `AUTH_PROVIDER=oidc` + `OIDC_ISSUER`, `OIDC_CLIENT_ID`,
   `OIDC_CLIENT_SECRET` (Discovery, PKCE, redirect `/api/oidc/callback`).
   Keeps the proxy-provider path untouched; users.yml-equivalent allowlist via
   the existing single-user config.
3. **S** Multi-user (PRD Z4): the single shared credential becomes the first
   user; users live in the same SQLite store (`users` table: name, hash, role).
   Roles: `admin` (all) and `viewer` (read-only always, actions gated if we add
   any). Audit trail: extend `access` rows with a `user` column, record
   downloads/exports in it, render the user in the UI header + audit panel.

## Phase 4 — Multi-node: agent mode (the big one, C10)

Design before code — reuse the hub/collector architecture, not Dozzle's
protobuf verbatim.

1. ✅ **S** Split `cmd/capscope` into subcommands (`serve` default, `agent`,
   `healthcheck`, `version`, `generate-certs`) without breaking current entrypoint.
2. ✅ **S** Agent binary mode: same collector + ring buffers, but streams instead
   of storing; `DOCKER_FILTER` label filtering like Dozzle agents.
3. ✅ **S** Transport: framed gob streaming (log batches, stat samples, event list,
   container list) over mutual TLS with a generated cert pair (`generate-certs`
   subcommand). Pin the hub to trust one pinned cert fingerprint
   (private-certificate semantics = Dozzle's `DOZZLE_CERT_PEM` pair).
4. ✅ **S** Hub ingestion of agents: collectors fan into a store tagged with
   `host_id`; schema migration for logs/metrics/containers/snapshots tables
   (`ALTER TABLE … ADD COLUMN host_id`), queries filter by it. `POST /api/share`
   expiring permalinks, `GET /api/logs/export?fmt=json`, and self-contained HTML
   incident export also landed (Phase 6.1–6.3).
5. **S** UI: host selector chip in the sidebar; per-host crash shelves and
   host-group merge view (`?host=…` permalinks).
6. **S** Agent healthcheck subcommand + hub-side reconnect/backoff mirrors
   the v0.8 SSE resume logic.
7. 🔒 Cross-node validation: run hub + 2 agents on the test rig; soak.
8. **S** Swarm: with agents deployed as a global service, label-sniffing
   already yields per-node app names — add node grouping by
   `com.docker.swarm.node.id` label on the agent side (no Swarm API calls yet).
9. **S** Podman: only docs + a `remoteSocket` fallback path in `docker.New`.
10. **S** Host-level metrics: sample host CPU/mem in the agent/hub (parse
    `/proc/stat` and cgroup v2 usage) and chart it beside container metrics.

## Phase 5 — Container control (DECIDE — non-goal today)

Only if the user decides the read-only stance changes; otherwise these stay
out and the gap analysis marks them "deliberately not done":

1. **S** `ACTIONS=true` opt-in flag: `POST /api/containers/{id}/start|stop|restart`.
2. **S** UI action buttons + a persistent "actions enabled" chip.
3. **S** Shell/exec: a websocket bridge to `docker exec` — largest risk; keep
   it behind the same flag plus an `ALLOW_SHELL` env. Ask before shipping.
4. **S** Healthcheck display: container row state from `/inspect` (already
   fetched) surfaced as a chip, health-check-failure snapshots (PRD X8).

## Phase 6 — Differentiators & odd items (PRD leftovers)

1. **S** Expiring permalinks (V10): `?ttl=…` links that stop resolving after
   the stored expiry.
2. **S** Any-range JSON export: `/api/logs?fmt=json` alongside CSV.
3. **S** Self-contained HTML incident export: single file with the crash
   snapshot + surrounding log rows + metric sparkline as inline data URIs.
4. **S** MCP server over the store (`cmd/capscope mcp` + `--mcp` mode):
   tools `query_logs`, `list_snapshots`, `get_snapshot`, `run_sql`, read-only,
   auth-protected by the same middleware. This is the one Dozzle cannot match
   (its cloud rail is live-only); prioritize even though it is "extra".
5. **S** Access-log app attribution (C9.7) + optional GeoIP (`GEOIP_DB` env).
6. i18n, PWA, analytics — **skip** (documented decisions).

## Phase 7 — Externally-gated verification 🔒 (one session on the test rig)

1. 🔒 Deploy the built image on a real CapRover (native app), verify socket
   mount, storage path, SSE keeps open behind CapRover's nginx.
2. 🔒 Soak: 24–48 h with a noisy app (`yes` "-"er) — watch retention, DB size
   cap, hub drop counters, alert firing, and memory.
3. 🔒 Update the acceptance checklist in `PRD.md` from ⬜ to ✅.

## Suggested sequencing per session

| # | Session scope | Size |
|---|---|---|
| 1 | Phase 0 + Phase 1 (log engine) | small |
| 2 | Phase 2 (alerts batch) | medium |
| 3 | Phase 3.1 (session secret) + Phase 6.1–6.3 | small |
| 4 | ✅ Phase 4.1–4.4 (agent mode + hub ingestion) | medium |
| 5 | ✅ Phase 4.5 (hub UI: host selector, per-host views, ?host= permalinks) | medium |
| 6 | ✅ Phase 3.2–3.3 (OIDC + multi-user) | medium |
| 7 | ✅ Phase 6.4 (MCP server) | medium |
| 8 | 🔒 Phase 7 (real-host verification + soak) | external |
