// Package collector watches Docker, follows logs and stats for each running
// container, persists them, and writes a crash snapshot when a container dies.
package collector

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"capscope/internal/access"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/logproc"
	"capscope/internal/store"
)

type Manager struct {
	cfg config.Config
	dk  *docker.Client
	st  *store.Store
	hub *hub.Hub
	log *slog.Logger

	lines      chan store.LogRow
	accessRows chan store.AccessRow
	accDet     access.Detector
	accDropped atomic.Int64

	mu      sync.Mutex
	tracked map[string]*tracker
	lastTS  map[string]int64 // per container id: newest log timestamp accepted (ns)
	// Flags from oom/kill events of containers without a tracker, kept so a
	// death replayed after downtime (C8) is still classified correctly.
	pending map[string]pendingEv

	curMu    sync.Mutex
	curSaved int64 // last Docker event cursor persisted to the store (ns)
}

// pendingEv is an event flag waiting for its container's die event.
type pendingEv struct {
	ts  int64 // event time, unix ns
	oom bool
	sig string
}

const (
	maxCatchUp = 24 * time.Hour // longest event gap replayed from the Docker buffer on startup
	maxPending = 10 * time.Minute
)

func NewManager(cfg config.Config, dk *docker.Client, st *store.Store, h *hub.Hub, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, dk: dk, st: st, hub: h, log: log,
		lines:      make(chan store.LogRow, 8192),
		accessRows: make(chan store.AccessRow, 4096),
		accDet:     access.NewDetector(cfg.AccessLog),
		tracked:    map[string]*tracker{},
		lastTS:     map[string]int64{},
		pending:    map[string]pendingEv{},
	}
}

// ContainerInfo is the live view returned by the API.
type ContainerInfo struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	App      string  `json:"app"`
	Replica  string  `json:"replica"`
	Image    string  `json:"image"`
	Started  int64   `json:"started"`
	CPU      float64 `json:"cpu"`
	Mem      int64   `json:"mem"`
	MemLimit int64   `json:"mem_limit"`
	SampleTS int64   `json:"sample_ts"`
}

func (m *Manager) Containers() []ContainerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ContainerInfo, 0, len(m.tracked))
	for _, t := range m.tracked {
		ci := ContainerInfo{ID: t.id, Name: t.name, App: t.app, Replica: t.replica, Image: t.image, Started: t.created.UnixMilli()}
		if p, ok := t.lastMetric(); ok {
			ci.CPU, ci.Mem, ci.MemLimit, ci.SampleTS = p.CPU, p.Mem, p.MemLimit, p.TS
		}
		out = append(out, ci)
	}
	return out
}

// Run blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); m.writer(ctx) }()
	go func() { defer wg.Done(); m.accessWriter(ctx) }()
	go func() { defer wg.Done(); m.maintenance(ctx) }()

	m.reconcile(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reconcile(ctx)
			}
		}
	}()
	m.eventLoop(ctx)
	wg.Wait()
}

// ---- discovery ----

func (m *Manager) reconcile(ctx context.Context) {
	cs, err := m.dk.ListContainers(ctx, false)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("list containers failed", "err", err)
		}
		return
	}
	seen := map[string]bool{}
	for _, c := range cs {
		seen[c.ID] = true
		m.ensure(ctx, c.ID)
	}
	// Drop trackers for containers that vanished without a die event (missed while reconnecting).
	m.mu.Lock()
	var stale []*tracker
	for id, t := range m.tracked {
		if !seen[id] && time.Since(t.created) > 20*time.Second {
			stale = append(stale, t)
			delete(m.tracked, id)
		}
	}
	m.mu.Unlock()
	for _, t := range stale {
		t.markExited()
		t.cancel()
		if !t.accessOnly {
			_ = m.st.MarkContainerState(t.id, "exited")
		}
		m.log.Info("tracker dropped (no die event seen)", "container", t.name)
	}
}

func (m *Manager) excluded(name, app string) bool {
	if m.cfg.Include != nil && !m.cfg.Include.MatchString(name) && !m.cfg.Include.MatchString(app) {
		return true
	}
	if m.cfg.Exclude != nil && (m.cfg.Exclude.MatchString(name) || m.cfg.Exclude.MatchString(app)) {
		return true
	}
	return false
}

// newTracker inspects a container and builds its tracker. Returns (nil, false, err)
// when the container is not running; excluded containers are still tracked,
// but only to parse their access logs (accessOnly).
func (m *Manager) newTracker(ctx context.Context, id string) (*tracker, bool, error) {
	insp, err := m.dk.Inspect(ctx, id)
	if err != nil {
		return nil, false, err
	}
	name := strings.TrimPrefix(insp.Name, "/")
	app, replica := AppName(name, insp.Config.Labels)
	if !insp.State.Running {
		return nil, false, nil
	}
	return &tracker{
		id: id, name: name, app: app, replica: replica, image: insp.Config.Image,
		tty: insp.Config.Tty, created: time.Now(), logDone: make(chan struct{}),
		accessOnly: m.excluded(name, app),
		acc:        m.accDet.Stream(app),
		maxLogs:    m.cfg.SnapshotLogLines, maxMets: m.cfg.SnapshotMetricSamples,
	}, true, nil
}

func (m *Manager) ensure(ctx context.Context, id string) {
	m.mu.Lock()
	_, ok := m.tracked[id]
	m.mu.Unlock()
	if ok {
		return
	}
	t, running, err := m.newTracker(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Debug("inspect failed", "id", short(id), "err", err)
		}
		return
	}
	if t == nil || !running {
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel // must be set before the tracker becomes visible to event handlers
	m.mu.Lock()
	if _, ok := m.tracked[id]; ok {
		m.mu.Unlock()
		cancel()
		return
	}
	m.tracked[id] = t
	m.mu.Unlock()

	now := time.Now().UnixMilli()
	if t.accessOnly {
		m.log.Info("tracking container (access logs only)", "container", t.name)
	} else {
		_ = m.st.UpsertContainer(store.ContainerRow{ID: id, Name: t.name, App: t.app, Image: t.image, Replica: t.replica, State: "running", FirstSeen: now, LastSeen: now})
		m.log.Info("tracking container", "app", t.app, "container", t.name)
	}
	go m.logLoop(tctx, t)
	if !t.accessOnly {
		go m.statsLoop(tctx, t)
	}
}

func (m *Manager) stillRunning(ctx context.Context, id string) bool {
	insp, err := m.dk.Inspect(ctx, id)
	return err == nil && insp.State.Running
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---- logs ----

func (m *Manager) logLoop(ctx context.Context, t *tracker) {
	defer close(t.logDone)
	for ctx.Err() == nil {
		var since time.Time
		tail := "all"
		m.mu.Lock()
		last := m.lastTS[t.id]
		m.mu.Unlock()
		if last > 0 {
			since = time.Unix(0, last+1)
		} else {
			tail = strconv.Itoa(m.cfg.InitialTail)
		}
		err := m.dk.Logs(ctx, t.id, t.tty, since, tail, m.cfg.MaxLineBytes, func(l docker.LogLine) { m.handleLine(ctx, t, l) })
		if ctx.Err() != nil || t.isExited() {
			return
		}
		if err != nil {
			m.log.Debug("log stream ended", "container", t.name, "err", err)
		}
		if !m.stillRunning(ctx, t.id) {
			return
		}
		sleepCtx(ctx, time.Second)
	}
}

func (m *Manager) handleLine(ctx context.Context, t *tracker, l docker.LogLine) {
	ts := l.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	n := ts.UnixNano()
	m.mu.Lock()
	if !l.TS.IsZero() && n <= m.lastTS[t.id] {
		m.mu.Unlock()
		return // already stored before a reconnect
	}
	if n > m.lastTS[t.id] {
		m.lastTS[t.id] = n
	}
	m.mu.Unlock()

	text := l.Text
	if m.cfg.Redact {
		text = logproc.Redact(text)
	}

	m.maybeAccess(t, text, n)

	row := store.LogRow{
		TS: n, TSMs: n / 1_000_000, ContainerID: t.id, Container: t.name, App: t.app,
		Stream: "o", Level: logproc.Level(text), Line: text,
	}
	if l.Stream == "stderr" {
		row.Stream = "e"
	}
	if t.accessOnly {
		return // not part of the log history; access rows above are the payload
	}
	t.pushLog(row)
	select {
	case m.lines <- row:
	case <-ctx.Done():
	}
}

// maybeAccess parses the line and queues an access row when the container's
// stream parses as web-server access logs. Excluded (accessOnly) containers
// are the prime source: their lines exist here exactly once, in parsed form.
func (m *Manager) maybeAccess(t *tracker, text string, ns int64) {
	rec, ok := t.acc.Observe(text)
	if !ok {
		return
	}
	ar := store.AccessRow{
		TS: ns / 1_000_000, ContainerID: t.id, Container: t.name, App: t.app,
		Method: rec.Method, Path: logproc.Redact(rec.Path), Status: rec.Status,
		Bytes: rec.Bytes, Referer: logproc.Redact(rec.Referer), UA: rec.UA,
	}
	if m.cfg.AnonIPS {
		ar.IP = access.Anonymize(rec.IP)
	} else {
		ar.IP = rec.IP
	}
	select {
	case m.accessRows <- ar:
	default:
		m.accDropped.Add(1)
	}
}

// accessWriter batches parsed access rows into SQLite.
func (m *Manager) accessWriter(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	batch := make([]store.AccessRow, 0, 512)
	flush := func() {
		if n := m.accDropped.Swap(0); n > 0 {
			m.log.Warn("access rows dropped (buffer full)", "dropped", n)
		}
		if len(batch) == 0 {
			return
		}
		if err := m.st.InsertAccess(batch); err != nil {
			m.log.Error("insert access failed", "err", err, "rows", len(batch))
		}
		batch = batch[:0]
	}
	for {
		select {
		case r := <-m.accessRows:
			batch = append(batch, r)
			if len(batch) >= 1000 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			for {
				select {
				case r := <-m.accessRows:
					batch = append(batch, r)
				default:
					flush()
					return
				}
			}
		}
	}
}

// writer batches rows into SQLite, then publishes them (with IDs) to live subscribers.
func (m *Manager) writer(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	batch := make([]store.LogRow, 0, 1024)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := m.st.InsertLogs(batch); err != nil {
			m.log.Error("insert logs failed", "err", err, "rows", len(batch))
		} else {
			m.hub.Publish(batch)
		}
		batch = batch[:0]
	}
	for {
		select {
		case r := <-m.lines:
			batch = append(batch, r)
			if len(batch) >= 1000 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			for {
				select {
				case r := <-m.lines:
					batch = append(batch, r)
				default:
					flush()
					return
				}
			}
		}
	}
}

// ---- stats ----

func (m *Manager) statsLoop(ctx context.Context, t *tracker) {
	var lastSample time.Time
	for ctx.Err() == nil && !t.isExited() {
		err := m.dk.Stats(ctx, t.id, func(s docker.Stats) error {
			if s.PreCPUStats.CPUUsage.TotalUsage == 0 {
				return nil // first frame has no baseline for CPU%
			}
			now := time.Now()
			if now.Sub(lastSample) < m.cfg.StatsInterval {
				return nil
			}
			lastSample = now
			sm := s.Sample()
			p := store.MetricPoint{
				TS: now.UnixMilli(), CPU: sm.CPUPct, Mem: int64(sm.Mem), MemLimit: int64(sm.MemLimit),
				NetRx: int64(sm.NetRx), NetTx: int64(sm.NetTx), BlkR: int64(sm.BlkR), BlkW: int64(sm.BlkW), Pids: int64(sm.Pids),
			}
			t.pushMetric(p)
			if err := m.st.InsertMetric(t.id, t.app, p); err != nil {
				m.log.Error("insert metric failed", "err", err)
			}
			return nil
		})
		if ctx.Err() != nil || t.isExited() {
			return
		}
		if err != nil {
			m.log.Debug("stats stream ended", "container", t.name, "err", err)
		}
		sleepCtx(ctx, 2*time.Second)
		if !m.stillRunning(ctx, t.id) {
			return
		}
	}
}

// ---- events and crash snapshots ----

func (m *Manager) eventLoop(ctx context.Context) {
	since := m.catchUpStart(ctx)
	for ctx.Err() == nil {
		err := m.dk.Events(ctx, since, func(ev docker.Event) error {
			if ev.TimeNano > 0 {
				since = time.Unix(0, ev.TimeNano)
			}
			m.handleEvent(ctx, ev)
			// Die and oom decide crash snapshots, so never lose those from the
			// cursor; ordinary events are checkpointed at most once a second.
			force := ev.Type == "container" && (ev.Action == "die" || ev.Action == "oom")
			m.saveCursor(ev.TimeNano, force)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		m.log.Warn("docker event stream ended, reconnecting", "err", err)
		sleepCtx(ctx, 2*time.Second)
		m.reconcile(ctx)
	}
}

// catchUpStart returns where the event stream should start: the last event
// CapScope processed when that was recent enough to still sit in Docker's
// event buffer (C8), otherwise now. Deaths inside the gap are replayed and
// snapshotted like live ones, so a restart no longer loses crash evidence.
func (m *Manager) catchUpStart(ctx context.Context) time.Time {
	now := time.Now()
	ns := m.st.StateGet(ctx, "event_cursor_ns")
	if ns <= 0 {
		return now
	}
	gap := now.Sub(time.Unix(0, ns))
	if gap <= 0 || gap > maxCatchUp {
		return now
	}
	m.log.Info("replaying docker events missed while CapScope was down", "gap", gap.Round(time.Second))
	return time.Unix(0, ns)
}

// saveCursor checkpoints the newest processed event so the next start can
// replay from it. Throttled to once a second unless force is set.
func (m *Manager) saveCursor(ns int64, force bool) {
	if ns <= 0 {
		return
	}
	m.curMu.Lock()
	defer m.curMu.Unlock()
	if ns <= m.curSaved {
		return
	}
	if !force && ns-m.curSaved < int64(time.Second) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.st.StateSet(ctx, "event_cursor_ns", ns); err != nil {
		m.log.Debug("save event cursor failed", "err", err)
		return
	}
	m.curSaved = ns
}

func (m *Manager) handleEvent(ctx context.Context, ev docker.Event) {
	if ev.Type != "container" {
		return
	}
	id := ev.Actor.ID
	switch ev.Action {
	case "start":
		go m.ensure(ctx, id)
	case "oom":
		if t := m.get(id); t != nil {
			t.setOOM()
		} else {
			m.rememberFlag(id, ev.TimeNano, pendingEv{ts: ev.TimeNano, oom: true})
		}
	case "kill":
		if sig := ev.Actor.Attributes["signal"]; sig != "" && sig != "0" {
			if t := m.get(id); t != nil {
				t.setSignal(sig)
			} else {
				m.rememberFlag(id, ev.TimeNano, pendingEv{ts: ev.TimeNano, sig: sig})
			}
		}
	case "die":
		exit, _ := strconv.Atoi(ev.Actor.Attributes["exitCode"])
		if t := m.get(id); t != nil && ev.TimeNano > 0 && t.created.After(time.Unix(0, ev.TimeNano)) {
			// Historical death (replayed after downtime): this container id has
			// died and been recreated since, and the live tracker belongs to the
			// new incarnation. Snapshot the old death without disturbing it.
			go m.finishDie(ctx, nil, id, exit, ev.TimeNano)
			return
		}
		// Detach synchronously so a restart-policy "start" event for the same id
		// (processed next) creates a fresh tracker instead of seeing the dying one.
		t := m.detach(id)
		go m.finishDie(ctx, t, id, exit, ev.TimeNano)
	case "destroy":
		m.mu.Lock()
		delete(m.lastTS, id)
		delete(m.pending, id)
		m.mu.Unlock()
	}
}

// rememberFlag stores an oom/kill flag for a container that has no tracker
// yet, so the die event that follows (possibly replayed after downtime) can
// still be classified. Older flags than maxPending are dropped.
func (m *Manager) rememberFlag(id string, now int64, f pendingEv) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.pending {
		if now-v.ts > int64(maxPending) {
			delete(m.pending, k)
		}
	}
	m.pending[id] = f
}

// takePending consumes and clears the stored flags for one container.
func (m *Manager) takePending(id string, now int64) (oom bool, sig string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.pending[id]
	delete(m.pending, id)
	if !ok || now-f.ts > int64(maxPending) {
		return false, ""
	}
	return f.oom, f.sig
}

func (m *Manager) get(id string) *tracker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tracked[id]
}

func (m *Manager) detach(id string) *tracker {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tracked[id]
	delete(m.tracked, id)
	return t
}

func (m *Manager) finishDie(ctx context.Context, t *tracker, id string, exit int, dieNs int64) {
	if t != nil {
		t.markExited()
		// Let the log follower drain the final lines (it ends by itself when the container stops).
		select {
		case <-t.logDone:
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		t.cancel()
		if t.accessOnly {
			return // excluded container: nothing to snapshot
		}
	} else {
		// No tracker: the death happened while CapScope was down (events are
		// replayed from the persisted cursor) or between reconcile passes.
		// Rebuild what we can; this works on stopped containers too (C8).
		nt := m.rebuildTracker(ctx, id)
		if nt == nil {
			return
		}
		t = nt
	}
	insp, ierr := m.dk.Inspect(ctx, id)
	if ierr != nil || !insp.State.Running {
		_ = m.st.MarkContainerState(id, "exited")
	}
	oomFlag, sig := t.flags()
	pOOM, pSig := m.takePending(id, dieNs)
	oom := oomFlag || pOOM || (ierr == nil && insp.State.OOMKilled)
	if sig == "" {
		sig = pSig
	}
	kind := classify(exit, oom, sig)
	dieMs := dieNs / 1_000_000
	if ok, _ := m.st.SnapshotExists(ctx, id, dieMs); ok {
		return // this death was already snapshotted (replayed event)
	}
	loop := m.deathLoop(ctx, id, dieMs)
	if !m.cfg.SnapshotKinds[kind] {
		return
	}
	sn := buildSnapshot(t, exit, insp, ierr == nil, kind, dieMs, loop)
	sid, err := m.st.InsertSnapshot(sn)
	if err != nil {
		m.log.Error("insert snapshot failed", "err", err)
		return
	}
	m.log.Warn("crash snapshot saved", "id", sid, "app", t.app, "container", t.name, "kind", kind, "exit", exit, "deaths_in_window", loop.count)
	if loop.threshold > 0 && loop.count == loop.threshold {
		m.log.Warn("possible restart loop", "app", t.app, "container", t.name, "deaths", loop.count, "window", loop.window)
	}
}

// rebuildTracker builds the tracker of a container that died without a live
// follower. Unlike newTracker it accepts stopped containers, because that is
// their normal state during catch-up. Its log buffer comes from the lines
// already stored, or straight from Docker's log file when none were — the
// container died while CapScope was down, so nothing was ever collected.
func (m *Manager) rebuildTracker(ctx context.Context, id string) *tracker {
	insp, err := m.dk.Inspect(ctx, id)
	if err != nil {
		// Removed entirely (docker run --rm, swarm task cleanup): nothing left
		// to inspect or read logs from.
		m.log.Debug("cannot rebuild tracker", "id", short(id), "err", err)
		return nil
	}
	name := strings.TrimPrefix(insp.Name, "/")
	app, replica := AppName(name, insp.Config.Labels)
	t := &tracker{
		id: id, name: name, app: app, replica: replica, image: insp.Config.Image,
		tty: insp.Config.Tty, created: time.Now(), logDone: make(chan struct{}), cancel: func() {},
		maxLogs: m.cfg.SnapshotLogLines, maxMets: m.cfg.SnapshotMetricSamples,
	}
	rows, _ := m.st.QueryLogs(ctx, store.LogQuery{Container: id, Limit: m.cfg.SnapshotLogLines})
	if len(rows) == 0 {
		rows = m.fetchFinalLogs(ctx, t)
	}
	t.logs = rows
	return t
}

// fetchFinalLogs reads the last lines of a (possibly stopped) container
// straight from Docker's log file — the only evidence left when the death
// happened while CapScope was down. Works until the container is removed.
func (m *Manager) fetchFinalLogs(ctx context.Context, t *tracker) []store.LogRow {
	var rows []store.LogRow
	err := m.dk.Logs(ctx, t.id, t.tty, time.Time{}, strconv.Itoa(m.cfg.SnapshotLogLines), m.cfg.MaxLineBytes, func(l docker.LogLine) {
		ts := l.TS
		if ts.IsZero() {
			ts = time.Now()
		}
		text := l.Text
		if m.cfg.Redact {
			text = logproc.Redact(text)
		}
		stream := "o"
		if l.Stream == "stderr" {
			stream = "e"
		}
		rows = append(rows, store.LogRow{
			TS: ts.UnixNano(), TSMs: ts.UnixMilli(), ContainerID: t.id,
			Container: t.name, App: t.app, Stream: stream, Level: logproc.Level(text), Line: text,
		})
	})
	if err != nil {
		m.log.Debug("final log fetch failed", "container", t.name, "err", err)
	}
	return rows
}

// deathLoop counts the deaths of one container stored inside the configured
// window, including this one, for restart-loop detection (X7).
func (m *Manager) deathLoop(ctx context.Context, id string, dieMs int64) restartLoop {
	if m.cfg.RestartLoopCount < 2 {
		return restartLoop{} // disabled
	}
	n := int64(1)
	if dieMs > 0 {
		if c, err := m.st.CountDeathsSince(ctx, id, dieMs-m.cfg.RestartLoopWindow.Milliseconds()); err == nil {
			n = c + 1
		} else {
			m.log.Debug("count deaths failed", "err", err)
		}
	}
	return restartLoop{count: int(n), threshold: m.cfg.RestartLoopCount, window: m.cfg.RestartLoopWindow}
}

// ---- housekeeping ----

func (m *Manager) maintenance(ctx context.Context) {
	run := func() {
		n, err := m.st.Prune(ctx, m.cfg.LogRetention, m.cfg.MetricsRetention, m.cfg.SnapshotRetention)
		if err != nil && ctx.Err() == nil {
			m.log.Error("prune failed", "err", err)
		}
		// S7: per-app budgets run before the global size cap, so a single
		// noisy app is trimmed on its own first.
		p, err := m.st.PrunePerApp(ctx, m.cfg.AppRetention)
		if err != nil && ctx.Err() == nil {
			m.log.Error("per-app prune failed", "err", err)
		}
		budgets := make(map[string]int64, len(m.cfg.AppMaxMB))
		for app, mb := range m.cfg.AppMaxMB {
			budgets[app] = mb << 20
		}
		b, err := m.st.EnforcePerAppSize(ctx, budgets)
		if err != nil && ctx.Err() == nil {
			m.log.Error("per-app size enforcement failed", "err", err)
		}
		d, err := m.st.EnforceSize(ctx, m.cfg.MaxDBMB<<20)
		if err != nil && ctx.Err() == nil {
			m.log.Error("size enforcement failed", "err", err)
		}
		if n+p+b+d > 0 {
			m.log.Info("retention pass", "pruned_by_age", n, "pruned_by_app_age", p, "pruned_by_app_size", b, "pruned_by_size", d)
		}
	}
	sleepCtx(ctx, 30*time.Second)
	for ctx.Err() == nil {
		run()
		sleepCtx(ctx, 10*time.Minute)
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
