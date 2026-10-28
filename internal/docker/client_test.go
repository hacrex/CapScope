package docker

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestReadMultiplexed(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "2026-10-28T08:56:00.123456789Z hello\n2026-10-28T08:56:01.000000000Z wor"))
	buf.Write(frame(2, "2026-10-28T08:56:02.000000000Z oops\n"))
	buf.Write(frame(1, "ld\n"))                                      // completes the split line
	buf.Write(frame(1, "2026-10-28T08:56:03.000000000Z no newline")) // flushed at EOF

	var got []LogLine
	if err := readMultiplexed(&buf, 0, func(l LogLine) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 lines, got %d: %+v", len(got), got)
	}
	if got[0].Text != "hello" || got[0].Stream != "stdout" || got[0].TS.Nanosecond() != 123456789 {
		t.Errorf("line0: %+v", got[0])
	}
	if got[1].Text != "oops" || got[1].Stream != "stderr" {
		t.Errorf("line1: %+v", got[1])
	}
	if got[2].Text != "world" {
		t.Errorf("split line not reassembled: %+v", got[2])
	}
	if got[3].Text != "no newline" {
		t.Errorf("trailing: %+v", got[3])
	}
}

func TestReadRawAndTruncate(t *testing.T) {
	in := bytes.NewBufferString("2026-10-28T08:56:00Z " + string(bytes.Repeat([]byte("a"), 50)) + "\n")
	var got []LogLine
	_ = readRaw(in, 10, func(l LogLine) { got = append(got, l) })
	if len(got) != 1 || len(got[0].Text) > 10+len(" ...[truncated]") {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestSample(t *testing.T) {
	var s Stats
	s.CPUStats.CPUUsage.TotalUsage = 2_000_000
	s.PreCPUStats.CPUUsage.TotalUsage = 1_000_000
	s.CPUStats.SystemCPUUsage = 20_000_000
	s.PreCPUStats.SystemCPUUsage = 10_000_000
	s.CPUStats.OnlineCPUs = 4
	s.MemoryStats.Usage = 300
	s.MemoryStats.Stats = map[string]uint64{"inactive_file": 100}
	sm := s.Sample()
	if sm.CPUPct < 39.9 || sm.CPUPct > 40.1 {
		t.Errorf("cpu = %v, want 40", sm.CPUPct)
	}
	if sm.Mem != 200 {
		t.Errorf("mem = %d, want 200 (usage minus inactive_file)", sm.Mem)
	}
}
