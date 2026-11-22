// Package incidents defines the incident model and business logic (Phase 5).
//
// Incidents turn raw alert firings into operational workflow: auto-created
// from alert rule firings, auto-resolved when the rule stops firing, and
// manually openable/acknowledgable/resolvable via API and UI.
package incidents

import (
	"encoding/json"
	"time"
)

// Incident is an operational event derived from alert firings.
type Incident struct {
	ID             int64   `json:"id"`
	Title          string  `json:"title"`
	Severity       string  `json:"severity"`      // critical | warning | info
	State          string  `json:"state"`         // open | ack | resolved
	StartedAt      int64   `json:"started_at"`    // unix ms
	EndedAt        int64   `json:"ended_at"`      // unix ms (0 = still open)
	RelatedAlerts  []int64 `json:"related_alert_ids"`
	App            string  `json:"app"`
	HostID         string  `json:"host_id"`
}

// Severity values.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// State values.
const (
	StateOpen     = "open"
	StateAck      = "ack"
	StateResolved = "resolved"
)

// New creates an Incident with sensible defaults.
func New(title, severity, app, hostID string) Incident {
	return Incident{
		Title:         title,
		Severity:      severity,
		State:         StateOpen,
		StartedAt:     time.Now().UnixMilli(),
		App:           app,
		HostID:        hostID,
		RelatedAlerts: []int64{},
	}
}

// RelatedAlertIDsJSON returns the related alerts as a JSON array string.
func (inc Incident) RelatedAlertIDsJSON() string {
	b, _ := json.Marshal(inc.RelatedAlerts)
	return string(b)
}

// IsOpen returns true when the incident is open or acked (not resolved).
func (inc Incident) IsOpen() bool {
	return inc.State != StateResolved
}
