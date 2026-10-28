// Package collector watches Docker, follows logs and stats for each running
// container, persists them, and writes a crash snapshot when a container dies.
package collector

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

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

	lines chan store.LogRow

	mu      sync.Mutex
	tracked map[string]*tracker
	lastTS  map[string]int64 // per container id: newest log timestamp accepted (ns)
}

func NewManager(cfg config.Config, dk *docker.Client, st *store.Store, h *hub.Hub, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, dk: dk, st: st, hub: h, log: log,
		lines:   make(chan store.LogRow, 8192),
		tracked: map[string]*tracker{},
		lastTS:  map[string]int64{},
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
	wg.Add(2)
	go func() { defer wg.Done(); m.writer(ctx) }()
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
		_ = m.st.MarkContainerState(t.id, "exited")
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

// newTracker inspects a container and builds its tracker. Returns (nil, nil) when excluded.
func (m *Manager) newTracker(ctx context.Context, id string) (*tracker, bool, error) {
	insp, err := m.dk.Inspect(ctx, id)
	if err != nil {
		return nil, false, err
	}
	name := strings.TrimPrefix(insp.Name, "/")
	app, replica := AppName(name, insp.Config.Labels)
	if m.excluded(name, app) {
		return nil, false, nil
	}
	return &tracker{
		id: id, name: name, app: app, replica: replica, image: insp.Config.Image,
		tty: insp.Config.Tty, created: time.Now(), logDone: make(chan struct{}),
		maxLogs: m.cfg.SnapshotLogLines, maxMets: m.cfg.SnapshotMetricSamples,
	}, insp.State.Running, nil
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
	_ = m.st.UpsertContainer(store.ContainerRow{ID: id, Name: t.name, App: t.app, Image: t.image, Replica: t.replica, State: "running", FirstSeen: now, LastSeen: now})
	m.log.Info("tracking container", "app", t.app, "container", t.name)
	go m.logLoop(tctx, t)
	go m.statsLoop(tctx, t)
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
	row := store.LogRow{
		TS: n, TSMs: n / 1_000_000, ContainerID: t.id, Container: t.name, App: t.app,
		Stream: "o", Level: logproc.Level(text), Line: text,
	}
	if l.Stream == "stderr" {
		row.Stream = "e"
	}
	t.pushLog(row)
	select {
	case m.lines <- row:
	case <-ctx.Done():
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
	since := time.Now()
	for ctx.Err() == nil {
		err := m.dk.Events(ctx, since, func(ev docker.Event) error {
			if ev.TimeNano > 0 {
				since = time.Unix(0, ev.TimeNano)
			}
			m.handleEvent(ctx, ev)
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
		}
	case "kill":
		if sig := ev.Actor.Attributes["signal"]; sig != "" && sig != "0" {
			if t := m.get(id); t != nil {
				t.setSignal(sig)
			}
		}
	case "die":
		exit, _ := strconv.Atoi(ev.Actor.Attributes["exitCode"])
		// Detach synchronously so a restart-policy "start" event for the same id
		// (processed next) creates a fresh tracker instead of seeing the dying one.
		t := m.detach(id)
		go m.finishDie(ctx, t, id, exit)
	case "destroy":
		m.mu.Lock()
		delete(m.lastTS, id)
		m.mu.Unlock()
	}
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

func (m *Manager) finishDie(ctx context.Context, t *tracker, id string, exit int) {
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
	} else {
		// No tracker: the container started and died between reconcile passes. Rebuild what we can.
		nt, _, err := m.newTracker(ctx, id)
		if err != nil || nt == nil {
			return
		}
		rows, _ := m.st.QueryLogs(ctx, store.LogQuery{Container: id, Limit: m.cfg.SnapshotLogLines})
		nt.logs = rows
		t = nt
	}
	_ = m.st.MarkContainerState(id, "exited")

	insp, ierr := m.dk.Inspect(ctx, id)
	oomFlag, sig := t.flags()
	oom := oomFlag || (ierr == nil && insp.State.OOMKilled)
	kind := classify(exit, oom, sig)
	if !m.cfg.SnapshotKinds[kind] {
		return
	}
	sn := buildSnapshot(t, exit, insp, ierr == nil, kind)
	sid, err := m.st.InsertSnapshot(sn)
	if err != nil {
		m.log.Error("insert snapshot failed", "err", err)
		return
	}
	m.log.Warn("crash snapshot saved", "id", sid, "app", t.app, "container", t.name, "kind", kind, "exit", exit)
}

// ---- housekeeping ----

func (m *Manager) maintenance(ctx context.Context) {
	run := func() {
		n, err := m.st.Prune(ctx, m.cfg.LogRetention, m.cfg.MetricsRetention, m.cfg.SnapshotRetention)
		if err != nil && ctx.Err() == nil {
			m.log.Error("prune failed", "err", err)
		}
		d, err := m.st.EnforceSize(ctx, m.cfg.MaxDBMB<<20)
		if err != nil && ctx.Err() == nil {
			m.log.Error("size enforcement failed", "err", err)
		}
		if n+d > 0 {
			m.log.Info("retention pass", "pruned_by_age", n, "pruned_by_size", d)
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
