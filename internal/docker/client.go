// Package docker is a minimal Docker Engine API client built on net/http.
// It supports unix:// and tcp:// hosts (the latter for docker-socket-proxy).
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base   string
	short  *http.Client // bounded calls
	stream *http.Client // long-lived streams, no overall timeout
}

func New(host string) (*Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	c := &Client{}
	var tr *http.Transport
	switch u.Scheme {
	case "unix":
		sock := u.Path
		tr = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}
		c.base = "http://docker"
	case "tcp", "http":
		tr = &http.Transport{}
		c.base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %q", u.Scheme)
	}
	c.short = &http.Client{Transport: tr, Timeout: 15 * time.Second}
	c.stream = &http.Client{Transport: tr}
	return c, nil
}

// ---- types ----

type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

type Inspect struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	Config       struct {
		Image  string            `json:"Image"`
		Tty    bool              `json:"Tty"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	HostConfig struct {
		Memory        int64 `json:"Memory"`
		NanoCpus      int64 `json:"NanoCpus"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

type LogLine struct {
	TS     time.Time // zero when the daemon gave no timestamp
	Stream string    // "stdout" | "stderr"
	Text   string
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemCPUUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs     uint32 `json:"online_cpus"`
}

type Stats struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

type Sample struct {
	CPUPct                                  float64
	Mem, MemLimit, NetRx, NetTx, BlkR, BlkW uint64
	Pids                                    uint64
}

// Sample derives the numbers the UI cares about, using the same formulas as `docker stats`.
func (s Stats) Sample() Sample {
	var sm Sample
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemCPUUsage) - float64(s.PreCPUStats.SystemCPUUsage)
	n := float64(s.CPUStats.OnlineCPUs)
	if n == 0 {
		n = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if n == 0 {
		n = 1
	}
	if sysDelta > 0 && cpuDelta >= 0 {
		sm.CPUPct = cpuDelta / sysDelta * n * 100
	}
	mem := s.MemoryStats.Usage
	for _, k := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := s.MemoryStats.Stats[k]; ok {
			if v < mem {
				mem -= v
			}
			break
		}
	}
	sm.Mem, sm.MemLimit = mem, s.MemoryStats.Limit
	for _, nw := range s.Networks {
		sm.NetRx += nw.RxBytes
		sm.NetTx += nw.TxBytes
	}
	for _, e := range s.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			sm.BlkR += e.Value
		case "write":
			sm.BlkW += e.Value
		}
	}
	sm.Pids = s.PidsStats.Current
	return sm
}

// ---- calls ----

func (c *Client) get(ctx context.Context, hc *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("docker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	resp, err := c.get(ctx, c.short, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.get(ctx, c.short, "/_ping")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) ListContainers(ctx context.Context, all bool) ([]Container, error) {
	var out []Container
	p := "/containers/json"
	if all {
		p += "?all=1"
	}
	return out, c.getJSON(ctx, p, &out)
}

func (c *Client) Inspect(ctx context.Context, id string) (Inspect, error) {
	var out Inspect
	return out, c.getJSON(ctx, "/containers/"+id+"/json", &out)
}

// Logs follows a container's logs until the stream ends or ctx is cancelled.
// tail is "all" or a number; since, when set, is exclusive of earlier lines.
func (c *Client) Logs(ctx context.Context, id string, tty bool, since time.Time, tail string, maxLine int, fn func(LogLine)) error {
	q := url.Values{"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}}
	if tail != "" && tail != "all" {
		q.Set("tail", tail)
	}
	if !since.IsZero() {
		q.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
	}
	resp, err := c.get(ctx, c.stream, "/containers/"+id+"/logs?"+q.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if tty {
		return readRaw(resp.Body, maxLine, fn)
	}
	return readMultiplexed(resp.Body, maxLine, fn)
}

// Stats streams stats frames (about one per second) until the stream ends.
func (c *Client) Stats(ctx context.Context, id string, fn func(Stats) error) error {
	resp, err := c.get(ctx, c.stream, "/containers/"+id+"/stats?stream=true")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var s Stats
		if err := dec.Decode(&s); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := fn(s); err != nil {
			return err
		}
	}
}

// Events streams container lifecycle events from since onwards.
func (c *Client) Events(ctx context.Context, since time.Time, fn func(Event) error) error {
	q := url.Values{"filters": {`{"type":["container"]}`}}
	if !since.IsZero() {
		q.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
	}
	resp, err := c.get(ctx, c.stream, "/events?"+q.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// ---- log stream decoding ----

func emit(stream, line string, maxLine int, fn func(LogLine)) {
	line = strings.TrimSuffix(line, "\r")
	ll := LogLine{Stream: stream, Text: line}
	if i := strings.IndexByte(line, ' '); i > 20 && i < 40 {
		if t, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
			ll.TS, ll.Text = t, line[i+1:]
		}
	}
	if maxLine > 0 && len(ll.Text) > maxLine {
		ll.Text = ll.Text[:maxLine] + " ...[truncated]"
	}
	fn(ll)
}

// readMultiplexed decodes Docker's 8-byte-header framing used when the container has no TTY.
func readMultiplexed(r io.Reader, maxLine int, fn func(LogLine)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	bufs := map[byte]*bytes.Buffer{1: new(bytes.Buffer), 2: new(bytes.Buffer)}
	names := map[byte]string{1: "stdout", 2: "stderr"}
	flush := func() {
		for k, b := range bufs {
			if b.Len() > 0 {
				emit(names[k], b.String(), maxLine, fn)
				b.Reset()
			}
		}
	}
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			flush()
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:8])
		if n > 1<<24 {
			return fmt.Errorf("log frame too large: %d bytes", n)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			flush()
			if err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		st := hdr[0]
		if st != 1 && st != 2 {
			st = 1
		}
		b := bufs[st]
		b.Write(payload)
		for {
			i := bytes.IndexByte(b.Bytes(), '\n')
			if i < 0 {
				break
			}
			emit(names[st], string(b.Next(i + 1)[:i]), maxLine, fn)
		}
		if maxLine > 0 && b.Len() > maxLine*2 {
			emit(names[st], b.String(), maxLine, fn)
			b.Reset()
		}
	}
}

// readRaw decodes the plain stream used when the container has a TTY (stdout and stderr merged).
func readRaw(r io.Reader, maxLine int, fn func(LogLine)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			emit("stdout", strings.TrimSuffix(line, "\n"), maxLine, fn)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
