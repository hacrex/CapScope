package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capscope/internal/auth"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/docker"
	"capscope/internal/hub"
	"capscope/internal/store"

	"golang.org/x/crypto/bcrypt"

	"log/slog"
)

func setupShare(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	cfg := config.Config{AuthUser: "u", AuthPass: "p"}
	dk, _ := docker.New("tcp://127.0.0.1:1")
	mgr := collector.NewManager(cfg, dk, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	as := auth.NewSessionManager(slog.Default())
	// Seed the first user from AUTH_USER/AUTH_PASS
	hash, _ := bcrypt.GenerateFromPassword([]byte("p"), bcrypt.DefaultCost)
	_, _ = st.SeedFirstUser("u", string(hash))
	srv := httptest.NewServer(New(cfg, st, mgr, h, slog.Default(), WithAuth(as)).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func req(t *testing.T, method, url string, body string, auth string) *http.Request {
	t.Helper()
	r, _ := http.NewRequest(method, url, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	return r
}

// ---- share links (V10 expiring permalinks) ----

func TestShareCreateAndResolve(t *testing.T) {
	srv, _ := setupShare(t)
	tok := loginToken(t, srv.URL)

	body := `{"qs":"tab=logs&app=web&range=1d","hash":"crash=42","ttl":"7d"}`
	resp, err := http.DefaultClient.Do(req(t, "POST", srv.URL+"/api/share", body, tok))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("share create: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		URL       string `json:"url"`
		ExpiresMs int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.URL, "/s/") || len(out.URL) != len("/s/")+32 {
		t.Errorf("url = %q", out.URL)
	}
	if time.Until(time.UnixMilli(out.ExpiresMs)) < 6*24*time.Hour {
		t.Errorf("expires too soon: %v", time.Until(time.UnixMilli(out.ExpiresMs)))
	}

	// The capability token resolves back to the saved view without auth.
	// (http.DefaultClient follows redirects by default, so pin it down.)
	cli := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	redir, err := cli.Do(req(t, "GET", srv.URL+out.URL, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	redir.Body.Close()
	if redir.StatusCode != 302 {
		t.Fatalf("resolve: want 302, got %d", redir.StatusCode)
	}
	if loc := redir.Header.Get("Location"); !strings.Contains(loc, "tab=logs") || !strings.Contains(loc, "app=web") || !strings.Contains(loc, "#crash=42") {
		t.Errorf("location = %q", loc)
	}
}

func TestShareUnknownTokenIsGone(t *testing.T) {
	srv, _ := setupShare(t)
	redir, err := http.DefaultClient.Do(req(t, "GET", srv.URL+"/s/"+strings.Repeat("a", 32), "", ""))
	if err != nil {
		t.Fatal(err)
	}
	redir.Body.Close()
	if redir.StatusCode != 410 {
		t.Fatalf("unknown token: want 410, got %d", redir.StatusCode)
	}
}

func TestShareBadBodyRejected(t *testing.T) {
	srv, _ := setupShare(t)
	tok := loginToken(t, srv.URL)
	for _, body := range []string{`{}`, `{"qs":"","hash":""}`, `not-json`} {
		resp, err := http.DefaultClient.Do(req(t, "POST", srv.URL+"/api/share", body, tok))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("body %q: want 400, got %d", body, resp.StatusCode)
		}
	}
}

// ---- JSON export (Phase 6.2) ----

func seedLogsForAPI(t *testing.T, st *store.Store, app, line string, tsMs int64) {
	t.Helper()
	if err := st.InsertLogs([]store.LogRow{{TS: tsMs * 1_000_000, TSMs: tsMs, ContainerID: "c1", Container: app + "-1", App: app, Stream: "o", Level: "error", Line: line}}); err != nil {
		t.Fatal(err)
	}
}

func TestLogsExportJSON(t *testing.T) {
	srv, st := setupShare(t)
	tok := loginToken(t, srv.URL)
	now := time.Now()
	seedLogsForAPI(t, st, "web", "panic: oom", now.Add(-time.Minute).UnixMilli())
	seedLogsForAPI(t, st, "web", "continue running", now.UnixMilli())

	resp, err := http.DefaultClient.Do(req(t, "GET", srv.URL+"/api/logs/export?fmt=json&app=web", "", tok))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("content type = %q", resp.Header.Get("Content-Type"))
	}
	b, _ := io.ReadAll(resp.Body)
	var doc struct {
		Logs []struct {
			TS   int64  `json:"ts"`
			Line string `json:"line"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("body is not a JSON document: %v\n%s", err, b)
	}
	if len(doc.Logs) != 2 {
		t.Fatalf("rows = %d, want 2: %s", len(doc.Logs), b)
	}
	if !strings.Contains(string(b), "panic: oom") || !strings.Contains(string(b), "continue running") {
		t.Errorf("missing seeded lines: %s", b)
	}
}

// ---- self-contained HTML incident export (Phase 6.3) ----

func TestSnapshotIncidentHTML(t *testing.T) {
	srv, st := setupShare(t)
	tok := loginToken(t, srv.URL)
	now := time.Now()
	id, err := st.InsertSnapshot(store.Snapshot{
		TS:           now.UnixMilli(),
		DieTS:        now.UnixMilli(),
		Kind:         "crash",
		App:          "web",
		Container:    "web.1",
		Image:        "nginx:1.25",
		ExitCode:     1,
		OOMKilled:    true,
		RestartCount: 3,
		UptimeSec:    120,
		Summary:      "boom",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req(t, "GET", srv.URL+"/api/snapshots/"+itoa64(id)+"/incident.html", "", tok))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("content type = %q", resp.Header.Get("Content-Type"))
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	if !strings.Contains(s, "Incident") || !strings.Contains(s, "web") || !strings.Contains(s, "nginx:1.25") {
		t.Errorf("html missing expected content: %.200s", s)
	}
	if strings.Contains(s, "<script") || strings.Contains(s, `<link `) || strings.Contains(s, "http://") || strings.Contains(s, "https://") {
		t.Errorf("HTML export references external assets — must be self-contained: %.200s", s)
	}
}
