//go:build windows

package api

// diskUsage and hostUptime have no cheap stdlib implementation on Windows
// (GetDiskFreeSpaceEx and the uptime counter would pull in x/sys just for
// two gauges). Report 0/0 and 0 — the overview UI hides both when zero.
func diskUsage(string) (int64, int64, error) { return 0, 0, nil }
func hostUptime() int64                      { return 0 }
