# CapScope UI/UX Design System

**Status:** Proposal for v0.3 / v1.0  
**Owner:** Kashvit  
**Target audience:** Self-hosters, homelab brewers, DevOps engineers, CapRover users  
**Goal:** Make CapScope the "Dozzle with memory" that engineers love to star, fork, and run on every home server.

---

## 1. Recommended Theme: Neubrutalism + Dark-First

### Pick this: **Neubrutalism (Dark mode default)**

| Style | Fit for CapScope | Why |
|-------|------------------|-----|
| **Neubrutalism** | ★★★★★ | Thick black borders, high contrast, hard shadows, bold primary colors. Feels like a developer tool, not a SaaS dashboard. Instantly recognizable in screenshots → more GitHub stars. |
| **Bento** | ★★★☆☆ | Clean grid cards are nice for metrics, but too "corporate SaaS" for a homelab tool. Risk of looking generic. |
| **Brutalism** | ★★☆☆☆ | Too raw/hostile for daily monitoring. Good for a landing page, tiring for an 8-hour incident watch. |
| **Light mode** | ★★☆☆☆ | Monitoring happens at 3 AM. Dark-first is non-negotiable. Light mode as an opt-in toggle. |

### Design tokens (extend the existing CSS vars in `index.html`)

```css
:root {
  /* Neubrutalism core */
  --nb-bg: #0a0a0a;
  --nb-panel: #141414;
  --nb-border: #000000;
  --nb-shadow: 4px 4px 0px #000000;
  --nb-shadow-hover: 2px 2px 0px #000000;

  /* Accent */
  --nb-accent: #00ff41;   /* matrix green for "healthy" */
  --nb-accent-hover: #00cc33;

  /* Semantic */
  --nb-ok: #00ff41;
  --nb-warn: #ffcc00;
  --nb-err: #ff3131;
  --nb-deploy: #3291ff;

  /* Typography */
  --nb-mono: ui-monospace, SFMono-Regular, "Geist Mono", Menlo, Consolas, monospace;
  --nb-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
}

[data-theme="light"] {
  --nb-bg: #ffffff;
  --nb-panel: #f4f4f4;
  --nb-border: #000000;
  --nb-shadow: 4px 4px 0px #000000;
  --nb-accent: #0070f3;
}
```

**Neubrutalism rules for CapScope:**
- Every card, button, and input gets a 2–3px solid black border.
- Buttons have a hard shadow (4px 4px 0 black) that lifts on hover (shadow shrinks, button moves down 2px).
- No rounded corners larger than 6px. Prefer 4px or square.
- High-contrast text: white on black, black on accent colors.
- Accent color used sparingly — for deploy markers, live indicators, and primary CTAs.

---

## 2. Screen-by-screen UX Specs

### 2.1 Login Screen (`/login`)

**Why:** Replace the browser Basic Auth popup with a branded, ownable entry point. A login page is the first thing visitors see on GitHub screenshots.

**Layout:**
- Full viewport dark background with subtle noise/gradient.
- Centered neubrutalism card (4px black border, 4px 4px shadow).
- CapScope logo + wordmark at top.
- Fields: Username, Password, "Remember me" checkbox, "Sign in" button (full width).
- Footer: "Forgot password? Set AUTH_PASS env var" + version badge.

**Behavior:**
- POST to `/api/login` → returns session token stored in `localStorage` + `HttpOnly` cookie.
- On app load, if no token → redirect to `/login`. If token → verify with `/api/auth/verify`.
- 401 → shake animation + inline error text (neubrutalism red border).
- "Sign in" button disabled while loading, shows spinner.

**GitHub-star factor:** A custom login page makes the product look finished, not like an internal tool.

---

### 2.2 Global Dashboard (`/` or `/dashboard`)

Currently users land directly in per-app logs. A dashboard gives context at a glance — critical for homelabbers with 10+ apps.

**Top stat row (6 cards):**
1. **Containers** — total count, chip showing running/stopped split.
2. **Errors (1h)** — red badge with count, click → filters logs to error level.
3. **Warnings (1h)** — amber badge.
4. **CPU %** — sparkline + current value, red if > 85%.
5. **Memory** — used / limit bar, red if > 90%.
6. **Uptime** — host uptime or "X containers up".

**Container grid (bento-lite):**
- One card per app, 3–4 columns.
- Each card: app name, health chip (🟢 running / 🔴 stopped / 🟡 degraded), mini CPU + memory sparklines (72px tall), last log line (truncated), "Open logs" CTA.
- Hover: card lifts, shadow grows, reveals "Metrics" and "Crashes" quick links.

**Recent deploys strip:**
- Horizontal scroll of deploy markers from `/api/deploys` with version + time ago.

**Recent crashes strip:**
- Last 5 crashes with exit code chips (137 = OOM red, 1 = crash red, 0 = clean exit green).

---

### 2.3 CPU & Memory Metrics (Metrics tab)

Current: two static canvases. Target: observability-grade, still lightweight.

**Enhancements:**
1. **Live streaming:** SSE stream pushes new samples every second. "Live" dot pulses green. Pause toggle.
2. **Multi-container overlay:** Checkbox select up to 4 containers, each a different color, with a legend showing current value.
3. **Threshold lines:** User-defined dashed line at 80% CPU / 90% memory. Area above threshold shaded light red.
4. **Mem limit line:** Already partially implemented — make it a prominent dashed red line with label "mem limit".
5. **Tooltip:** Hover canvas → crosshair + timestamp + exact value box (neubrutalism: black border, white bg, black text).
6. **Export:** "Download PNG" button (canvas `toDataURL`) per chart.
7. **Table view toggle:** Switch from charts to a sortable table: Container | CPU % | Mem MiB | Mem Limit | Uptime | Restarts | Status.
8. **Time presets:** Add 30 days to existing 15m / 1h / 6h / 24h / 7d.

---

### 2.4 Container Sidebar

Current: simple list. Target: command center.

**Enhancements:**
- **Search box:** "Filter apps…" at top, debounced.
- **Status groups:** Collapsible sections — "Running (12)", "Stopped (3)", "Errors (1)".
- **Pin/star:** Star icon per app, persisted in `localStorage`. Pinned apps float to top.
- **Quick actions:** Hover reveals 📋 Logs, 📈 Metrics, 💥 Crashes, 🔁 Restart (if API added).
- **All-apps mode:** Keep the merged timeline — it's a killer feature for homelabbers.

---

### 2.5 Log View (Logs tab)

Already strong. Add:

- **Log bookmarks:** Star a log line or time range to revisit later (stored in `localStorage`, keyed by app + ts).
- **Split-pane diff:** Compare two replicas side-by-side (already supports multiple panes — make it easier to trigger).
- **Copy line:** Hover over a log line → small "copy" button.
- **Severity chips:** Add `fatal`, `panic`, `critical` colors beyond error/warn/debug.
- **Auto-scroll per pane:** Independent follow/pause per replica pane.

---

### 2.6 Access Logs (Access tab)

GoAccess-derived. Add:

- **Top slow endpoints:** If access logs include response time.
- **GeoIP country labels:** Flag emoji next to top client IPs (optional, privacy toggle).
- **4xx / 5xx trend:** Separate colored lines on the timeline.

---

### 2.7 Crashes (Crashes tab)

- **Crash timeline:** Vertical chronological timeline of all crashes across apps, filterable by app.
- **OOM highlight:** Red "OOM KILLED" badge when exit 137 / OOMKilled flag.
- **Restart loop grouping:** Auto-group containers crashing in a loop with 🔁 badge.
- **One-click actions:** "Restart container", "View logs before crash" (needs Docker API endpoint).

---

### 2.8 Alerts (Alerts tab)

- **Rule builder UI:** Form to create deadman / restart_loop / metric / log_pattern rules (instead of editing `ALERTS_FILE` JSON).
- **Channel tester modal:** Dropdown (Slack, Discord, Webhook, Telegram) + test message.
- **Alert history:** Table of fired alerts: time, rule, app, message, channel status.
- **Silence:** Mute an alert for 1h / 24h / until resolved.

---

### 2.9 Settings Page

Replace env vars with UI where safe:

| Section | Settings |
|---------|----------|
| General | App name, theme (auto/dark/light), timezone |
| Auth | Login method, session timeout, change password |
| Docker | Socket path, `DOCKER_HOST`, exclude patterns (`^captain-`) |
| Storage | `MAX_DB_MB`, retention days, backup location |
| Alerts | Webhook URLs, channels, test sender |
| About | Version, build date, Git commit, license |

---

## 3. Theme Decision Summary

| Question | Answer |
|----------|--------|
| Primary theme | **Neubrutalism** |
| Default mode | **Dark** |
| Light mode | Opt-in toggle, persisted in `localStorage` |
| Why for GitHub stars | Neubrutalism screenshots look like a "real dev tool" — sharp, bold, memorable. Homelabbers and engineers share dark-mode dashboards on Twitter/Reddit. |
| Color accent | Matrix green `#00ff41` for healthy, CapRover blue `#3291ff` for deploys |

---

## 4. Implementation Priority

| Priority | Feature | Effort | File |
|----------|---------|--------|------|
| P0 | Login screen + session token | Medium | `index.html`, `internal/api/server.go` |
| P0 | Neubrutalism design tokens + dark default | Low | `index.html` `<style>` |
| P1 | Global dashboard | Medium | `index.html` `#view-dashboard` |
| P1 | Live streaming metrics charts | Medium | `index.html` `loadMetrics()` |
| P2 | Sidebar search + filters + pin | Low | `index.html` sidebar |
| P2 | Chart PNG export + tooltips | Low | `index.html` canvas |
| P3 | Settings UI page | Medium | `index.html` + API |
| P3 | Alert rule builder UI | Medium-High | `index.html` + `internal/alerts` |
| P4 | Log bookmarks + split-pane diff | Low | `index.html` |
| P4 | Crash timeline + one-click restart | Medium | `index.html` + Docker API |

---

## 5. GitHub Growth Tips (for stars)

1. **Screenshot-first README:** The `README.md` should lead with a big dark-mode dashboard screenshot. Neubrutalism looks great in preview cards.
2. **One-liner tagline:** "Dozzle with memory — persistent container logs, crash snapshots, and metrics for CapRover and Docker, in one self-hosted binary."
3. **Homelab SEO:** Add tags like `homelab`, `caprover`, `docker-logs`, `self-hosted`, `oom-killer`, `crash-snapshots` to repo topics.
4. **Ship the login page early:** A custom auth screen signals "this is a real product" to visitors.
5. **Changelog with screenshots:** Every release post should include a before/after UI screenshot.
