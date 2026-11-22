package collector

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HostSampler measures the host's own CPU and memory usage by reading
// /proc/stat and /proc/meminfo. It runs only productively on Linux (the
// Docker host) — every read fails gracefully elsewhere and reports zero, so
// it is safe to start unconditionally on any platform.
//
// One sample per tick; a ring holds the last 10 minutes bucketed into the
// same grid the overview charts use (60 buckets of 10s), so /api/overview can
// chart host CPU% and mem% beside per-container metrics without touching
// Docker at all.
type HostSampler struct {
	mu         sync.Mutex
	samples    []hostSample // newest last; trimmed to hostMaxSamples
	cpuPrev    *cpuTick     // previous /proc/stat sample, for delta computation
	maxSamples int
}

type hostSample struct {
	ts  int64   // unix ms
	cpu float64 // percent of total host capacity, 0-100
	mem float64 // percent of MemTotal used, 0-100
}

type cpuTick struct {
	total int64 // sum of all /proc/stat "cpu" fields
	idle  int64 // idle field (CPU doing no useful work)
}

const (
	hostTick       = 10 * time.Second // sample cadence
	hostMaxSamples = 360              // 60 min at 10s; trimmed aggressively below
)

// NewHostSampler returns a sampler that reads /proc/stat and /proc/meminfo.
func NewHostSampler() *HostSampler {
	return &HostSampler{maxSamples: hostMaxSamples}
}

// Run samples forever until ctx is cancelled. The first tick primes the
// /proc/stat baseline so the very first reported CPU% has a delta to work
// with.
func (h *HostSampler) Run(ctx context.Context) {
	t := time.NewTicker(hostTick)
	defer t.Stop()
	h.readCPUTick() // prime baseline (discarded)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sample()
		}
	}
}

func (h *HostSampler) sample() {
	h.mu.Lock()
	h.samples = append(h.samples, hostSample{
		ts:  time.Now().UnixMilli(),
		cpu: h.readCPU(),
		mem: h.readMem(),
	})
	if len(h.samples) > h.maxSamples {
		h.samples = h.samples[len(h.samples)-h.maxSamples:]
	}
	h.mu.Unlock()
}

// Current returns the newest CPU% and mem% readings (0, 0 until the first
// sample lands).
func (h *HostSampler) Current() (cpu, mem float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.samples); n > 0 {
		last := h.samples[n-1]
		return last.cpu, last.mem
	}
	return 0, 0
}

// Series returns bucketed CPU% and mem% series matching the overview grid
// (60 buckets of OverviewHistStepMS ending at nowMs), trimmed to the first
// bucket that holds a sample. Empty leading buckets are dropped so a newly
// started sampler charts from its first reading rather than leading zeros.
func (h *HostSampler) Series(nowMs int64) (cpu, mem Series) {
	h.mu.Lock()
	defer h.mu.Unlock()
	end := nowMs - nowMs%OverviewHistStepMS
	t0 := end - (overviewHistBuckets-1)*OverviewHistStepMS
	var csum, msum [overviewHistBuckets]float64
	var cn, mn [overviewHistBuckets]int
	first := -1
	for _, s := range h.samples {
		if s.ts < t0 {
			continue
		}
		i := int((s.ts - t0) / OverviewHistStepMS)
		if i < 0 || i >= overviewHistBuckets {
			continue
		}
		if first < 0 || i < first {
			first = i
		}
		csum[i] += s.cpu
		cn[i]++
		msum[i] += s.mem
		mn[i]++
	}
	if first < 0 {
		empty := Series{StepMS: OverviewHistStepMS, V: []float64{}}
		return empty, empty
	}
	cV := make([]float64, 0, overviewHistBuckets-first)
	mV := make([]float64, 0, overviewHistBuckets-first)
	for i := first; i < overviewHistBuckets; i++ {
		var cv, mv float64
		if cn[i] > 0 {
			cv = csum[i] / float64(cn[i])
		}
		if mn[i] > 0 {
			mv = msum[i] / float64(mn[i])
		}
		cV = append(cV, cv)
		mV = append(mV, mv)
	}
	t0 = t0 + int64(first)*OverviewHistStepMS
	return Series{T0: t0, StepMS: OverviewHistStepMS, V: cV},
		Series{T0: t0, StepMS: OverviewHistStepMS, V: mV}
}

// ---- /proc readers ----

func (h *HostSampler) readCPU() float64 {
	cur, ok := h.readCPUTick()
	if !ok {
		return 0
	}
	h.mu.Lock()
	prev := h.cpuPrev
	h.cpuPrev = cur
	h.mu.Unlock()
	if prev == nil {
		return 0 // first sample: no baseline yet
	}
	dt := cur.total - prev.total
	if dt <= 0 {
		return 0
	}
	didle := cur.idle - prev.idle
	if didle < 0 {
		didle = 0
	}
	return 100.0 * float64(dt-didle) / float64(dt)
}

// readCPUTick parses the aggregate "cpu " line of /proc/stat. Fields after
// "cpu": user nice system idle iowait irq softirq steal guest guest_nice.
// Idle time is the "idle" field (the CPU is doing no useful work then).
func (h *HostSampler) readCPUTick() (*cpuTick, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			return nil, false
		}
		var total, idle int64
		for _, x := range f[1:] {
			v, err := strconv.ParseInt(x, 10, 64)
			if err != nil {
				continue
			}
			total += v
		}
		idle, _ = strconv.ParseInt(f[4], 10, 64) // "idle" field
		return &cpuTick{total: total, idle: idle}, true
	}
	return nil, false
}

func (h *HostSampler) readMem() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var total, avail int64
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, _ = strconv.ParseInt(strings.Fields(line)[1], 10, 64)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail, _ = strconv.ParseInt(strings.Fields(line)[1], 10, 64)
		}
		if total > 0 && avail > 0 {
			break
		}
	}
	if total <= 0 {
		return 0
	}
	if avail > total {
		avail = total
	}
	return 100.0 * float64(total-avail) / float64(total)
}
