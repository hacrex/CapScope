package transport

import (
	"capscope/internal/store"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

var _ = io.Copy
var _ = errors.Is

// TestGenerateCertPair produces a CA-signed agent cert with client-auth EKU
// and a separate CA cert; fingerprints differ.
func TestGenerateCertPair(t *testing.T) {
	agentCert, agentKey, caCert, err := GenerateCertPair("testhost")
	if err != nil {
		t.Fatal(err)
	}
	if len(agentCert) == 0 || len(agentKey) == 0 || len(caCert) == 0 {
		t.Fatal("empty PEM output")
	}
	fp, err := Fingerprint(agentCert)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 64 {
		t.Errorf("fingerprint = %q, want 64 hex chars", fp)
	}
	caFP, err := Fingerprint(caCert)
	if err != nil {
		t.Fatal(err)
	}
	if caFP == fp {
		t.Error("CA and agent cert fingerprints must differ")
	}
	block, _ := pem.Decode(agentCert)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	hasClientAuth := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		t.Error("agent cert missing clientAuth EKU")
	}
}

// TestFramedConnRoundTrip exchanges messages both directions over a TLS
// connection with mutual auth and pin enforcement.
func TestFramedConnRoundTrip(t *testing.T) {
	agentCert, agentKey, caCert, err := GenerateCertPair("client-host")
	if err != nil {
		t.Fatal(err)
	}
	agentFP, err := Fingerprint(agentCert)
	if err != nil {
		t.Fatal(err)
	}

	paths := CertPaths{CA: string(caCert), Cert: string(agentCert), Key: string(agentKey)}
	caPool, _, err := LoadCertificates(paths)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, err := ServerTLSConfig(paths, caPool, agentFP)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			conn := tls.Server(raw, srvCfg)
			go func() {
				defer raw.Close()
				if err := conn.Handshake(); err != nil {
					return
				}
				sc := WrapConn(conn)
				var hello Hello
				if err := sc.Recv(&hello); err != nil {
					return
				}
				if hello.HostID != "client-host" {
					return
				}
				_ = sc.Send(Ack{HostID: hello.HostID, OK: true})
			}()
		}
	}()

	cliCfg, err := ClientTLSConfig(paths, caPool, "localhost")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer raw.Close()
	cc := WrapConn(raw)

	if err := cc.Send(Hello{HostID: "client-host", Version: "test"}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	var ack Ack
	if err := cc.Recv(&ack); err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if !ack.OK || ack.HostID != "client-host" {
		t.Errorf("ack = %+v", ack)
	}
}

// TestPinnedFingerprintRejected: a client presenting a cert whose fingerprint
// doesn't match the hub's pin is refused at the TLS layer.
func TestPinnedFingerprintRejected(t *testing.T) {
	otherCert, otherKey, caCert, err := GenerateCertPair("evil-host")
	if err != nil {
		t.Fatal(err)
	}
	goodCert, goodKey, _, err := GenerateCertPair("good-host")
	if err != nil {
		t.Fatal(err)
	}
	goodFP, err := Fingerprint(goodCert)
	if err != nil {
		t.Fatal(err)
	}

	paths := CertPaths{CA: string(caCert), Cert: string(goodCert), Key: string(goodKey)}
	caPool, _, err := LoadCertificates(paths)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, err := ServerTLSConfig(paths, caPool, goodFP)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			conn := tls.Server(raw, srvCfg)
			go func() {
				defer raw.Close()
				_ = conn.Handshake() // expect failure for evil cert
			}()
		}
	}()

	cliPaths := CertPaths{CA: string(caCert), Cert: string(otherCert), Key: string(otherKey)}
	cliPool, _, err := LoadCertificates(cliPaths)
	if err != nil {
		t.Fatal(err)
	}
	cliCfg, err := ClientTLSConfig(cliPaths, cliPool, "evil-host")
	if err != nil {
		t.Fatal(err)
	}

	connErr := make(chan error, 1)
	go func() {
		_, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
		connErr <- err
	}()
	select {
	case err := <-connErr:
		if err == nil {
			t.Error("expected handshake to fail with wrong fingerprint, but it succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not complete")
	}
}

// TestLoadCertificatesMissingCert: missing identity file yields an error.
func TestLoadCertificatesMissingCert(t *testing.T) {
	_, _, err := LoadCertificates(CertPaths{CA: "", Cert: "/nonexistent.crt", Key: "/nonexistent.key"})
	if err == nil {
		t.Error("expected error loading missing cert")
	}
}

// TestFrameSplitsLargeBatches: a message larger than a single read is still
// delivered whole (frame length prefix handles arbitrary sizes up to max).
func TestFrameSplitsLargeBatches(t *testing.T) {
	agentCert, agentKey, caCert, err := GenerateCertPair("client-host")
	if err != nil {
		t.Fatal(err)
	}
	paths := CertPaths{CA: string(caCert), Cert: string(agentCert), Key: string(agentKey)}
	caPool, _, err := LoadCertificates(paths)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, err := ServerTLSConfig(paths, caPool, "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			conn := tls.Server(raw, srvCfg)
			go func() {
				defer raw.Close()
				if err := conn.Handshake(); err != nil {
					return
				}
				sc := WrapConn(conn)
				var got LogBatch
				if err := sc.Recv(&got); err != nil {
					return
				}
				_ = sc.Send(got)
			}()
		}
	}()

	cliPaths := CertPaths{CA: string(caCert), Cert: string(agentCert), Key: string(agentKey)}
	cliPool, _, err := LoadCertificates(cliPaths)
	if err != nil {
		t.Fatal(err)
	}
	cliCfg, err := ClientTLSConfig(cliPaths, cliPool, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer raw.Close()
	cc := WrapConn(raw)

	big := strings.Repeat("x", 100000) // larger than one typical TCP segment
	if err := cc.Send(LogBatch{HostID: "h", Rows: []store.LogRow{{Line: big}}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var got LogBatch
	if err := cc.Recv(&got); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0].Line != big {
		t.Error("large frame not delivered whole")
	}
}
