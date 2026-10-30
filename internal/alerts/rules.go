// Package alerts evaluates user rules against the store and notifies channels.
package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Rule kinds. Each maps to one alert feature (A1 to A4).
const (
	KindDeadman     = "deadman"      // A1: no logs from an app for N minutes
	KindRestartLoop = "restart_loop" // A2: more than N deaths in M minutes
	KindMetric      = "metric"       // A3: metric above/below value for N minutes
	KindLogPattern  = "log_pattern"  // A4: regex matched N times in a window
)

// Dur is a time.Duration that marshals as Go duration strings ("5m") in JSON.
type Dur time.Duration

func (d Dur) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Dur) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Dur(v)
	return nil
}

func (d Dur) D() time.Duration { return time.Duration(d) }

// Rule is one alerting rule, loaded from the JSON rules file.
type Rule struct {
	Name string   `json:"name"`
	Kind string   `json:"kind"`
	Apps []string `json:"apps,omitempty"` // empty means every app

	// deadman (A1)
	For Dur `json:"for,omitempty"` // silence length that fires the alert

	// restart_loop (A2)
	Count  int `json:"count,omitempty"`  // deaths that fire the alert (> count)
	Window Dur `json:"window,omitempty"` // counted inside this window

	// metric (A3)
	Metric string  `json:"metric,omitempty"` // cpu | mem | mem_pct
	Op     string  `json:"op,omitempty"`     // gt | lt
	Value  float64 `json:"value,omitempty"`  // threshold (bytes for mem)
	// For (A3): the threshold must hold for this long.

	// log_pattern (A4)
	Pattern string `json:"pattern,omitempty"` // regex over the log line
	Hits    int    `json:"hits,omitempty"`    // matched lines that fire (>= hits)

	// silence between two notifications of the same rule+app.
	Repeat Dur `json:"repeat,omitempty"`

	re *regexp.Regexp
}

// validate compiles patterns and applies defaults; called after loading.
func (r *Rule) validate() error {
	if r.Name == "" {
		return errors.New("rule needs a name")
	}
	switch r.Kind {
	case KindDeadman:
		if r.For.D() <= 0 {
			r.For = Dur(5 * time.Minute)
		}
	case KindRestartLoop:
		if r.Count <= 0 {
			r.Count = 3
		}
		if r.Window.D() <= 0 {
			r.Window = Dur(10 * time.Minute)
		}
	case KindMetric:
		if r.Metric != "cpu" && r.Metric != "mem" && r.Metric != "mem_pct" {
			return fmt.Errorf("rule %s: metric must be cpu, mem or mem_pct", r.Name)
		}
		if r.Op != "gt" && r.Op != "lt" {
			return fmt.Errorf("rule %s: op must be gt or lt", r.Name)
		}
		if r.For.D() <= 0 {
			r.For = Dur(5 * time.Minute)
		}
	case KindLogPattern:
		if r.Pattern == "" {
			return fmt.Errorf("rule %s: pattern is required", r.Name)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("rule %s: bad pattern: %w", r.Name, err)
		}
		r.re = re
		if r.Hits <= 0 {
			r.Hits = 1
		}
		if r.Window.D() <= 0 {
			r.Window = Dur(10 * time.Minute)
		}
	default:
		return fmt.Errorf("rule %s: unknown kind %q", r.Name, r.Kind)
	}
	if r.Repeat.D() <= 0 {
		r.Repeat = Dur(30 * time.Minute)
	}
	return nil
}

// File is the on-disk rules document.
type File struct {
	Rules []Rule `json:"rules"`
}

// Load reads and validates a rules file. A missing file yields no rules and
// no error, so CapScope still runs before the user writes one.
func Load(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	for i := range f.Rules {
		if err := f.Rules[i].validate(); err != nil {
			return nil, err
		}
	}
	return f.Rules, nil
}

// mtime is the file change probe for hot reload; zero when the file is gone.
func mtime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
