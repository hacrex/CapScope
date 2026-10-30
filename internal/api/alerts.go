package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

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
