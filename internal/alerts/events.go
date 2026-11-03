package alerts

import (
	"fmt"
	"time"
)

// ContainerEvent is one Docker lifecycle event fanned into the engine by the
// collector (A6). Only alert-relevant actions reach the engine; the collector
// resolves the app name so rules can filter on it.
//
// Action is one of "die", "oom", "health_status" or the updater's pseudo-event
// "update_available". Attr carries the raw Docker actor attributes (e.g.
// healthStatus, signal) plus the update checker's image/digest fields.
type ContainerEvent struct {
	Action    string
	ID        string // container id
	Container string // container name (e.g. web-1)
	App       string // app the rule filters on
	ExitCode  int    // die: parsed exit code
	Attr      map[string]string
	TS        time.Time // event time; zero means "now"
}

// eventMatches reports whether one event rule accepts one event: the action
// must equal the rule's event name and, when the rule lists apps, the event's
// app must be one of them.
func (r *Rule) eventMatches(ev ContainerEvent) bool {
	if r.Event != ev.Action {
		return false
	}
	if len(r.Apps) > 0 {
		for _, a := range r.Apps {
			if a == ev.App {
				return true
			}
		}
		return false
	}
	return true
}

// eventFiring turns one accepted event into a decision + message for the
// rule. Behavior per event kind mirrors rules.go's documented semantics.
func (r *Rule) eventFiring(ev ContainerEvent) (string, bool) {
	switch ev.Action {
	case "die":
		for _, x := range r.Exclude {
			if x == ev.ExitCode {
				return "", false
			}
		}
		return fmt.Sprintf("%s: container %s died with exit code %d", ev.App, ev.Container, ev.ExitCode), true
	case "oom":
		return fmt.Sprintf("%s: container %s ran out of memory (OOM)", ev.App, ev.Container), true
	case "health_status":
		if ev.Attr["healthStatus"] != r.Status {
			return "", false
		}
		return fmt.Sprintf("%s: container %s health status is %s", ev.App, ev.Container, r.Status), true
	case "update_available":
		msg := fmt.Sprintf("%s: update available for %s (%s)", ev.App, ev.Container, ev.Attr["image"])
		if ev.Attr["local"] != "" && ev.Attr["remote"] != "" {
			msg += fmt.Sprintf(" (%s -> %s)", shortDigest(ev.Attr["local"]), shortDigest(ev.Attr["remote"]))
		}
		return msg, true
	}
	return "", false
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
