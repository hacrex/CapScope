// Package agent runs CapScope in agent mode: it collects logs, metrics, and
// Docker events from the local Docker daemon (the same collector logic as
// serve mode) and streams them to a hub over mutual TLS instead of storing
// them locally. It is read-only toward Docker, like serve mode.
//
// Usage: capscope agent
//
// Environment:
//
//	AGENT_HUB        host:port of the hub (required)
//	AGENT_HOST_ID    stable host identity (defaults to CAPSCOPE_HOST_ID, then hostname)
//	AGENT_CA_CERT    path to the hub CA certificate PEM
//	AGENT_CERT       path to the agent certificate PEM
//	AGENT_KEY        path to the agent private key PEM
//	DOCKER_FILTER    label selector (name=value[,name=value]) limiting tracked containers
package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"capscope/internal/alerts"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
	"capscope/internal/transport"
)

const (
	agentFlushInterval = 250 * time.Millisecond
	agentMaxLogBatch   = 512
	agentMaxMetBatch   = 256
	agentMaxEvtBatch   = 256
	containerListEvery = 5 * time.Second
	reconnectBackoff   = 2 * time.Second
)

// version is set by the build (ldflags -X). Falls back to "dev".
var version = "dev"

// Run connects to the hub, streams collected rows until ctx is cancelled,
// reconnecting with backoff if the link drops.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.AgentHub == "" {
		return errors.New("agent: AGENT_HUB is required in agent mode")
	}
	hostID := resolveHostID(cfg, log)
	if cfg.AccessLog != "off" {
		log.Warn("agent: access-log streaming is not yet supported; forcing ACCESS_LOG=off")
		cfg.AccessLog = "off"
	}

	dk, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	if err := dk.Ping(ctx); err != nil {
		return fmt.Errorf("agent: cannot reach Docker at %s: %w", cfg.DockerHost, err)
	}

	caPool, err := loadCAPool(cfg.AgentCACert)
	if err != nil {
		return err
	}

	for ctx.Err() == nil {
		if err := runOnce(ctx, cfg, dk, hostID, caPool, log); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("agent: hub link down, reconnecting", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(reconnectBackoff):
			}
		}
	}
	return nil
}

func runOnce(ctx context.Context, cfg config.Config, dk *docker.Client, hostID string, caPool *x509.CertPool, log *slog.Logger) error {
	// Dial + mutual TLS handshake.
	dialer := net.Dialer{Timeout: 10 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", cfg.AgentHub)
	if err != nil {
		return fmt.Errorf("agent: dial %s: %w", cfg.AgentHub, err)
	}
	cfgTLS, err := transport.ClientTLSConfig(
		transport.CertPaths{CA: cfg.AgentCACert, Cert: cfg.AgentCert, Key: cfg.AgentKey},
		caPool, "",
	)
	if err != nil {
		raw.Close()
		return err
	}
	conn := tls.Client(raw, cfgTLS)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return fmt.Errorf("agent: tls handshake: %w", err)
	}
	log.Info("agent: connected to hub", "hub", cfg.AgentHub, "host", hostID)

	stream := transport.WrapConn(conn)
	// Hello identifies this host to the hub.
	if err := stream.Send(transport.Hello{
		HostID:  hostID,
		Version: version,
	}); err != nil {
		conn.Close()
		return fmt.Errorf("agent: send hello: %w", err)
	}

	sink := newAgentSink(hostID, stream, log)
	// Drain acks the hub sends back so the bi-di stream never blocks on the
	// hub's write side.
	go func() {
		for {
			var discarded any
			if err := stream.Recv(&discarded); err != nil {
				return // connection closed
			}
		}
	}()
	// Nil store: agent streams, never persists locally.
	mgr := collector.NewManagerWithSink(cfg, dk, nil, hub.New(), sink, log)
	mgr.SetEventSink(sink.sendEvent)
	mgr.SetTrackIf(trackIfFromFilter(cfg.DockerFilter))

	done := make(chan struct{})
	go func() { defer close(done); mgr.Run(ctx) }()

	// Periodic flush of partially-filled batches + container list snapshot.
	flush := time.NewTicker(agentFlushInterval)
	containers := time.NewTicker(containerListEvery)
	defer flush.Stop()
	defer containers.Stop()
	defer conn.Close()
	defer close(sink.done)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-flush.C:
			if err := sink.flush(ctx); err != nil {
				return fmt.Errorf("agent: flush: %w", err)
			}
		case <-containers.C:
			if err := stream.Send(transport.ContainerList{
				HostID:     hostID,
				Containers: containerInfos(mgr.Containers("")),
			}); err != nil {
				return fmt.Errorf("agent: send container list: %w", err)
			}
		case <-done:
			_ = sink.flush(ctx)
			return nil
		}
	}
}

type agentSink struct {
	hostID string
	conn   *transport.Conn
	log    *slog.Logger
	done   chan struct{}

	mu   sync.Mutex
	logs []store.LogRow
	mets []store.MetricPoint
	evts []docker.Event
}

func newAgentSink(hostID string, conn *transport.Conn, log *slog.Logger) *agentSink {
	return &agentSink{hostID: hostID, conn: conn, log: log, done: make(chan struct{})}
}

func (s *agentSink) UpsertContainer(c store.ContainerRow) error { return nil }
func (s *agentSink) InsertAccess(rows []store.AccessRow) error  { return nil }
func (s *agentSink) Publish(rows []store.LogRow)                {}

func (s *agentSink) sendEvent(ev alerts.ContainerEvent) {
	s.mu.Lock()
	s.evts = append(s.evts, toDockerEvent(ev))
	full := len(s.evts) >= agentMaxEvtBatch
	if full {
		buf := s.evts
		s.evts = nil
		s.mu.Unlock()
		_ = s.conn.Send(transport.EventBatch{HostID: s.hostID, Events: buf})
		return
	}
	s.mu.Unlock()
}

func (s *agentSink) InsertLogs(rows []store.LogRow) error {
	s.mu.Lock()
	s.logs = append(s.logs, rows...)
	full := len(s.logs) >= agentMaxLogBatch
	if full {
		buf := s.logs
		s.logs = nil
		s.mu.Unlock()
		return s.conn.Send(transport.LogBatch{HostID: s.hostID, Rows: buf})
	}
	s.mu.Unlock()
	return nil
}

func (s *agentSink) InsertMetric(containerID, app string, p store.MetricPoint) error {
	p.Container = containerID
	p.App = app
	s.mu.Lock()
	s.mets = append(s.mets, p)
	full := len(s.mets) >= agentMaxMetBatch
	if full {
		buf := s.mets
		s.mets = nil
		s.mu.Unlock()
		return s.conn.Send(transport.MetricBatch{HostID: s.hostID, Samples: buf})
	}
	s.mu.Unlock()
	return nil
}

// flush sends any partially-filled batches. Called periodically so data flows
// even below the batch-size threshold.
func (s *agentSink) flush(ctx context.Context) error {
	s.mu.Lock()
	var lb []store.LogRow
	lb, s.logs = s.logs, nil
	var mb []store.MetricPoint
	mb, s.mets = s.mets, nil
	var eb []docker.Event
	eb, s.evts = s.evts, nil
	s.mu.Unlock()
	if len(lb) > 0 {
		if err := s.conn.Send(transport.LogBatch{HostID: s.hostID, Rows: lb}); err != nil {
			return err
		}
	}
	if len(mb) > 0 {
		if err := s.conn.Send(transport.MetricBatch{HostID: s.hostID, Samples: mb}); err != nil {
			return err
		}
	}
	if len(eb) > 0 {
		if err := s.conn.Send(transport.EventBatch{HostID: s.hostID, Events: eb}); err != nil {
			return err
		}
	}
	return nil
}

// toDockerEvent converts an alert-engine ContainerEvent into a docker.Event
// for transport as an EventBatch.
func toDockerEvent(e alerts.ContainerEvent) docker.Event {
	ev := docker.Event{Type: "container", Action: e.Action, TimeNano: e.TS.UnixNano()}
	ev.Actor.ID = e.ID
	ev.Actor.Attributes = e.Attr
	return ev
}

// trackIfFromFilter parses a label selector name=value[,name=value]; ALL must
// match. Empty = track everything.
func trackIfFromFilter(filter string) func(docker.Container) bool {
	if filter == "" {
		return nil
	}
	var pairs [][2]string
	for _, part := range strings.Split(filter, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		pairs = append(pairs, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	if len(pairs) == 0 {
		return nil
	}
	return func(c docker.Container) bool {
		for _, p := range pairs {
			if c.Labels[p[0]] != p[1] {
				return false
			}
		}
		return true
	}
}

func containerInfos(in []collector.ContainerInfo) []transport.Container {
	out := make([]transport.Container, 0, len(in))
	for _, c := range in {
		out = append(out, transport.Container{
			ID:       c.ID,
			Name:     c.Name,
			App:      c.App,
			Replica:  c.Replica,
			Image:    c.Image,
			Started:  c.Started,
			CPU:      c.CPU,
			Mem:      c.Mem,
			MemLimit: c.MemLimit,
			SampleTS: c.SampleTS,
		})
	}
	return out
}

func resolveHostID(cfg config.Config, log *slog.Logger) string {
	if cfg.AgentHostID != "" {
		return cfg.AgentHostID
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	log.Warn("agent: cannot determine hostname, using random host_id")
	return randomID()
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		b = []byte(time.Now().String())
	}
	return fmt.Sprintf("%x", b)
}

func loadCAPool(caPath string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if caPath == "" {
		return pool, nil
	}
	data, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("agent: read CA cert %s: %w", caPath, err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("agent: parse CA cert %s", caPath)
	}
	return pool, nil
}
