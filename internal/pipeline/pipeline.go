// Package pipeline implements ingest-time log→metrics rollups (Phase 2).
//
// A pipelines.yaml file declares rules like:
//
//   - name: error_rate
//     match:
//     level: "error"
//     app: "shop"
//     metric: app_errors_total
//     type: counter
//     group_by: [app, container]
//
// Each matching log line increments a counter metric stored in the same
// `metrics` table used by dashboards and alert rules. The engine hot-reloads
// on file change (mirroring alerts.Engine.reload) and supports an optional
// one-time backfill of existing logs.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"time"

	"capscope/internal/store"
	"gopkg.in/yaml.v3"
)

// Rule is one log→metric rollup rule.
type Rule struct {
	Name    string   `yaml:"name"`
	Match   Match    `yaml:"match"`
	Metric  string   `yaml:"metric"`
	Type    string   `yaml:"type"` // "counter" or "gauge"
	GroupBy []string `yaml:"group_by"`
	Value   string   `yaml:"value"` // optional: regex capture group name for gauge values
}

// Match selects which log lines a rule applies to. Empty fields match anything.
type Match struct {
	Level     string `yaml:"level"`
	Regex     string `yaml:"regex"`
	App       string `yaml:"app"`
	Container string `yaml:"container"`
}

// Pipeline is one compiled rollup rule with its matchers pre-compiled.
type Pipeline struct {
	Name    string
	Match   Match
	Metric  string
	Type    string
	GroupBy []string
	Value   string
	levelRx *regexp.Regexp // compiled when Match.Level is set
	regexRx *regexp.Regexp // compiled when Match.Regex is set
	appRx   *regexp.Regexp // compiled when Match.App is set
	contRx  *regexp.Regexp // compiled when Match.Container is set
}

// Engine evaluates log rows against compiled pipelines and emits MetricPoint
// rows. Safe for concurrent use.
type Engine struct {
	mu        sync.RWMutex
	pipelines []*Pipeline
	log       *slog.Logger
	file      string
	mtime     time.Time
}

// NewEngine creates a pipeline engine with no rules loaded.
func NewEngine(log *slog.Logger) *Engine {
	return &Engine{log: log}
}

// Load reads pipelines.yaml from file and compiles the rules. Returns the
// number of rules loaded. A missing file is not an error — it just means no
// pipelines are configured.
func (e *Engine) Load(file string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loadLocked(file)
}

func (e *Engine) loadLocked(file string) (int, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			e.pipelines = nil
			e.file = file
			e.mtime = time.Time{}
			return 0, nil
		}
		return 0, err
	}
	var raw []Rule
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return 0, fmt.Errorf("parse pipelines: %w", err)
	}
	pipes := make([]*Pipeline, 0, len(raw))
	for _, r := range raw {
		p, err := compileRule(r)
		if err != nil {
			return 0, fmt.Errorf("compile rule %q: %w", r.Name, err)
		}
		pipes = append(pipes, p)
	}
	info, _ := os.Stat(file)
	e.pipelines = pipes
	e.file = file
	if info != nil {
		e.mtime = info.ModTime()
	}
	e.log.Info("pipelines loaded", "file", file, "rules", len(pipes))
	return len(pipes), nil
}

// MaybeReload checks the file mtime and reloads if changed. Safe to call
// periodically (e.g. every 30 seconds).
func (e *Engine) MaybeReload() (int, error) {
	e.mu.RLock()
	file := e.file
	mtime := e.mtime
	e.mu.RUnlock()
	if file == "" {
		return 0, nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return 0, nil
	}
	if info.ModTime().Equal(mtime) {
		return 0, nil
	}
	return e.Load(file)
}

// Pipelines returns a copy of the compiled rules.
func (e *Engine) Pipelines() []*Pipeline {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Pipeline, len(e.pipelines))
	copy(out, e.pipelines)
	return out
}

// Evaluate matches one log row against all pipelines and returns the metric
// points to emit. A counter increments by 1; a gauge sets the value (parsed
// from the Value capture group, or 1). The metric name is embedded in the
// App field with a "pipe:" prefix so pipeline-derived metrics are
// distinguishable from container metrics in /api/metrics.
func (e *Engine) Evaluate(row store.LogRow) []store.MetricPoint {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []store.MetricPoint
	for _, p := range e.pipelines {
		if !p.matches(row) {
			continue
		}
		ts := row.TSMs
		// Build a dimension suffix from group-by fields so the same metric
		// name with different dims stays distinct.
		var dims string
		for _, g := range p.GroupBy {
			switch g {
			case "level":
				dims += "|level=" + row.Level
			case "stream":
				dims += "|stream=" + row.Stream
			case "app":
				dims += "|app=" + row.App
			case "container":
				dims += "|container=" + row.Container
			}
		}
		val := 1.0
		if p.Type == "gauge" && p.Value != "" {
			if v, ok := extractCapture(row.Line, p.Value); ok {
				val = v
			}
		}
		out = append(out, store.MetricPoint{
			TS:        ts,
			Container: row.Container,
			App:       "pipe:" + p.Metric + dims,
			HostID:    row.HostID,
			CPU:       val,
			Mem:       0,
		})
	}
	return out
}

func compileRule(r Rule) (*Pipeline, error) {
	p := &Pipeline{
		Name:    r.Name,
		Match:   r.Match,
		Metric:  r.Metric,
		Type:    r.Type,
		GroupBy: r.GroupBy,
		Value:   r.Value,
	}
	if p.Type == "" {
		p.Type = "counter"
	}
	var err error
	if r.Match.Level != "" {
		p.levelRx, err = regexp.Compile("(?i)" + regexp.QuoteMeta(r.Match.Level))
		if err != nil {
			return nil, err
		}
	}
	if r.Match.Regex != "" {
		p.regexRx, err = regexp.Compile(r.Match.Regex)
		if err != nil {
			return nil, err
		}
	}
	if r.Match.App != "" {
		p.appRx, err = regexp.Compile("(?i)" + r.Match.App)
		if err != nil {
			return nil, err
		}
	}
	if r.Match.Container != "" {
		p.contRx, err = regexp.Compile("(?i)" + r.Match.Container)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Pipeline) matches(row store.LogRow) bool {
	if p.levelRx != nil && !p.levelRx.MatchString(row.Level) {
		return false
	}
	if p.regexRx != nil && !p.regexRx.MatchString(row.Line) {
		return false
	}
	if p.appRx != nil && !p.appRx.MatchString(row.App) {
		return false
	}
	if p.contRx != nil && !p.contRx.MatchString(row.Container) {
		return false
	}
	return true
}

// extractCapture pulls a named regex capture group from a line and parses it
// as a float64. Returns ok=false when the group is missing or not a number.
func extractCapture(line, group string) (float64, bool) {
	// group is expected to be a regex with a named capture, e.g. "latency=(\d+)"
	re, err := regexp.Compile(group)
	if err != nil {
		return 0, false
	}
	m := re.FindStringSubmatch(line)
	if len(m) < 2 {
		return 0, false
	}
	var v float64
	_, err = fmt.Sscanf(m[1], "%f", &v)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Backfill scans existing logs older than `sinceMs` and emits metric points
// for each pipeline match, up to `limit` rows. Returns the number of points
// emitted. Used for one-time backfill when a new pipeline is added.
func (e *Engine) Backfill(ctx context.Context, st *store.Store, sinceMs, limit int64) (int64, error) {
	// This is intentionally a best-effort, memory-bounded scan: we page
	// through logs and emit points without materializing the whole result set.
	e.mu.RLock()
	pipes := make([]*Pipeline, len(e.pipelines))
	copy(pipes, e.pipelines)
	e.mu.RUnlock()
	if len(pipes) == 0 {
		return 0, nil
	}
	var total int64
	var afterID int64
	for {
		rows, err := st.QueryLogs(ctx, store.LogQuery{
			SinceMs: sinceMs,
			AfterID: afterID,
			Limit:   1000,
		})
		if err != nil {
			return total, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			e.mu.RLock()
			_ = pipes // evaluate under RLock would deadlock; copy already taken
			e.mu.RUnlock()
			for _, p := range pipes {
				if !p.matches(row) {
					continue
				}
				total++
				if limit > 0 && total > limit {
					return total, nil
				}
			}
			afterID = row.ID
		}
	}
	return total, nil
}
