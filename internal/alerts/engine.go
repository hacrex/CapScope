package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"capscope/internal/store"
)

// Engine periodically evaluates rules and notifies channels. It is idle when
// no rules file exists; editing the file is picked up without a restart.
// Collector events (A6) reach it through a bounded channel: FeedEvent never
// blocks the collector.
type Engine struct {
	st       *store.Store
	path     string
	interval time.Duration
	chans    []Channel
	log      *slog.Logger
	now      func() time.Time // injectable clock for tests

	evCh chan ContainerEvent

	mu    sync.Mutex // guards rules (reloaded on disk change, read by the API)
	rules []Rule
	rmt   time.Time // mtime of the rules file when last loaded
}

// Option tweaks the engine (tests use WithClock).
type Option func(*Engine)

// WithClock replaces the engine clock.
func WithClock(now func() time.Time) Option { return func(e *Engine) { e.now = now } }

// New builds an engine over the store. path is the JSON rules file (may not
// exist yet); interval is the evaluation period (default one minute);
// chans are the notification destinations.
func New(st *store.Store, path string, interval time.Duration, chans []Channel, log *slog.Logger, opts ...Option) *Engine {
	if interval <= 0 {
		interval = time.Minute
	}
	e := &Engine{st: st, path: path, interval: interval, chans: chans, log: log, now: time.Now, evCh: make(chan ContainerEvent, 64)}
	for _, o := range opts {
		o(e)
	}
	e.reload()
	return e
}

// reload re-reads the rules file when it changed on disk.
func (e *Engine) reload() {
	mt := mtime(e.path)
	e.mu.Lock()
	if mt.Equal(e.rmt) {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	rules, err := Load(e.path)
	if err != nil {
		e.log.Error("alerts: rules file rejected, keeping previous rules", "err", err)
		e.mu.Lock()
		e.rmt = mt // don't re-attempt until the file's mtime changes
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	e.rules, e.rmt = rules, mt
	e.mu.Unlock()
	if len(rules) > 0 {
		e.log.Info("alerts: rules loaded", "count", len(rules), "channels", len(e.chans))
	}
}

// Rules returns a copy of the active rules for the API.
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rules
}

// Channels returns the configured channel names for the API.
func (e *Engine) Channels() []string {
	out := make([]string, 0, len(e.chans))
	for _, c := range e.chans {
		out = append(out, c.Name)
	}
	return out
}

// TestChannels exposes the channels for the /api/alerts/test endpoint.
func (e *Engine) TestChannels() []Channel { return e.chans }

// Path returns the rules file location (used by the rule-builder API).
func (e *Engine) Path() string { return e.path }

// SaveRules validates and atomically writes the full rule set to the rules
// file, then forces a reload so the engine state matches disk immediately.
func (e *Engine) SaveRules(rules []Rule) error {
	f := File{Rules: rules}
	if err := f.Validate(); err != nil {
		return err
	}
	if err := Save(e.path, f); err != nil {
		return err
	}
	e.mu.Lock()
	e.rules = rules
	e.rmt = mtime(e.path)
	e.mu.Unlock()
	e.log.Info("alerts: rules saved via API", "count", len(rules))
	return nil
}

// Run evaluates on a ticker until ctx is done. Collector events (A6) are
// handled out-of-band so event rules fire the moment Docker emits them.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.reload()
			if err := e.Evaluate(ctx); err != nil {
				e.log.Error("alerts: evaluation failed", "err", err)
			}
		case ev := <-e.evCh:
			e.handleEvent(ctx, ev)
		}
	}
}

// FeedEvent fans one collector event into the engine. Non-blocking: alerting
// must never slow (or duplicate decisions in) the collector, so a full buffer
// drops the event and says so in the log.
func (e *Engine) FeedEvent(ev ContainerEvent) {
	select {
	case e.evCh <- ev:
	default:
		e.log.Warn("alerts: event dropped (engine busy), buffer full", "action", ev.Action, "app", ev.App)
	}
}

// handleEvent evaluates every event rule against one fanned-in event.
// Firing reuses the same silence window (repeat/cooldown), the same insert
// and the same notification path as the ticker rules.
func (e *Engine) handleEvent(ctx context.Context, ev ContainerEvent) {
	for _, r := range e.Rules() {
		if r.Kind != KindEvent || !r.eventMatches(ev) {
			continue
		}
		msg, ok := r.eventFiring(ev)
		if !ok {
			continue
		}
		if _, firing := e.mayFire(ctx, r, ev.App); !firing {
			continue
		}
		ts := ev.TS
		if ts.IsZero() {
			ts = e.now()
		}
		e.record(ctx, r, ev.App, store.Alert{TS: ts.UnixMilli(), Rule: r.Name, App: ev.App, Kind: r.Kind, Message: msg})
	}
}

// mayFire checks the rule+app silence window: GD-style repeat/cooldown
// suppresses duplicate notifications (identical for ticker and event paths).
func (e *Engine) mayFire(ctx context.Context, r Rule, app string) (time.Time, bool) {
	now := e.now()
	if last := e.st.LastAlertTS(ctx, r.Name, app); last > 0 && now.Sub(time.UnixMilli(last)) < r.Repeat.D() {
		return now, false
	}
	return now, true
}

// record inserts one fired alert and notifies the channels, recording the
// delivery status. Returns the stored alert (id + delivery) so callers can
// report it; the zero value when the insert failed.
func (e *Engine) record(ctx context.Context, r Rule, app string, a store.Alert) store.Alert {
	id, err := e.st.InsertAlert(a)
	if err != nil {
		e.log.Error("alerts: cannot record alert", "rule", r.Name, "app", app, "err", err)
		return store.Alert{}
	}
	a.ID = id
	status := e.notify(ctx, a)
	a.Delivery = status
	if err := e.st.SetAlertDelivery(id, status); err != nil {
		e.log.Error("alerts: cannot record delivery status", "rule", r.Name, "app", app, "err", err)
	}
	return a
}

// Evaluate runs every rule once. It returns the alerts it fired.
func (e *Engine) Evaluate(ctx context.Context) []store.Alert {
	rules := e.Rules()
	if len(rules) == 0 {
		return nil
	}
	apps, err := e.knownApps(ctx)
	if err != nil {
		e.log.Error("alerts: cannot list apps", "err", err)
		return nil
	}
	var fired []store.Alert
	for _, r := range rules {
		for _, app := range expandApps(r.Apps, apps) {
			a, ok := e.evalOne(ctx, r, app)
			if !ok {
				continue
			}
			got := e.record(ctx, r, app, a)
			if got.ID == 0 {
				continue // insert failed
			}
			fired = append(fired, got)
		}
	}
	return fired
}

// evalOne evaluates one rule for one app and reports whether it fired.
func (e *Engine) evalOne(ctx context.Context, r Rule, app string) (store.Alert, bool) {
	now := e.now()
	msg, firing := "", false
	switch r.Kind {
	case KindDeadman:
		last := e.st.LastLogTS(ctx, app)
		if last == 0 {
			return store.Alert{}, false // app never logged; not worth paging
		}
		if gap := now.Sub(time.UnixMilli(last)); gap >= r.For.D() {
			firing, msg = true, fmt.Sprintf("%s: no logs for %s (since %s)", app, gap.Round(time.Minute), time.UnixMilli(last).Format(time.RFC3339))
		}
	case KindRestartLoop:
		n, err := e.st.CountAppDeathsSince(ctx, app, now.Add(-r.Window.D()).UnixMilli())
		if err != nil {
			e.log.Error("alerts: death count failed", "rule", r.Name, "app", app, "err", err)
			return store.Alert{}, false
		}
		// Deliberately strict ">": fire only when deaths EXCEED the limit,
		// matching the semantics documented in rules.go and the README
		// ("More than `count` deaths"). The collector's crash-snapshot log
		// line uses "==" to flag the Nth death itself as a suspected loop;
		// these are two different features with intentionally different
		// thresholds (see TestRestartLoopFires / QuietBelowCount).
		if n > int64(r.Count) {
			firing, msg = true, fmt.Sprintf("%s: %d restarts in %s (limit %d)", app, n, r.Window.D(), r.Count)
		}
	case KindMetric:
		msg, firing = e.evalMetric(ctx, r, app, now)
	case KindLogPattern:
		n, err := e.st.CountLogsMatching(ctx, app, r.re, now.Add(-r.Window.D()).UnixMilli())
		if err != nil {
			e.log.Error("alerts: pattern count failed", "rule", r.Name, "app", app, "err", err)
			return store.Alert{}, false
		}
		if n >= int64(r.Hits) {
			firing, msg = true, fmt.Sprintf("%s: %q matched %d lines in %s (limit %d)", app, r.Pattern, n, r.Window.D(), r.Hits)
		}
	}
	if !firing {
		return store.Alert{}, false
	}
	// Silence: the last firing of this rule+app is recent enough to stay quiet.
	if _, firing := e.mayFire(ctx, r, app); !firing {
		return store.Alert{}, false
	}
	return store.Alert{TS: now.UnixMilli(), Rule: r.Name, App: app, Kind: r.Kind, Message: msg}, true
}

// evalMetric implements A3: the threshold must hold over the whole "for"
// window, so one cold sample inside it stops the alert. Only samples newer
// than the window count, so a stale app cannot fire it. When the rule sets
// Window, the smoothing variant is used instead.
func (e *Engine) evalMetric(ctx context.Context, r Rule, app string, now time.Time) (string, bool) {
	if r.Window.D() > 0 {
		return e.evalMetricSmoothed(ctx, r, app, now)
	}
	since := now.Add(-r.For.D()).UnixMilli()
	pts, err := e.st.MetricSamples(ctx, app, since, now.UnixMilli(), 2000)
	if err != nil || len(pts) == 0 {
		return "", false
	}
	// Need recent data: the newest sample must be younger than twice the window.
	if newest := time.UnixMilli(pts[len(pts)-1].TS); now.Sub(newest) > 2*r.For.D() {
		return "", false
	}
	over := func(v float64) bool {
		if r.Op == "lt" {
			return v < r.Value
		}
		return v > r.Value
	}
	var worst float64
	seen := 0
	for _, p := range pts {
		var v float64
		switch r.Metric {
		case "cpu":
			v = p.CPU
		case "mem":
			v = float64(p.Mem)
		case "mem_pct":
			if p.MemLimit <= 0 {
				continue // no limit known; percentage is undefined
			}
			v = float64(p.Mem) / float64(p.MemLimit) * 100
		}
		seen++
		if !over(v) {
			return "", false // threshold broke inside the window
		}
		// Track the true extreme (max for "gt", min for "lt") so the alert
		// message reports the worst offender, not just the last sample.
		if seen == 1 || (r.Op == "lt" && v < worst) || (r.Op != "lt" && v > worst) {
			worst = v
		}
	}
	if seen == 0 {
		return "", false // e.g. mem_pct with no known limits
	}
	unit := "%"
	if r.Metric == "mem" {
		unit = " B"
	}
	return fmt.Sprintf("%s: %s %s%s for %s (limit %s%s)", app, r.Metric, trimNum(worst), unit, r.For.D(), trimNum(r.Value), unit), true
}

// evalMetricSmoothed is the smoothing variant of evalMetric: the value
// compared is the MEAN of every sample inside the rule's window, so a single
// spiky (or spotty) sample can no longer flip the alert. Freshness is judged
// against the same window.
func (e *Engine) evalMetricSmoothed(ctx context.Context, r Rule, app string, now time.Time) (string, bool) {
	since := now.Add(-r.Window.D()).UnixMilli()
	pts, err := e.st.MetricSamples(ctx, app, since, now.UnixMilli(), 2000)
	if err != nil || len(pts) == 0 {
		return "", false
	}
	if newest := time.UnixMilli(pts[len(pts)-1].TS); now.Sub(newest) > 2*r.Window.D() {
		return "", false // the app is gone; averages of stale data say nothing
	}
	var sum float64
	seen := 0
	for _, p := range pts {
		switch r.Metric {
		case "cpu":
			sum += p.CPU
		case "mem":
			sum += float64(p.Mem)
		case "mem_pct":
			if p.MemLimit <= 0 {
				continue // percentage undefined without a limit
			}
			sum += float64(p.Mem) / float64(p.MemLimit) * 100
		}
		seen++
	}
	if seen == 0 {
		return "", false
	}
	avg := sum / float64(seen)
	fire := avg > r.Value
	if r.Op == "lt" {
		fire = avg < r.Value
	}
	if !fire {
		return "", false
	}
	unit := "%"
	if r.Metric == "mem" {
		unit = " B"
	}
	return fmt.Sprintf("%s: %s avg %s%s over %d samples in %s (limit %s%s)", app, r.Metric, trimNum(avg), unit, seen, r.Window.D(), trimNum(r.Value), unit), true
}

func trimNum(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.1f", v)
}

// knownApps lists every app the store has seen.
func (e *Engine) knownApps(ctx context.Context) ([]string, error) {
	sums, err := e.st.AppSummaries(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(sums))
	for _, s := range sums {
		out = append(out, s.App)
	}
	sort.Strings(out)
	return out, nil
}

// expandApps resolves a rule's app list; empty means every known app.
func expandApps(want, known []string) []string {
	if len(want) > 0 {
		return want
	}
	return known
}

// notify sends the alert to every configured channel and returns the
// delivery status recorded in history: "sent" when at least one channel
// accepted the message, "failed" when all attempted channels errored, or
// "" when no channels are configured (nothing was attempted).
func (e *Engine) notify(ctx context.Context, a store.Alert) string {
	if len(e.chans) == 0 {
		return ""
	}
	msg := fmt.Sprintf("CapScope alert [%s] %s", a.Rule, a.Message)
	ok, attempted := false, 0
	for _, c := range e.chans {
		cctx, cancel := context.WithTimeout(ctx, notifyTimeout)
		err := c.Send(cctx, msg)
		cancel()
		attempted++
		if err != nil {
			e.log.Error("alerts: channel failed", "channel", c.Name, "rule", a.Rule, "err", err)
		} else {
			ok = true
		}
	}
	if attempted > 0 && !ok {
		return "failed"
	}
	return "sent"
}
