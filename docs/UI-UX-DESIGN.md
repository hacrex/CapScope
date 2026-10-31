# CapScope UI/UX Design System

**Status:** Proposal for v0.3 / v1.0  
**Owner:** Kashvit  
**Target audience:** Self-hosters, homelab brewers, DevOps engineers, CapRover users  
**Goal:** Make CapScope the "Dozzle with memory" that engineers love to star, fork, and run on every home server.

---

## 1. Recommended Theme: Neubrutalism + Brand Colors

### Pick this: **Neubrutalism (White + Docker Orange + Deep Navy)**

| Style | Fit for CapScope | Why |
|-------|------------------|-----|
| **Neubrutalism** | ★★★★★ | Thick navy borders, hard shadows, high contrast, bold Docker orange accent. Feels like a developer tool, not a SaaS dashboard. Instantly recognizable in screenshots → more GitHub stars. |
| **Bento** | ★★★☆☆ | Clean grid cards are nice for metrics, but too "corporate SaaS" for a homelab tool. Risk of looking generic. |
| **Brutalism** | ★★☆☆☆ | Too raw/hostile for daily monitoring. Good for a landing page, tiring for an 8-hour incident watch. |
| **Light mode** | ★★☆☆☆ | Monitoring happens at 3 AM. Dark mode available as opt-in toggle, but default is light with deep navy accents. |

### Design tokens (CSS vars in `m-site/assets/style.css`)

```css
:root {
  /* Brand colors */
  --brand-white: #ffffff;
  --brand-orange: #f89c1c;      /* Docker orange */
  --brand-orange-hover: #e88614;
  --brand-navy: #0a192f;        /* Deep navy */
  --brand-navy-dark: #071221;

  /* Backgrounds */
  --bg: #ffffff;
  --bg-panel: #f8fafc;
  --bg-elevated: #ffffff;

  /* Borders — thick navy borders for neubrutalism */
  --border: var(--brand-navy);

  /* Text */
  --text: var(--brand-navy);
  --text-muted: #475569;
  --text-inverse: #ffffff;

  /* Accent */
  --accent: var(--brand-orange);
  --accent-hover: var(--brand-orange-hover);
  --accent-text: var(--brand-navy);

  /* Semantic */
  --ok: #15803d;
  --ok-bg: #dcfce7;
  --warn: #b45309;
  --warn-bg: #fef3c7;
  --err: #dc2626;
  --err-bg: #fee2e2;

  /* Neubrutalism shadows */
  --shadow: 6px 6px 0px var(--brand-navy);
  --shadow-hover: 3px 3px 0px var(--brand-navy);
  --shadow-none: none;

  /* Typography */
  --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  --font-mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
}
```

**Neubrutalism rules for CapScope:**
- Every card, button, and input gets a **4px solid deep navy border**.
- Buttons have a hard shadow (`6px 6px 0 #0a192f`) that shrinks on hover (`3px 3px 0 #0a192f`).
- Border radius: **8px** max. Prefer 8px or square.
- High-contrast text: deep navy on white, white on orange/navy.
- Accent color (Docker orange `#f89c1c`) used sparingly — for primary CTAs, badges, and deploy markers.
- Backgrounds: white (`#ffffff`) default, light panel (`#f8fafc`) for cards.

---

## 2. Screen-by-screen UX Specs

### 2.1 Login Screen (`/login`)

**Why:** Replace the browser Basic Auth popup with a branded, ownable entry point. A login page is the first thing visitors see on GitHub screenshots.

**Layout:**
- Full viewport white background with subtle panel sections.
- Centered neubrutalism card (4px navy border, 6px 6px shadow).
- CapScope logo + wordmark at top.
- Fields: Username, Password, "Remember me" checkbox, "Sign in" button (full width).
- Footer: "Forgot password? Set AUTH_PASS env var" + version badge.

**Behavior:**
- POST to `/api/login` → returns session token stored in `localStorage` + `HttpOnly` cookie.
- On app load, if no token → redirect to `/login`. If token → verify with `/api/auth/verify`.
- 401 → shake animation + inline error text (navy border, red text).
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
5. **Tooltip:** Hover canvas → crosshair + timestamp + exact value box (neubrutalism: navy border, white bg, navy text).
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
| Background | **White** (`#ffffff`) |
| Primary accent | **Docker Orange** (`#f89c1c`) |
| Border / text | **Deep Navy** (`#0a192f`) |
| Default mode | **Light** |
| Dark mode | Opt-in toggle, persisted in `localStorage` |
| Why for GitHub stars | Neubrutalism screenshots look like a "real dev tool" — sharp, bold, memorable. Clean white + navy + orange looks professional in preview cards. |
| Color accent | Docker orange `#f89c1c` for primary CTAs, deep navy `#0a192f` for borders and text |

---

## 4. Implementation Priority

| Priority | Feature | Effort | File |
|----------|---------|--------|------|
| P0 | Login screen + session token | Medium | `index.html`, `internal/api/server.go` |
| P0 | Neubrutalism design tokens + brand colors | Low | `m-site/assets/style.css` |
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

1. **Screenshot-first README:** The `README.md` should lead with a big dashboard screenshot. Neubrutalism with white + navy + orange looks great in preview cards.
2. **One-liner tagline:** "Containers never truly disappear — persistent container logs, crash snapshots, and metrics for CapRover and Docker, in one self-hosted binary."
3. **Homelab SEO:** Add tags like `homelab`, `caprover`, `docker-logs`, `self-hosted`, `oom-killer`, `crash-snapshots` to repo topics.
4. **Ship the login page early:** A custom auth screen signals "this is a real product" to visitors.
5. **Changelog with screenshots:** Every release post should include a before/after UI screenshot.
