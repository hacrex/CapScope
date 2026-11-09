package transport

// Transport: mutual-TLS framed streaming connection for the agent↔hub link.
// Frames are length-prefixed gob messages (see capscope.proto). Certificate
// generation produces a CA + agent cert pair; the hub pins the agent cert's
// SHA-256 fingerprint so only an authorized agent can connect (private
// certificate semantics, like Dozzle's DOZZLE_CERT_PEM pair).

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/gob"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"reflect"
	"sync"
	"time"
)

// gobTypes holds the concrete message types registered for decoding through
// the stream's any parameter. Registered in init().
var gobTypes = []any{
	Hello{},
	LogBatch{},
	MetricBatch{},
	EventBatch{},
	ContainerList{},
	Container{},
	Ack{},
}

func init() {
	for _, v := range gobTypes {
		gob.Register(v)
	}
	gob.Register(time.Time{})
}

const (
	// frameMaxBytes caps a single gob frame (4 MiB). Larger batches are split
	// by the sender.
	frameMaxBytes = 4 << 20
	// frameHeader is the 4-byte big-endian length prefix.
	frameHeaderLen = 4
)

// CertPaths are the locations the agent/hub read their identity from. Empty
// means "generate on first run" (agent side) or "no client auth required"
// (hub side, not recommended).
type CertPaths struct {
	CA   string // CA certificate PEM (trust anchor)
	Cert string // certificate PEM
	Key  string // private key PEM
}

// Fingerprint returns the SHA-256 fingerprint (colon hex) of a certificate
// PEM block.
func Fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("transport: no PEM block found in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("transport: parse certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	return fmt.Sprintf("%x", sum), nil
}

// LoadCertificates loads a CA trust pool and a client identity from PEM
// content (callers read the files themselves). caPEM may be empty.
func LoadCertificates(paths CertPaths) (*x509.CertPool, tls.Certificate, error) {
	caPool := x509.NewCertPool()
	if paths.CA != "" {
		if !caPool.AppendCertsFromPEM([]byte(paths.CA)) {
			return nil, tls.Certificate{}, errors.New("transport: failed to parse CA certificate")
		}
	}
	cert, err := tls.X509KeyPair([]byte(paths.Cert), []byte(paths.Key))
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("transport: load key pair: %w", err)
	}
	return caPool, cert, nil
}

// ServerTLSConfig builds a mutual-TLS config for the hub. It requires and
// verifies client certs against caPool and pins the expected agent fingerprint
// (if non-empty) so only an authorized agent connects.
func ServerTLSConfig(certPaths CertPaths, caPool *x509.CertPool, pinnedFingerprint string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(certPaths.Cert), []byte(certPaths.Key))
	if err != nil {
		return nil, fmt.Errorf("transport: load server key pair: %w", err)
	}
	c := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	if pinnedFingerprint != "" {
		c.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("transport: no client certificate presented")
			}
			sum := sha256.Sum256(rawCerts[0])
			if fmt.Sprintf("%x", sum) != pinnedFingerprint {
				return fmt.Errorf("transport: pinned fingerprint mismatch: got %x, want %s", sum, pinnedFingerprint)
			}
			return nil
		}
	}
	return c, nil
}

// ClientTLSConfig builds a mutual-TLS config for the agent. It trusts caPool
// as the server's CA and presents the agent's identity.
func ClientTLSConfig(certPaths CertPaths, caPool *x509.CertPool, serverName string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(certPaths.Cert), []byte(certPaths.Key))
	if err != nil {
		return nil, fmt.Errorf("transport: load agent key pair: %w", err)
	}
	c := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}
	if serverName != "" {
		c.ServerName = serverName
	}
	return c, nil
}

// GenerateCertBundle creates one self-signed CA and signs both an agent
// certificate and a hub certificate with it. All certs are valid for 10
// years. Returns (caCert, caKey, agentCert, agentKey, hubCert, hubKey) as PEM
// blocks. The hub pins the agent fingerprint (AGENT_HUB_PINNED_FINGERPRINTS)
// so only authorized agents connect.
func GenerateCertBundle(host string) (caCert, caKey, agentCert, agentKey, hubCert, hubKey []byte, err error) {
	caRawKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "capscope-agent-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caRawKey.PublicKey, caRawKey)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	caCert = pemEncode(caDER, "CERTIFICATE")

	// Shared end-entity template: valid as both client (agent) and server (hub) identity.
	leafTmpl := func(commonName string) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano() % (1 << 62)),
			Subject:      pkix.Name{CommonName: commonName},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
			DNSNames:     []string{commonName, "localhost"},
		}
	}
	signLeaf := func(tmpl *x509.Certificate) (certPEM []byte, key *rsa.PrivateKey, err error) {
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caTmpl, &key.PublicKey, caRawKey)
		if err != nil {
			return nil, nil, err
		}
		return pemEncode(der, "CERTIFICATE"), key, nil
	}
	var agentRawKey, hubRawKey *rsa.PrivateKey
	agentCert, agentRawKey, err = signLeaf(leafTmpl("capscope-agent/" + host))
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	hubCert, hubRawKey, err = signLeaf(leafTmpl("capscope-hub/" + host))
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	return caCert, pemEncodeKey(caRawKey), agentCert, pemEncodeKey(agentRawKey), hubCert, pemEncodeKey(hubRawKey), nil
}

// GenerateCertPair is kept for backward compatibility: it returns the agent
// half of a fresh bundle (agent cert/key + CA). Prefer GenerateCertBundle.
func GenerateCertPair(host string) ([]byte, []byte, []byte, error) {
	caCert, _, agentCert, agentKey, _, _, err := GenerateCertBundle(host)
	return agentCert, agentKey, caCert, err
}

// SignLeaf signs a new end-entity certificate (clientAuth + serverAuth) under
// the given CA, useful for minting additional agent certs that share one trust
// anchor. Returns (leafCertPEM, leafKeyPEM).
func SignLeaf(caCertPEM, caKeyPEM, commonName string) ([]byte, []byte, error) {
	caBlock, _ := pem.Decode([]byte(caCertPEM))
	if caBlock == nil {
		return nil, nil, errors.New("transport: cannot parse CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: parse CA cert: %w", err)
	}
	caKeyBlock, _ := pem.Decode([]byte(caKeyPEM))
	if caKeyBlock == nil {
		return nil, nil, errors.New("transport: cannot parse CA key PEM")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: parse CA key: %w", err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() % (1 << 62)),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName, "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode(der, "CERTIFICATE"), pemEncodeKey(leafKey), nil
}

func pemEncode(der []byte, typ string) []byte {
	var b bytes.Buffer
	_ = pem.Encode(&b, &pem.Block{Type: typ, Bytes: der})
	return b.Bytes()
}

func pemEncodeKey(key *rsa.PrivateKey) []byte {
	var b bytes.Buffer
	_ = pem.Encode(&b, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return b.Bytes()
}

// Conn is a length-prefixed gob frame stream over a net.Conn.
// Send is concurrency-safe (agent-side stats/log loops call it from multiple
// goroutines); Recv is single-reader (one goroutine per connection).
type Conn struct {
	conn   net.Conn
	sendMu sync.Mutex
}

// WrapConn wraps an existing connection (e.g. a TLS conn) in a frame stream.
func WrapConn(c net.Conn) *Conn { return &Conn{conn: c} }

// Send writes one gob-encoded message, prefixed with a 4-byte length.
// The message is encoded through a pointer to the interface so gob writes
// concrete type info, allowing the receiver to decode into any.
func (c *Conn) Send(m any) error {
	ptr := &m
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(ptr); err != nil {
		return err
	}
	data := buf.Bytes()
	if len(data) > frameMaxBytes {
		return fmt.Errorf("transport: frame %d bytes exceeds max %d", len(data), frameMaxBytes)
	}
	var hdr [frameHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	return c.sendFrame(hdr[:], data)
}

// sendFrame writes header+payload as one locked, atomic unit so concurrent
// Send calls from the agent's log/stats/event loops cannot interleave frames
// (e.g. header-A, header-B, body-B, body-A).
func (c *Conn) sendFrame(hdr []byte, data []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if _, err := c.conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.conn.Write(data)
	return err
}

// Recv reads one gob-encoded message into m.
func (c *Conn) Recv(m any) error {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > frameMaxBytes {
		return fmt.Errorf("transport: frame %d bytes exceeds max %d", n, frameMaxBytes)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(c.conn, data); err != nil {
		return err
	}
	// Decode into any first (gob writes concrete type info when encoding
	// through a pointer to interface), then assign into m.
	var decoded any
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&decoded); err != nil {
		return err
	}
	// decoded holds a concrete value; copy it into the caller's interface.
	rv := reflect.ValueOf(m).Elem()
	// Guard against a desynchronized stream (decoded message is not of the
	// type the caller expected) so one bad frame can't crash the whole hub.
	dv := reflect.ValueOf(decoded)
	if !dv.Type().AssignableTo(rv.Type()) {
		return fmt.Errorf("transport: unexpected message type %s, want %s", dv.Type(), rv.Type())
	}
	rv.Set(dv)
	return nil
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }

// RemoteAddr returns the remote address.
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }
