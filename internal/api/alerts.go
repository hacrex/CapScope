package api

import (
	"context"
	"encoding/json"

	"capscope/internal/alerts"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// readBody slurps at most max bytes of the request body.
func readBody(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, max))
}

// alertsList returns the recorded alert history (A1 to A4 firings).
func (s *Server) alertsList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.st.ListAlerts(r.Context(), r.URL.Query().Get("app"), limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"alerts": list})
}

// alertsRules describes the active rules and channels, so the UI can show
// what is armed without exposing secrets.
func (s *Server) alertsRules(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		writeJSON(w, map[string]any{"rules": []any{}, "channels": []any{}, "enabled": false})
		return
	}
	rules := s.eng.Rules()
	type ruleView struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		Apps    []string `json:"apps,omitempty"`
		For     string   `json:"for,omitempty"`
		Count   int      `json:"count,omitempty"`
		Window  string   `json:"window,omitempty"`
		Metric  string   `json:"metric,omitempty"`
		Op      string   `json:"op,omitempty"`
		Value   float64  `json:"value,omitempty"`
		Pattern string   `json:"pattern,omitempty"`
		Hits    int      `json:"hits,omitempty"`
		Repeat  string   `json:"repeat,omitempty"`
	}
	views := make([]ruleView, 0, len(rules))
	for _, ru := range rules {
		v := ruleView{Name: ru.Name, Kind: ru.Kind, Apps: ru.Apps, Count: ru.Count, Metric: ru.Metric, Op: ru.Op, Value: ru.Value, Pattern: ru.Pattern, Hits: ru.Hits}
		if ru.For > 0 {
			v.For = ru.For.D().String()
		}
		if ru.Window > 0 {
			v.Window = ru.Window.D().String()
		}
		if ru.Repeat > 0 {
			v.Repeat = ru.Repeat.D().String()
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"rules": views, "channels": s.eng.Channels(), "enabled": true})
}

// alertsTest sends a test message through every configured channel, so a
// user can verify webhooks before an incident.
func (s *Server) alertsTest(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		fail(w, 400, fmt.Errorf("alerting is disabled (no %s)", "ALERTS_FILE"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	msg := fmt.Sprintf("CapScope test alert (%s)", time.Now().Format(time.RFC3339))
	results := map[string]string{}
	for _, c := range s.eng.TestChannels() {
		if err := c.Send(ctx, msg); err != nil {
			results[c.Name] = "failed: " + err.Error()
		} else {
			results[c.Name] = "sent"
		}
	}
	if len(results) == 0 {
		fail(w, 400, fmt.Errorf("no channels configured"))
		return
	}
	writeJSON(w, map[string]any{"results": results})
}

// ruleInput is one rule as submitted by the alert-rule builder UI. Durations
// arrive as Go duration strings ("5m", "1h") and are validated server-side.
type ruleInput struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Apps    []string `json:"apps,omitempty"`
	For     string   `json:"for,omitempty"`
	Count   int      `json:"count,omitempty"`
	Window  string   `json:"window,omitempty"`
	Metric  string   `json:"metric,omitempty"`
	Op      string   `json:"op,omitempty"`
	Value   float64  `json:"value,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Hits    int      `json:"hits,omitempty"`
	Repeat  string   `json:"repeat,omitempty"`
}

func (ri ruleInput) toRule() (alerts.Rule, error) {
	r := alerts.Rule{Name: ri.Name, Kind: ri.Kind, Apps: ri.Apps, Count: ri.Count, Metric: ri.Metric, Op: ri.Op, Value: ri.Value, Pattern: ri.Pattern, Hits: ri.Hits}
	for _, d := range []struct {
		field string
		str   string
		dst   *alerts.Dur
	}{
		{"for", ri.For, &r.For},
		{"window", ri.Window, &r.Window},
		{"repeat", ri.Repeat, &r.Repeat},
	} {
		if d.str == "" {
			continue
		}
		v, err := time.ParseDuration(d.str)
		if err != nil {
			return r, fmt.Errorf("%s: bad duration %q", d.field, d.str)
		}
		*d.dst = alerts.Dur(v)
	}
	return r, nil
}

// parseRulesBody decodes the {"rules":[...]} document into validated rules.
func parseRulesBody(body []byte) ([]alerts.Rule, error) {
	var doc struct {
		Rules []ruleInput `json:"rules"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := make([]alerts.Rule, 0, len(doc.Rules))
	for i, ri := range doc.Rules {
		r, err := ri.toRule()
		if err != nil {
			return nil, fmt.Errorf("rule %d (%s): %w", i, ri.Name, err)
		}
		out = append(out, r)
	}
	// Validate through the alerts package so kind/pattern/duration rules and
	// defaults match exactly what Load applies from disk.
	f := alerts.File{Rules: out}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f.Rules, nil
}

// alertsRulesValidate dry-runs a rules document without touching disk; the
// builder UI calls it to show errors before saving.
func (s *Server) alertsRulesValidate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 1<<20)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if _, err := parseRulesBody(body); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// alertsRulesSave replaces the whole rules file with the posted document.
// The engine hot-reloads and starts evaluating the new rules immediately.
func (s *Server) alertsRulesSave(w http.ResponseWriter, r *http.Request) {
	if s.eng == nil {
		fail(w, 400, fmt.Errorf("alerting is disabled (no ALERTS_FILE configured)"))
		return
	}
	body, err := readBody(r, 1<<20)
	if err != nil {
		fail(w, 400, err)
		return
	}
	rules, err := parseRulesBody(body)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.eng.SaveRules(rules); err != nil {
		fail(w, 400, err)
		return
	}
	s.log.Info("alerts: rules updated via API", "count", len(rules), "path", s.eng.Path())
	writeJSON(w, map[string]any{"ok": true, "count": len(rules)})
}
