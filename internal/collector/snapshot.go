package collector

import (
	"encoding/json"
	"fmt"
	"time"

	"capscope/internal/docker"
	"capscope/internal/store"
)

// classify decides what kind of stop this was.
//
//	oom        kernel OOM killer (event or inspect flag)
//	clean_exit exit code 0
//	killed     a kill/stop signal was sent before death (docker stop, swarm rollout)
//	crash      anything else with a non-zero exit code
func classify(exit int, oom bool, signal string) string {
	switch {
	case oom:
		return "oom"
	case exit == 0:
		return "clean_exit"
	case signal != "":
		return "killed"
	default:
		return "crash"
	}
}

// restartLoop is the death context of one snapshot: how many times this
// container died inside the configured window (count includes this death).
// A count below threshold, or a zero threshold, means no loop was detected.
type restartLoop struct {
	count, threshold int
	window           time.Duration
}

func ordinal(n int) string {
	suf := "th"
	if r := n % 100; r < 11 || r > 13 {
		switch n % 10 {
		case 1:
			suf = "st"
		case 2:
			suf = "nd"
		case 3:
			suf = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suf)
}

func humanDur(d time.Duration) string {
	if h := int(d.Hours()); h > 0 {
		if h == 1 {
			return "hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	m := int(d.Minutes())
	if m == 1 {
		return "minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// summarize produces a short rule-based explanation. It states what the data
// shows and the usual meaning of the exit code, not a diagnosis.
func summarize(kind string, exit int, peakMem, memLimit int64, errCount int, lastErr string, loop restartLoop) string {
	var s string
	switch {
	case kind == "oom":
		s = "Killed by the kernel OOM killer."
		if memLimit > 0 && peakMem > 0 {
			s += fmt.Sprintf(" Peak sampled memory was %s of a %s limit.", humanBytes(peakMem), humanBytes(memLimit))
		}
		s += " Raise the memory limit or look for a leak."
	case kind == "clean_exit":
		s = "Exited with code 0."
	case exit == 137:
		s = "Exit 137 (SIGKILL) without an OOM flag: killed from outside, for example docker kill, a Swarm stop timeout, or host memory pressure."
	case exit == 143:
		s = "Exit 143 (SIGTERM): stopped by a termination signal."
	case exit == 139:
		s = "Exit 139 (SIGSEGV): the process crashed with a segmentation fault."
	case exit == 126:
		s = "Exit 126: the entrypoint or command was found but is not executable."
	case exit == 127:
		s = "Exit 127: the entrypoint or command was not found. Check CMD and ENTRYPOINT."
	case exit == 1:
		s = "Exit 1: the application exited with a generic error. Check the last error lines."
	default:
		s = fmt.Sprintf("Exited with code %d.", exit)
	}
	if kind == "killed" {
		s += " A stop signal was sent first, so this was likely a deploy, restart or manual stop."
	}
	if loop.threshold > 0 && loop.count >= loop.threshold {
		s += fmt.Sprintf(" This is the %s death within %s, so the container is likely restart-looping.", ordinal(loop.count), humanDur(loop.window))
	}
	if errCount > 0 {
		s += fmt.Sprintf(" %d error-level lines in the last captured logs.", errCount)
		if lastErr != "" {
			if len(lastErr) > 160 {
				lastErr = lastErr[:160] + "..."
			}
			s += " Last: " + lastErr
		}
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

type inspectSummary struct {
	Status        string `json:"status"`
	ExitCode      int    `json:"exit_code"`
	OOMKilled     bool   `json:"oom_killed"`
	Error         string `json:"error"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	RestartCount  int    `json:"restart_count"`
	MemoryLimit   int64  `json:"memory_limit"`
	NanoCPUs      int64  `json:"nano_cpus"`
	RestartPolicy string `json:"restart_policy"`
	Image         string `json:"image"`
}

// buildSnapshot assembles the evidence for one container death. insp may be
// zero-valued when Docker no longer knows the container. dieMs is the death
// time from the Docker event (0 when unknown); a snapshot rebuilt after the
// fact keeps that time instead of the time it was assembled.
func buildSnapshot(t *tracker, exit int, insp docker.Inspect, haveInspect bool, kind string, dieMs int64, loop restartLoop) store.Snapshot {
	logs := t.recentLogs()
	mets := t.recentMetrics()

	var peak int64
	for _, m := range mets {
		if m.Mem > peak {
			peak = m.Mem
		}
	}
	var errCount int
	var lastErr string
	for _, l := range logs {
		if l.Level == "error" {
			errCount++
			lastErr = l.Line
		}
	}

	sn := store.Snapshot{
		TS:             time.Now().UnixMilli(),
		DieTS:          dieMs,
		Kind:           kind,
		ContainerID:    t.id,
		Container:      t.name,
		App:            t.app,
		Image:          t.image,
		ExitCode:       exit,
		OOMKilled:      kind == "oom",
		RestartsWindow: loop.count,
		Logs:           logs,
		Metrics:        mets,
	}
	if dieMs > 0 {
		sn.TS = dieMs // caught-up deaths keep their real time
	}
	if haveInspect {
		sn.Error = insp.State.Error
		sn.RestartCount = insp.RestartCount
		sn.MemLimit = insp.HostConfig.Memory
		if st, err1 := time.Parse(time.RFC3339Nano, insp.State.StartedAt); err1 == nil {
			if fin, err2 := time.Parse(time.RFC3339Nano, insp.State.FinishedAt); err2 == nil && fin.After(st) {
				sn.UptimeSec = int64(fin.Sub(st).Seconds())
			}
		}
		b, _ := json.Marshal(inspectSummary{
			Status: insp.State.Status, ExitCode: insp.State.ExitCode, OOMKilled: insp.State.OOMKilled,
			Error: insp.State.Error, StartedAt: insp.State.StartedAt, FinishedAt: insp.State.FinishedAt,
			RestartCount: insp.RestartCount, MemoryLimit: insp.HostConfig.Memory, NanoCPUs: insp.HostConfig.NanoCpus,
			RestartPolicy: insp.HostConfig.RestartPolicy.Name, Image: insp.Config.Image,
		})
		sn.Inspect = b
	}
	if sn.MemLimit == 0 && len(mets) > 0 {
		sn.MemLimit = mets[len(mets)-1].MemLimit
	}
	sn.Summary = summarize(kind, exit, peak, sn.MemLimit, errCount, lastErr, loop)
	return sn
}
