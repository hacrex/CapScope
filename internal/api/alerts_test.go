package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capscope/internal/alerts"
	"capscope/internal/auth"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"
)

// setupAlerts builds a server with an alert engine armed with one deadman rule.
func setupAlerts(t *testing.T, rulesFile string) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	cfg := config.Config{AuthUser: "u", AuthPass: "p", SnapshotKinds: map[string]bool{}}
	dk, _ := docker.New("tcp://127.0.0.1:1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := collector.NewManager(cfg, dk, st, h, log)
	eng := alerts.New(st, rulesFile, time.Minute, nil, log)
	as := auth.NewSessionManager(log)
	srv := httptest.NewServer(New(cfg, st, mgr, h, log, WithAlerts(eng), WithAuth(as)).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestAlertsEndpoints(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "alerts.json")
	body := `{"rules":[{"name":"silent","kind":"deadman","apps":["web"],"for":"5m"},{"name":"loop","kind":"restart_loop","count":3,"window":"10m"}]}`
	if err := writeFile(rulesPath, body); err != nil {
		t.Fatal(err)
	}
	srv, st := setupAlerts(t, rulesPath)

	// Rules endpoint reflects the loaded file.
	var meta struct {
		Rules []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"rules"`
		Enabled bool `json:"enabled"`
	}
	r := get(t, srv.URL+"/api/alerts/rules", true)
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("rules status %d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&meta)
	if !meta.Enabled || len(meta.Rules) != 2 {
		t.Fatalf("meta = %+v", meta)
	}

	// History endpoint is empty then reflects a stored alert.
	var hist struct {
		Alerts []store.Alert `json:"alerts"`
	}
	r2 := get(t, srv.URL+"/api/alerts?app=web", true)
	defer r2.Body.Close()
	_ = json.NewDecoder(r2.Body).Decode(&hist)
	if len(hist.Alerts) != 0 {
		t.Fatalf("want empty history, got %d", len(hist.Alerts))
	}

	if _, err := st.InsertAlert(store.Alert{TS: time.Now().UnixMilli(), Rule: "silent", App: "web", Kind: "deadman", Message: "web: no logs for 10m"}); err != nil {
		t.Fatal(err)
	}
	r3 := get(t, srv.URL+"/api/alerts?app=web", true)
	defer r3.Body.Close()
	_ = json.NewDecoder(r3.Body).Decode(&hist)
	if len(hist.Alerts) != 1 || hist.Alerts[0].Rule != "silent" {
		t.Fatalf("history = %+v", hist.Alerts)
	}
}

func TestAlertsTestEndpointSendsAndReports(t *testing.T) {
	// Point the webhook channel at a local capture server.
	var received []string
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = append(received, string(b))
	}))
	defer capture.Close()

	rulesPath := filepath.Join(t.TempDir(), "alerts.json")
	if err := writeFile(rulesPath, `{"rules":[]}`); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	cfg := config.Config{AuthUser: "u", AuthPass: "p", SnapshotKinds: map[string]bool{}}
	dk, _ := docker.New("tcp://127.0.0.1:1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := collector.NewManager(cfg, dk, st, h, log)
	eng := alerts.New(st, rulesPath, time.Minute, alerts.BuildChannels(capture.URL, "", "", "", ""), log)
	srv := httptest.NewServer(New(cfg, st, mgr, h, log, WithAlerts(eng)).Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/api/alerts/test", nil)
	loginIfNeeded(t)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("test endpoint status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Results map[string]string `json:"results"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Results["webhook"] != "sent" {
		t.Fatalf("results = %v", out.Results)
	}
	if len(received) != 1 || !strings.Contains(received[0], "CapScope test alert") {
		t.Fatalf("webhook payload = %v", received)
	}
}

func TestAlertsDisabledWithoutEngine(t *testing.T) {
	srv, _, _ := setup(t) // no WithAlerts
	r := get(t, srv.URL+"/api/alerts/rules", true)
	defer r.Body.Close()
	var meta struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&meta)
	if meta.Enabled {
		t.Error("alerts should report disabled without an engine")
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/alerts/test", nil)
	loginIfNeeded(t)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("test endpoint without engine should 400, got %d", resp.StatusCode)
	}
}

// writeFile is a tiny helper to keep the setup readable.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
