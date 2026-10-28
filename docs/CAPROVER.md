# Deploying CapScope on CapRover

## 1. Create the app

1. In the CapRover dashboard: Apps, create a new app named `capscope`. Do not tick "Has Persistent Data" if you plan to set directories manually below.
2. Open the app, **App Configs**.

## 2. Configure it

**Environment variables**

| Key | Value |
|---|---|
| `AUTH_USER` | your username |
| `AUTH_PASS` | a long random password |
| `MAX_DB_MB` | size cap for live data, for example `2048` |
| `EXCLUDE` | optional, for example `^captain-` to hide CapRover's own services |

**Container HTTP Port**: `8080`.

**Persistent Directories** (needed twice):

| Path in container | Label or host path |
|---|---|
| `/data` | a named volume such as `capscope-data` (SQLite lives here) |
| `/var/run/docker.sock` | host path `/var/run/docker.sock` |

**Instance count**: `1`. Do not scale CapScope out, each instance would write the same data.

**HTTPS**: enable HTTPS and "Force HTTPS". Do not expose this app on plain HTTP, Basic auth credentials would travel in clear text.

**WebSocket Support**: not needed. The live view uses Server-Sent Events. CapScope sends `X-Accel-Buffering: no` so CapRover's nginx does not buffer the stream.

## 3. Deploy the code

From the project folder, using the CapRover CLI:

```bash
npm i -g caprover
caprover deploy
```

Pick your server and the `capscope` app. CapRover reads `captain-definition`, which points at the `Dockerfile`. Alternatively zip the project (without `.git`) and upload it in the app's **Deployment** tab.

The first build downloads Go modules, so the CapRover host needs outbound internet during the build.

## 4. Verify

- Open `https://capscope.<your-root-domain>` and log in.
- The header chip should show a container count above zero.
- Trigger a test crash on a throwaway app, for example run a container that exits with code 1, and check the Crashes tab.

## Hardening: socket proxy (recommended)

Mounting `docker.sock` into a container is root equivalent on the host. To limit what CapScope can do, run a socket proxy that exposes only read endpoints and point CapScope at it.

1. Create an app `docker-proxy` from the image `tecnativa/docker-socket-proxy`, bind-mount `/var/run/docker.sock` read only, and set environment `CONTAINERS=1`, `EVENTS=1`, `PING=1`, `POST=0`. Do not map any public port. Keep it internal to the CapRover network.
2. On the `capscope` app, remove the `/var/run/docker.sock` mount and set `DOCKER_HOST=tcp://srv-captain--docker-proxy:2375`.
3. In the proxy app, keep **Expose as web app** off.

`CONTAINERS=1` is what allows `GET /containers/*` (inspect, logs, stats). Verify on your CapRover version that the proxy lets CapScope start, because proxy defaults vary between image versions.

## Capacity planning

Measured on a sandbox: about **400 bytes of database per stored log line** for 130 byte JSON lines (three indexes included). Estimate:

```
bytes per day = lines per second x 86400 x 400
```

| Lines per second (all apps) | Per day | Per 7 days |
|---|---|---|
| 1 | about 35 MB | about 240 MB |
| 10 | about 350 MB | about 2.4 GB |
| 100 | about 3.5 GB | about 24 GB |

`MAX_DB_MB` wins over `LOG_RETENTION`: if your volume is high, the oldest logs are dropped early to stay under the cap. Raise the cap, lower the retention, or use `EXCLUDE` for chatty containers such as `captain-nginx`.

Metrics are cheap: one row per container every 10 seconds is about 8,600 rows per container per day.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Exits at start: `AUTH_PASS is required` | Set `AUTH_PASS`, or `ALLOW_NO_AUTH=true` for a private test |
| Exits at start: `cannot reach Docker` | Socket not mounted, or proxy URL wrong |
| App list empty | `INCLUDE` or `EXCLUDE` regex filtering everything, or no containers on this node |
| Live view stalls behind a proxy | A proxy in front of CapRover is buffering SSE. Disable buffering for `/api/logs/stream` |
| Container shows no logs | Its logging driver does not support reading (`docker logs` fails for it) |
