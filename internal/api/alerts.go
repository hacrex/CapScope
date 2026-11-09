package api

import (
	"context"
	"encoding/json"

	"capscope/internal/alerts"
	"capscope/internal/auth"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// readBody slurps at most max bytes of the request body.
func readBody(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, max))
}

// alertAdmin gates alert configuration: only admins may change rules or fire
// test webhooks. Proxy-auth deployments have no local session manager, so they
// must be rejected outright rather than falling through the (short-circuiting)
// role check below.
func (s *Server) alertAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.sessions == nil {
		http.Error(w, "forbidden: admin authentication is not configured", http.StatusForbidden)
		return false
	}
	if s.sessions.Role(requestToken(r)) != auth.RoleAdmin {
		http.Error(w, "forbidden: admin required", http.StatusForbidden)
		return false
	}
	return true
}

// alertsList returns the recorded alert history (A1 to A4 firings).
func (s *Server) alertsList(w http.ResponseWriter, r *http.Request) {
	if apps, restricted := s.viewerScope(r); restricted && !appInScope(r.URL.Query().Get("app"), apps) {
		http.Error(w, "forbidden: choose an assigned application", http.StatusForbidden)
		return
	}
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
		Name     string   `json:"name"`
		Kind     string   `json:"kind"`
		Apps     []string `json:"apps,omitempty"`
		For      string   `json:"for,omitempty"`
		Count    int      `json:"count,omitempty"`
		Window   string   `json:"window,omitempty"`
		Metric   string   `json:"metric,omitempty"`
		Op       string   `json:"op,omitempty"`
		Value    float64  `json:"value,omitempty"`
		Pattern  string   `json:"pattern,omitempty"`
		Hits     int      `json:"hits,omitempty"`
		Repeat   string   `json:"repeat,omitempty"`
		Event    string   `json:"event,omitempty"`
		Status   string   `json:"status,omitempty"`
		Exclude  []int    `json:"exclude,omitempty"`
		Cooldown string   `json:"cooldown,omitempty"`
	}
	views := make([]ruleView, 0, len(rules))
	allowed, restricted := s.viewerScope(r)
	for _, ru := range rules {
		if restricted {
			visible := false
			for _, app := range ru.Apps {
				if appInScope(app, allowed) {
					visible = true
					break
				}
			}
			if !visible {
				continue
			}
		}
		v := ruleView{Name: ru.Name, Kind: ru.Kind, Apps: ru.Apps, Count: ru.Count, Metric: ru.Metric, Op: ru.Op, Value: ru.Value, Pattern: ru.Pattern, Hits: ru.Hits,
			Event: ru.Event, Status: ru.Status, Exclude: ru.Exclude}
		if ru.For > 0 {
			v.For = ru.For.D().String()
		}
		if ru.Window > 0 {
			v.Window = ru.Window.D().String()
		}
		if ru.Repeat > 0 {
			v.Repeat = ru.Repeat.D().String()
		}
		if ru.Cooldown > 0 {
			v.Cooldown = ru.Cooldown.D().String()
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"rules": views, "channels": s.eng.Channels(), "enabled": true})
}

// alertsTest sends a test message through every configured channel, so a
// user can verify webhooks before an incident.
func (s *Server) alertsTest(w http.ResponseWriter, r *http.Request) {
	if !s.alertAdmin(w, r) {
		return
	}
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

// alertsTestRule evaluates a rule against recent log data without saving it.
// Used by the alert rule builder to preview whether a rule would fire.
func (s *Server) alertsTestRule(w http.ResponseWriter, r *http.Request) {
	if !s.alertAdmin(w, r) {
		return
	}
	if s.eng == nil {
		fail(w, 400, fmt.Errorf("alerting is disabled (no ALERTS_FILE)"))
		return
	}
	body, err := readBody(r, 1<<20)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var req struct {
		Rule  alerts.Rule `json:"rule"`
		Limit int         `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, 400, err)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 100
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	result := struct {
		Firing       bool   `json:"firing"`
		MatchingRows int    `json:"matchingRows"`
		Message      string `json:"message,omitempty"`
	}{}

	switch req.Rule.Kind {
	case alerts.KindDeadman:
		app := ""
		if len(req.Rule.Apps) > 0 {
			app = req.Rule.Apps[0]
		}
		last := s.st.LastLogTS(ctx, app)
		if last == 0 {
			result.Firing = false
			result.Message = "no logs found for app"
		} else {
			gap := now.Sub(time.UnixMilli(last))
			result.Firing = gap >= req.Rule.For.D()
			result.Message = fmt.Sprintf("no logs for %s (last %s ago)", gap.Round(time.Minute), gap.Round(time.Minute))
		}
	case alerts.KindRestartLoop:
		app := ""
		if len(req.Rule.Apps) > 0 {
			app = req.Rule.Apps[0]
		}
		n, err := s.st.CountDeathsSince(ctx, app, now.Add(-req.Rule.Window.D()).UnixMilli())
		if err != nil {
			fail(w, 500, err)
			return
		}
		result.Firing = n > int64(req.Rule.Count)
		result.MatchingRows = int(n)
		result.Message = fmt.Sprintf("%d restarts in %s (limit %d)", n, req.Rule.Window.D(), req.Rule.Count)
	case alerts.KindMetric:
		app := ""
		if len(req.Rule.Apps) > 0 {
			app = req.Rule.Apps[0]
		}
		// Use store.QueryMetrics instead of unexported evalMetric
		pts, err := s.st.QueryMetrics(ctx, app, "", "", now.Add(-req.Rule.Window.D()).UnixMilli(), now.UnixMilli(), 60000)
		if err != nil {
			fail(w, 500, err)
			return
		}
		var n int64
		for _, pt := range pts {
			if req.Rule.Op == "gt" && pt.CPU > req.Rule.Value {
				n++
			} else if req.Rule.Op == "lt" && pt.CPU < req.Rule.Value {
				n++
			}
		}
		result.Firing = n > 0
		result.MatchingRows = int(n)
		result.Message = fmt.Sprintf("%d samples above threshold (limit %f)", n, req.Rule.Value)
	case alerts.KindLogPattern:
		re, err := regexp.Compile(req.Rule.Pattern)
		if err != nil {
			fail(w, 400, fmt.Errorf("invalid regex: %w", err))
			return
		}
		app := ""
		if len(req.Rule.Apps) > 0 {
			app = req.Rule.Apps[0]
		}
		n, err := s.st.CountLogsMatching(ctx, app, re, now.Add(-req.Rule.Window.D()).UnixMilli())
		if err != nil {
			fail(w, 500, err)
			return
		}
		result.Firing = n >= int64(req.Rule.Hits)
		result.MatchingRows = int(n)
		result.Message = fmt.Sprintf("%d matches in %s (limit %d)", n, req.Rule.Window.D(), req.Rule.Hits)
	case alerts.KindEvent:
		result.Firing = false
		result.Message = "event rules fire on live Docker events, not on historical data"
	default:
		fail(w, 400, fmt.Errorf("unknown rule kind: %s", req.Rule.Kind))
		return
	}

	writeJSON(w, result)
}

// snapshotExport renders a crash snapshot as a Markdown document.

// ruleInput is one rule as submitted by the alert-rule builder UI. Durations
// arrive as Go duration strings ("5m", "1h") and are validated server-side.
type ruleInput struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Apps     []string `json:"apps,omitempty"`
	For      string   `json:"for,omitempty"`
	Count    int      `json:"count,omitempty"`
	Window   string   `json:"window,omitempty"`
	Metric   string   `json:"metric,omitempty"`
	Op       string   `json:"op,omitempty"`
	Value    float64  `json:"value,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Hits     int      `json:"hits,omitempty"`
	Repeat   string   `json:"repeat,omitempty"`
	Event    string   `json:"event,omitempty"`
	Status   string   `json:"status,omitempty"`
	Exclude  []int    `json:"exclude,omitempty"`
	Cooldown string   `json:"cooldown,omitempty"`
}

func (ri ruleInput) toRule() (alerts.Rule, error) {
	r := alerts.Rule{Name: ri.Name, Kind: ri.Kind, Apps: ri.Apps, Count: ri.Count, Metric: ri.Metric, Op: ri.Op, Value: ri.Value, Pattern: ri.Pattern, Hits: ri.Hits,
		Event: ri.Event, Status: ri.Status, Exclude: ri.Exclude}
	for _, d := range []struct {
		field string
		str   string
		dst   *alerts.Dur
	}{
		{"for", ri.For, &r.For},
		{"window", ri.Window, &r.Window},
		{"repeat", ri.Repeat, &r.Repeat},
		{"cooldown", ri.Cooldown, &r.Cooldown},
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
	if !s.alertAdmin(w, r) {
		return
	}
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
	if !s.alertAdmin(w, r) {
		return
	}
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
