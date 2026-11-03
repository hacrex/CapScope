// Package hubagent serves the hub side of the agent↔hub link (Phase 4.4).
// It accepts mutual-TLS connections from agents, validates their pinned
// certificate fingerprint, and ingests LogBatch / MetricBatch / EventBatch /
// ContainerList into the store tagged by host_id. Events are fanned into the
// alert engine; log rows are published to the hub so live SSE tailing covers
// agent containers too.
package hubagent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"capscope/internal/alerts"
	"capscope/internal/hub"
	"capscope/internal/store"
	"capscope/internal/transport"
)

// Server ingests agent streams into one store, tagged by host_id.
type Server struct {
	st   *store.Store
	hub  *hub.Hub       // publish ingested log rows so SSE live-tail covers agent containers
	eng  *alerts.Engine // nil when alerting is disabled
	log  *slog.Logger
	pins map[string]bool // allowed agent cert fingerprints (empty = any CA-signed cert)
}

// New builds a hub agent server. pins are SHA-256 fingerprints (colon hex)
// of agent client certificates allowed to connect.
func New(st *store.Store, h *hub.Hub, eng *alerts.Engine, log *slog.Logger, pins ...string) *Server {
	p := map[string]bool{}
	for _, pin := range pins {
		if pin != "" {
			p[strings.ToLower(pin)] = true
		}
	}
	return &Server{st: st, hub: h, eng: eng, log: log, pins: p}
}

// Run listens on ln with mutual TLS and ingests agents until ctx is
// cancelled. hubCertPEM/hubKeyPEM are the hub's server cert/key PEM;
// caCertPEM is the CA used to verify agent client certs.
func (s *Server) Run(ctx context.Context, ln net.Listener, hubCertPEM, hubKeyPEM, caCertPEM string) error {
	cert, err := tls.X509KeyPair([]byte(hubCertPEM), []byte(hubKeyPEM))
	if err != nil {
		return fmt.Errorf("hubagent: load hub key pair: %w", err)
	}
	caPool := x509.NewCertPool()
	if caCertPEM != "" {
		if !caPool.AppendCertsFromPEM([]byte(caCertPEM)) {
			return errors.New("hubagent: failed to parse CA certificate")
		}
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	if len(s.pins) > 0 {
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("hubagent: no client certificate presented")
			}
			sum := sha256.Sum256(rawCerts[0])
			fp := fmt.Sprintf("%x", sum)
			if !s.pins[fp] {
				return fmt.Errorf("hubagent: agent fingerprint %s not pinned", fp)
			}
			return nil
		}
	}

	s.log.Info("hubagent: listening for agents", "listen", ln.Addr())

	for {
		raw, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			s.log.Warn("hubagent: accept failed", "err", err)
			continue
		}
		go s.handleConn(tls.Server(raw, tlsCfg), raw)
	}
}

// handleConn performs the TLS handshake, validates the client cert, then
// delegates to handleStream for the message loop.
func (s *Server) handleConn(conn *tls.Conn, raw net.Conn) {
	defer raw.Close()
	if err := conn.Handshake(); err != nil {
		s.log.Warn("hubagent: tls handshake failed", "err", err)
		return
	}
	cs := conn.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		s.log.Warn("hubagent: client presented no certificate")
		return
	}
	agentFP := fmt.Sprintf("%x", sha256.Sum256(cs.PeerCertificates[0].Raw))
	stream := transport.WrapConn(conn)
	s.handleStream(stream, agentFP)
}

// handleStream runs the message loop over an already-wrapped transport
// connection. It is exported for testing (net.Pipe).
func (s *Server) handleStream(stream *transport.Conn, agentFP string) {
	// Hello identifies this host.
	var hello transport.Hello
	if err := stream.Recv(&hello); err != nil {
		s.log.Warn("hubagent: read hello failed", "err", err)
		return
	}
	if hello.HostID == "" {
		s.log.Warn("hubagent: hello without host_id")
		return
	}
	if err := s.st.UpsertHost(hello.HostID, hello.Labels); err != nil {
		s.log.Warn("hubagent: register host failed", "host", hello.HostID, "err", err)
	}
	s.log.Info("hubagent: agent connected", "host", hello.HostID, "fingerprint", agentFP)
	// Acknowledge the hello so the agent knows the link is established.
	if err := stream.Send(transport.Ack{HostID: hello.HostID, OK: true}); err != nil {
		s.log.Warn("hubagent: hello ack failed", "host", hello.HostID, "err", err)
		return
	}

	// Message loop until the agent disconnects.
	for {
		var msg any
		if err := stream.Recv(&msg); err != nil {
			s.log.Info("hubagent: agent disconnected", "host", hello.HostID, "err", err)
			return
		}
		switch m := msg.(type) {
		case transport.LogBatch:
			s.ingestLogs(hello.HostID, m)
			_ = stream.Send(transport.Ack{HostID: hello.HostID, OK: true})
		case transport.MetricBatch:
			s.ingestMetrics(hello.HostID, m)
			_ = stream.Send(transport.Ack{HostID: hello.HostID, OK: true})
		case transport.EventBatch:
			s.ingestEvents(hello.HostID, m)
			_ = stream.Send(transport.Ack{HostID: hello.HostID, OK: true})
		case transport.ContainerList:
			s.ingestContainers(hello.HostID, m)
			_ = stream.Send(transport.Ack{HostID: hello.HostID, OK: true})
		case transport.Hello:
			// Re-hello: update labels.
			_ = s.st.UpsertHost(m.HostID, m.Labels)
			_ = stream.Send(transport.Ack{HostID: m.HostID, OK: true})
		case transport.Ack:
			// Unsolicited ack from agent; ignore.
		default:
			s.log.Warn("hubagent: unknown message", "host", hello.HostID, "type", fmt.Sprintf("%T", msg))
		}
	}
}

func (s *Server) ingestLogs(hostID string, b transport.LogBatch) {
	rows := make([]store.LogRow, 0, len(b.Rows))
	for i := range b.Rows {
		r := b.Rows[i]
		r.HostID = hostID // trust the authenticated connection, not the agent
		rows = append(rows, r)
	}
	if err := s.st.InsertLogs(rows); err != nil {
		s.log.Error("hubagent: insert logs failed", "host", hostID, "rows", len(rows), "err", err)
		return
	}
	if s.hub != nil {
		s.hub.Publish(rows)
	}
	s.log.Info("hubagent: ingested logs", "host", hostID, "rows", len(rows))
}

func (s *Server) ingestMetrics(hostID string, b transport.MetricBatch) {
	for i := range b.Samples {
		p := b.Samples[i]
		p.HostID = hostID
		if err := s.st.InsertMetric(p.Container, p.App, p); err != nil {
			s.log.Error("hubagent: insert metric failed", "host", hostID, "err", err)
			return
		}
	}
}

func (s *Server) ingestEvents(hostID string, b transport.EventBatch) {
	if s.eng == nil {
		return
	}
	for _, ev := range b.Events {
		if ev.Type != "container" {
			continue
		}
		app := ev.Actor.Attributes["app"]
		if app == "" {
			app = ev.Actor.Attributes["com.docker.compose.service"]
		}
		s.eng.FeedEvent(alerts.ContainerEvent{
			Action:    ev.Action,
			ID:        ev.Actor.ID,
			Container: ev.Actor.Attributes["name"],
			App:       app,
			ExitCode: func() int {
				if x, err := strconv.Atoi(ev.Actor.Attributes["exitCode"]); err == nil {
					return x
				}
				return 0
			}(),
			Attr: ev.Actor.Attributes,
			TS:   time.Unix(0, ev.TimeNano),
		})
	}
}

func (s *Server) ingestContainers(hostID string, b transport.ContainerList) {
	if err := s.st.UpsertHost(hostID, nil); err != nil {
		s.log.Warn("hubagent: register host failed", "host", hostID, "err", err)
	}
	var firstErr error
	for _, c := range b.Containers {
		if err := s.st.UpsertContainer(store.ContainerRow{
			ID:        c.ID,
			Name:      c.Name,
			App:       c.App,
			Image:     c.Image,
			Replica:   c.Replica,
			HostID:    hostID,
			State:     "running",
			FirstSeen: c.Started,
			LastSeen:  c.SampleTS,
		}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.log.Error("hubagent: upsert container failed", "host", hostID, "container", c.ID, "err", err)
		}
	}
	if firstErr != nil {
		s.log.Error("hubagent: upsert containers failed", "host", hostID, "err", firstErr)
	}
}
