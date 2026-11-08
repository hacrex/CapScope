# CapScope Team Enhancement Summary

**Date:** 2026-11-08
**Repository:** `hacrex/CapScope`
**Latest source commit:** [`c7a9782`](https://github.com/hacrex/CapScope/commit/c7a9782)

## Executive summary

CapScope now has per-application viewer RBAC, hardened Admin controls, scoped history and live SSE access, webhook alert support, high-volume SSE fan-out coverage, a Dozzle-style selected-container resource header, and a marketing-style login screen. The source branch is clean and synchronized with `origin/main`.

The final smoke test also identified a deployment gap: the currently running local deployment on `127.0.0.1:18080` is healthy but serves an older binary. It does not yet include the latest animated resource meters or PNG-button removal. The latest source must be redeployed before the new UI can be verified on that live instance.

## Recent delivered enhancements

### Admin panel and RBAC

- Added persistent `admin` and `viewer` roles.
- Added the `user_apps` SQLite table and indexed application assignments.
- Added Admin application assignment controls in the Admin panel through **Edit apps**.
- Added `GET /api/users/{id}/apps` and Admin-only `PUT /api/users/{id}/apps` APIs.
- Admins retain global access; viewers can access only explicitly assigned applications.
- Viewer scopes apply to application lists, dashboards, log history, live SSE streams, metrics, access analytics, exports, snapshots, incident pages, deploy markers, and alert history.
- Alert-rule mutation, webhook test, deploy writes, user management, and scope assignment remain Admin-only.
- Active sessions are revoked after role changes, account deletion, or scope changes.
- Fixed an explicit-app-filter bypass where a viewer could request an unauthorized app through a log query. The fix covers both SQLite history matching and live SSE matching.

### Alerts and webhooks

- Added and documented generic JSON webhook delivery through `ALERT_WEBHOOK_URL`.
- Existing notification channels include Telegram, Slack, Discord, and ntfy.
- Added error-spike examples using `log_pattern` rules with hit counts and time windows.
- Added Admin-only alert rule saves, tests, and webhook test delivery.
- Viewer alert history is filtered to assigned applications.
- Channel URLs and tokens support `${ENV_VAR}` expansion at send time.

### Live logs and performance

- Preserved gapless SSE resume behavior with `Last-Event-ID` and history catch-up.
- Added 500-subscriber hub fan-out coverage at thousands of rows per second.
- Non-race throughput run delivered approximately 6,100 rows/sec to 500 subscribers, producing 9.5 million fan-out deliveries with zero dropped rows and approximately 284 ms maximum observed delivery latency.
- Browser virtual-list profiling at approximately 8,300 synthetic rows/sec retained 83,000 rows while rendering about 65 DOM log nodes, demonstrating that virtualization prevents one DOM node per log row.

### Selected-container resource UI

The latest source commit adds a Dozzle-style resource header for the selected container:

- Animated CPU progress bar.
- Animated memory progress bar.
- Smooth width and color transitions.
- CPU thresholds: green below 60%, amber from 60–80%, red above 80%.
- Memory thresholds: green below 70%, amber from 70–85%, red above 85%.
- Memory used versus configured memory limit.
- Neutral memory meter when no container memory limit is available.
- Values refresh with the live container-list refresh cycle.

The PNG log-download action was removed from the toolbar, command palette, and URL synchronization logic. CSV, JSON, TXT, and Markdown incident exports remain.

### Login experience

The login screen now uses a marketing-style split layout:

- Product hero on the left.
- Sign-in panel on the right.
- Product positioning statement and feature highlights.
- Accent badge and decorative glow treatment.
- Responsive mobile stacking.
- Password visibility control.

The updated source UI was visually checked in Chrome and presents as a product-oriented hero while remaining focused on authentication.

## Validation completed

The following checks passed against the latest source:

- Embedded UI syntax check.
- Full `go vet ./...`.
- Full race-enabled Go test suite.
- Production binary build.
- High-volume 500-subscriber fan-out test.
- Git diff validation.
- Remote branch verification: `origin/main` points to `c7a9782c6f2d304de3578e41fc75b828bbac467b`.

## Final smoke-test results for the current live process

Target: `http://127.0.0.1:18080`

| Check | Result |
|---|---|
| `/api/session` | HTTP 200; anonymous/no-auth mode |
| `/api/apps` | HTTP 200; historical `blog` and `shop` entries returned |
| `/api/containers` | HTTP 200; no currently running containers |
| `/api/dashboard` | HTTP 200; dashboard data returned |
| `/api/metrics?app=shop&range=15m` | HTTP 200; no points because no live metric samples are present |
| Login hero present | Yes, but from the older deployed UI |
| Animated resource meters present | No; live HTML is older |
| PNG button removed | No; live HTML still contains the old PNG button |

## Required deployment follow-up

Redeploy the latest source commit `c7a9782` to the live environment. After redeployment, repeat the smoke test with at least one running container and recent metric samples. Verify that:

1. Clicking a container displays CPU and memory bars in the header.
2. The bars transition between green, amber, and red thresholds.
3. The PNG button is absent from the Logs toolbar and command palette.
4. The login hero includes the latest accent-glow styling.
5. `/api/containers` returns the selected container with current CPU and memory values.

Until that redeployment occurs, the new UI is verified in source and in a temporary rebuilt instance, but not in the currently running process on port 18080.
