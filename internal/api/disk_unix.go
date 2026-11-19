//go:build !windows

package api

import (
	"fmt"
	"os"
	"syscall"
)

// diskUsage reports the total and available bytes of the filesystem holding
// path (the Docker root dir). "Avail" (Bavail, not Bfree) is what non-root
// may write — the same number df shows. Both are 0 when the path is unknown
// or the stat fails; the UI hides the disk gauges then.
func diskUsage(path string) (int64, int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bsize) * int64(st.Blocks), int64(st.Bsize) * int64(st.Bavail), nil
}

// hostUptime reads /proc/uptime (Linux). Returns 0 where it is absent —
// BSD/macOS, or a container without /proc mounted — and the UI hides the
// uptime line when 0.
func hostUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	var up float64
	if _, err := fmt.Sscanf(string(b), "%f", &up); err != nil {
		return 0
	}
	return int64(up)
}
