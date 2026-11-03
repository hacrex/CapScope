package hubagent

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"capscope/internal/alerts"
	"capscope/internal/docker"
	"capscope/internal/store"
	"capscope/internal/transport"
)

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fingerprintStr returns the colon-hex SHA-256 fingerprint of a cert PEM.
func fingerprintStr(t *testing.T, certPEM []byte) string {
	t.Helper()
	fp, err := transport.Fingerprint(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// testStore opens a temp store (migrations apply the host_id columns).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ---- Direct sink method tests (no network) ----

func TestServerIngestLogsDirect(t *testing.T) {
	st := testStore(t)
	s := New(st, nil, nil, slogDiscard())
	ctx := context.Background()

	s.ingestLogs("node-1", transport.LogBatch{
		HostID: "node-1",
		Rows:   []store.LogRow{{ContainerID: "c1", Container: "web.1", App: "web", Stream: "o", Level: "info", Line: "direct log"}},
	})
	list, err := st.QueryLogs(ctx, store.LogQuery{HostID: "node-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Line != "direct log" {
		t.Fatalf("log row = %+v", list)
	}
}

func TestServerIngestMetricsDirect(t *testing.T) {
	st := testStore(t)
	s := New(st, nil, nil, slogDiscard())
	ctx := context.Background()

	s.ingestMetrics("node-1", transport.MetricBatch{
		HostID:  "node-1",
		Samples: []store.MetricPoint{{Container: "c1", App: "web", CPU: 50, Mem: 100, MemLimit: 1000}},
	})
	pts, err := st.QueryMetrics(ctx, "web", "", "node-1", 0, 0, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("expected 1 metric point, got %d", len(pts))
	}
}

func TestServerIngestContainersDirect(t *testing.T) {
	st := testStore(t)
	s := New(st, nil, nil, slogDiscard())
	ctx := context.Background()

	s.ingestContainers("node-1", transport.ContainerList{
		HostID:     "node-1",
		Containers: []transport.Container{{ID: "c1", Name: "web.1", App: "web", Image: "nginx"}},
	})
	cons, err := st.ListContainers(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 1 || cons[0].HostID != "node-1" || cons[0].App != "web" {
		t.Fatalf("containers = %+v", cons)
	}
	hosts, err := st.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].HostID != "node-1" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestServerIngestEventsDirect(t *testing.T) {
	st := testStore(t)
	eng := alerts.New(st, t.TempDir()+"/alerts.json", time.Minute, nil, slogDiscard())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)
	if err := eng.SaveRules([]alerts.Rule{{Name: "die", Kind: alerts.KindEvent, Event: "die"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertContainer(store.ContainerRow{ID: "c1", Name: "web.1", App: "web", Image: "nginx", HostID: "node-1", State: "running", FirstSeen: 1, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	s := New(st, nil, eng, slogDiscard())

	s.ingestEvents("node-1", transport.EventBatch{
		HostID: "node-1",
		Events: []docker.Event{
			func() docker.Event {
				e := docker.Event{Type: "container", Action: "die", TimeNano: time.Now().UnixNano()}
				e.Actor.ID = "c1"
				e.Actor.Attributes = map[string]string{"name": "web.1", "exitCode": "1", "com.docker.compose.service": "web"}
				return e
			}(),
		},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := st.ListAlerts(context.Background(), "web", 10)
		if err == nil {
			for _, a := range list {
				if a.Kind == "event" && strings.Contains(a.Message, "exit code 1") {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no event alert fired from ingested die event")
}

// ---- testRoundTripServer starts the hub agent server on a random port ----

func testRoundTripServer(t *testing.T, st *store.Store, eng *alerts.Engine, pin string, caCert, hubCert, hubKey []byte) string {
	t.Helper()
	s := New(st, nil, eng, slog.Default(), pin)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	go func() {
		_ = s.Run(ctx, ln, string(hubCert), string(hubKey), string(caCert))
	}()
	time.Sleep(50 * time.Millisecond)
	return ln.Addr().String()
}

// dialAgent connects a mutual-TLS agent to addr and returns a framed conn.
func dialAgent(t *testing.T, addr string, caCert, agentCert, agentKey []byte) (*transport.Conn, error) {
	t.Helper()
	paths := transport.CertPaths{CA: string(caCert), Cert: string(agentCert), Key: string(agentKey)}
	pool, _, err := transport.LoadCertificates(paths)
	if err != nil {
		return nil, err
	}
	cliCfg, err := transport.ClientTLSConfig(paths, pool, "localhost")
	if err != nil {
		return nil, err
	}
	raw, err := tls.Dial("tcp", addr, cliCfg)
	if err != nil {
		return nil, err
	}
	return transport.WrapConn(raw), nil
}

func TestFullRoundTripIngestsLogs(t *testing.T) {
	st := testStore(t)
	eng := alerts.New(st, "", time.Minute, nil, slogDiscard())
	caCert, _, agentCert, agentKey, hubCert, hubKey := testBundle(t)
	pin := fingerprintStr(t, agentCert)

	addr := testRoundTripServer(t, st, eng, pin, caCert, hubCert, hubKey)

	conn, err := dialAgent(t, addr, caCert, agentCert, agentKey)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer conn.Close()

	// Hello.
	if err := conn.Send(transport.Hello{HostID: "node-1", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	var ack transport.Ack
	if err := conn.Recv(&ack); err != nil || !ack.OK {
		t.Fatalf("hello ack = %+v err=%v", ack, err)
	}
	_ = hubCert
	_ = hubKey

	// Log batch - send and check persistence directly (don't wait for Ack).
	if err := conn.Send(transport.LogBatch{
		HostID: "node-1",
		Rows:   []store.LogRow{{ContainerID: "c1", Container: "web.1", App: "web", Stream: "o", Level: "info", Line: "remote log"}},
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := st.QueryLogs(ctx, store.LogQuery{HostID: "node-1"})
		if err == nil && len(list) > 0 && list[0].Line == "remote log" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("remote log not persisted with host_id node-1")
}

func TestFullRoundTripRejectsUnpinnedAgent(t *testing.T) {
	st := testStore(t)
	caCert, caKey, agentA, _, hubCert, hubKey := testBundle(t)
	pin := fingerprintStr(t, agentA)
	agentB, agentBKey, err := transport.SignLeaf(string(caCert), string(caKey), "agent-b")
	if err != nil {
		t.Fatal(err)
	}

	addr := testRoundTripServer(t, st, nil, pin, caCert, hubCert, hubKey)

	// Unpinned agent (same CA, different fingerprint): the handshake should
	// fail. tls.Dial may or may not surface the rejection immediately, but
	// either way no data must be ingested from an unpinned agent.
	conn, err := dialAgent(t, addr, caCert, agentB, agentBKey)
	if err == nil {
		// Best effort: send a Hello; the server closed its side after
		// rejecting the handshake, so this may or may not error.
		_ = conn.Send(transport.Hello{HostID: "agent-b", Version: "test"})
		conn.Close()
	}
	// No host may have been registered by an unpinned agent.
	time.Sleep(200 * time.Millisecond)
	hosts, err := st.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		if h.HostID == "agent-b" {
			t.Fatal("unpinned agent host was registered")
		}
	}
}

func testBundle(t *testing.T) (caCert, caKey, agentCert, agentKey, hubCert, hubKey []byte) {
	t.Helper()
	cc, ck, ac, ak, hc, hk, err := transport.GenerateCertBundle("testhost")
	if err != nil {
		t.Fatal(err)
	}
	return cc, ck, ac, ak, hc, hk
}
