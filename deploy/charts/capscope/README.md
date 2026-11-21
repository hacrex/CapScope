# CapScope Helm Chart

CapScope: A self-hosted container log and stats viewer with persistent
history, crash snapshots, access logs, and alerting.

## Prerequisites

- Kubernetes 1.20+
- Docker socket mounted on each node where CapScope runs
- Helm 3+

## Installing the Chart

```bash
helm repo add capscope https://hacrex.github.io/CapScope/
helm install capscope capscope/capscope
```

Or install from a local chart directory:

```bash
helm install capscope ./deploy/charts/capscope
```

## Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Image repository | `capscope/capscope` |
| `image.tag` | Image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `service.type` | Service type | `ClusterIP` |
| `service.port` | Service port | `8080` |
| `rbac.enabled` | Enable RBAC | `true` |
| `persistence.enabled` | Enable PVC for SQLite data | `true` |
| `persistence.size` | PVC storage size | `5Gi` |
| `persistence.accessMode` | PVC access mode | `ReadWriteOnce` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `512Mi` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `128Mi` |

### Authentication

Set `AUTH_PASS` to require authentication. You can create a secret manually:

```bash
kubectl create secret generic capscope-auth \
  --from-literal=password=your-password \
  --from-literal=smtp_pass=your-smtp-password
```

Then set `AUTH_PASS` in `values.yaml` to reference it, or pass it via `--set-string env.AUTH_PASS=your-password`.

### Docker Socket

CapScope mounts `/var/run/docker.sock` from the host. Ensure Docker is running on the nodes where CapScope pods are scheduled.

### Alerting Channels

Configure notification channels via environment variables in `values.yaml`:

- `ALERT_WEBHOOK_URL`: Generic webhook
- `ALERT_TELEGRAM_TOKEN` / `ALERT_TELEGRAM_CHAT_ID`: Telegram
- `ALERT_SLACK_URL`: Slack
- `ALERT_DISCORD_URL`: Discord
- `ALERT_NTFY_URL` / `ALERT_NTFY_TOKEN`: ntfy
- `ALERT_PAGERDUTY_KEY`: PagerDuty Events API v2
- `ALERT_OPSGENIE_KEY`: Opsgenie

### SMTP Email Notifications

Set `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM`, `SMTP_TO` to enable email alerts. Set `SMTP_DIGEST_INTERVAL` (e.g. `"1h"`, `"6h"`, `"24h"`) to send digest emails instead of immediate alerts.

## Uninstalling the Chart

```bash
helm uninstall capscope
```
