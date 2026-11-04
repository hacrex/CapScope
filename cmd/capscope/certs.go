package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"capscope/internal/transport"
)

// runGenerateCerts generates a CA + agent + hub certificate bundle for mTLS
// agent mode. Output (default ./certs): ca.crt, agent.crt, agent.key,
// hub.crt, hub.key. Set CAPSCOPE_CERTS_DIR to change the directory and
// AGENT_HOST for the certificate common names.
//
// On the hub, pin the agent fingerprint: AGENT_HUB_PINNED_FINGERPRINTS=<agent sha256>
func runGenerateCerts(log *slog.Logger) error {
	dir := os.Getenv("CAPSCOPE_CERTS_DIR")
	if dir == "" {
		dir = "certs"
	}
	host := os.Getenv("AGENT_HOST")
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "localhost"
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caCert, caKey, agentCert, agentKey, hubCert, hubKey, err := transport.GenerateCertBundle(host)
	if err != nil {
		return fmt.Errorf("generate-certs: %w", err)
	}
	writeFile := func(name string, data []byte, perm os.FileMode) error {
		return os.WriteFile(filepath.Join(dir, name), data, perm)
	}
	if err := writeFile("ca.crt", caCert, 0o644); err != nil {
		return err
	}
	if err := writeFile("ca.key", caKey, 0o600); err != nil {
		return err
	}
	if err := writeFile("agent.crt", agentCert, 0o644); err != nil {
		return err
	}
	if err := writeFile("agent.key", agentKey, 0o600); err != nil {
		return err
	}
	if err := writeFile("hub.crt", hubCert, 0o644); err != nil {
		return err
	}
	if err := writeFile("hub.key", hubKey, 0o600); err != nil {
		return err
	}
	afp, err := transport.Fingerprint(agentCert)
	if err != nil {
		return err
	}
	hfp, err := transport.Fingerprint(hubCert)
	if err != nil {
		return err
	}
	log.Info("generate-certs: wrote CA + agent + hub certificate bundle",
		"dir", dir, "host", host, "agent_fingerprint", afp, "hub_fingerprint", hfp)
	fmt.Printf("Agent:     AGENT_CA_CERT=%s/ca.crt AGENT_CERT=%s/agent.crt AGENT_KEY=%s/agent.key\n", dir, dir, dir)
	fmt.Printf("Hub:       AGENT_HUB_PINNED_FINGERPRINTS=%s\n", afp)
	fmt.Printf("Hub cert:  %s/hub.crt %s/hub.key\n", dir, dir)
	return nil
}
