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
	as := auth.NewSessionManager(log)
	srv := httptest.NewServer(New(cfg, st, mgr, h, log, WithAlerts(eng), WithAuth(as)).Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/api/alerts/test", nil)
	loginIfNeeded(t, srv.URL)
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
	loginIfNeeded(t, srv.URL)
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

// TestAlertsRuleBuilder covers POST /api/alerts/rules (save) and
// POST /api/alerts/rules/validate (dry-run) — the alert rule builder API.
func TestAlertsRuleBuilder(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "alerts.json")
	if err := writeFile(rulesPath, `{"rules":[{"name":"silent","kind":"deadman","apps":["web"],"for":"5m"}]}`); err != nil {
		t.Fatal(err)
	}
	srv, _ := setupAlerts(t, rulesPath)
	token := loginToken(t, srv.URL)

	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Unauthenticated save must be rejected.
	req, _ := http.NewRequest("POST", srv.URL+"/api/alerts/rules", strings.NewReader(`{"rules":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 anonymous save, got %d", resp.StatusCode)
	}

	// Validate: good document passes, bad kind fails with a helpful message.
	if code, _ := post("/api/alerts/rules/validate", `{"rules":[{"name":"hot","kind":"metric","metric":"cpu","op":"gt","value":80,"for":"2m"}]}`); code != 200 {
		t.Fatalf("validate good rules: want 200, got %d", code)
	}
	if code, body := post("/api/alerts/rules/validate", `{"rules":[{"name":"x","kind":"bogus"}]}`); code != 400 || !strings.Contains(body, "bogus") {
		t.Fatalf("validate bad kind: want 400 mentioning kind, got %d %s", code, body)
	}
	if code, body := post("/api/alerts/rules/validate", `{"rules":[{"name":"y","kind":"deadman","for":"5 parsecs"}]}`); code != 400 || !strings.Contains(body, "bad duration") {
		t.Fatalf("validate bad duration: want 400, got %d %s", code, body)
	}

	// Save replaces the file; on-disk content round-trips through Load.
	newDoc := `{"rules":[{"name":"hot","kind":"metric","apps":["api","web"],"metric":"mem_pct","op":"gt","value":90,"for":"3m","repeat":"1h"},{"name":"crashloop","kind":"restart_loop","count":4,"window":"15m"}]}`
	if code, body := post("/api/alerts/rules", newDoc); code != 200 {
		t.Fatalf("save: want 200, got %d %s", code, body)
	}
	got, err := alerts.Load(rulesPath)
	if err != nil {
		t.Fatalf("saved file does not load: %v", err)
	}
	if len(got) != 2 || got[0].Name != "hot" || got[1].Kind != "restart_loop" || got[1].Count != 4 {
		t.Fatalf("unexpected saved rules: %+v", got)
	}
	if got[0].For.D() != 3*time.Minute || got[0].Repeat.D() != time.Hour {
		t.Fatalf("durations not persisted: %+v", got[0])
	}

	// GET /api/alerts/rules now reflects the newly saved rules immediately.
	r := get(t, srv.URL+"/api/alerts/rules", true)
	defer r.Body.Close()
	var meta struct {
		Rules []struct {
			Name string `json:"name"`
		} `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Rules) != 2 || meta.Rules[0].Name != "hot" {
		t.Fatalf("API did not pick up saved rules: %+v", meta.Rules)
	}

	// Saving an invalid document is rejected and leaves the file untouched.
	if code, _ := post("/api/alerts/rules", `{"rules":[{"name":"","kind":"deadman"}]}`); code != 400 {
		t.Fatalf("save invalid: want 400, got %d", code)
	}
	if again, _ := alerts.Load(rulesPath); len(again) != 2 {
		t.Fatalf("file changed after rejected save: %+v", again)
	}
}
