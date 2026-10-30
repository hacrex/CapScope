package alerts

import (
	"context"
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

	"capscope/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// seedApp registers an app so knownApps sees it (empty log means no rows).
func seedApp(t *testing.T, st *store.Store, app string) {
	t.Helper()
	if err := st.UpsertContainer(store.ContainerRow{ID: "id-" + app, Name: app + "-1", App: app, Image: "img", State: "running", FirstSeen: 1, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
}

func seedLogs(t *testing.T, st *store.Store, app, line string, tsMs ...int64) {
	t.Helper()
	var rows []store.LogRow
	for _, ms := range tsMs {
		rows = append(rows, store.LogRow{
			TS: ms * 1_000_000, TSMs: ms, ContainerID: "c1", Container: app + "-1", App: app,
			Stream: "o", Level: "info", Line: line,
		})
	}
	if err := st.InsertLogs(rows); err != nil {
		t.Fatal(err)
	}
}

func seedMetrics(t *testing.T, st *store.Store, app string, pts ...store.MetricPoint) {
	t.Helper()
	for _, p := range pts {
		if err := st.InsertMetric("c1", app, p); err != nil {
			t.Fatal(err)
		}
	}
}

func seedDeaths(t *testing.T, st *store.Store, app string, tsMs ...int64) {
	t.Helper()
	for _, ms := range tsMs {
		if _, err := st.InsertSnapshot(store.Snapshot{TS: ms, DieTS: ms, Kind: "crash", ContainerID: "c1", Container: app + "-1", App: app, ExitCode: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- rules file loading ----

func TestLoadMissingFileYieldsNoRules(t *testing.T) {
	rules, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("want 0 rules, got %d", len(rules))
	}
}

func TestLoadRulesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	body := `{"rules":[
	  {"name":"silent","kind":"deadman","apps":["api"],"for":"5m"},
	  {"name":"loop","kind":"restart_loop","count":4,"window":"10m"},
	  {"name":"mem","kind":"metric","apps":["api"],"metric":"mem_pct","op":"gt","value":85,"for":"5m"},
	  {"name":"err500","kind":"log_pattern","apps":["api"],"pattern":"status=5\\d\\d","hits":3,"window":"15m"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("want 4 rules, got %d", len(rules))
	}
	if rules[0].For.D() != 5*time.Minute {
		t.Errorf("deadman for = %v, want 5m", rules[0].For.D())
	}
	if rules[1].Count != 4 || rules[1].Window.D() != 10*time.Minute {
		t.Errorf("restart_loop = %d/%v, want 4/10m", rules[1].Count, rules[1].Window.D())
	}
	if rules[2].Metric != "mem_pct" || rules[2].Value != 85 {
		t.Errorf("metric rule = %s/%v", rules[2].Metric, rules[2].Value)
	}
	if rules[3].re == nil || !rules[3].re.MatchString("status=503") {
		t.Error("log_pattern regex not compiled/matching")
	}
	// Defaults applied.
	if rules[0].Repeat.D() != 30*time.Minute {
		t.Errorf("default repeat = %v, want 30m", rules[0].Repeat.D())
	}
}

func TestLoadRejectsBadRule(t *testing.T) {
	for _, body := range []string{
		`{"rules":[{"name":"x","kind":"nope"}]}`,
		`{"rules":[{"name":"x","kind":"metric","metric":"mem"}]}`,     // bad op
		`{"rules":[{"name":"x","kind":"log_pattern","pattern":"("}]}`, // bad regex
		`{"rules":[{"kind":"deadman"}]}`,                              // no name
	} {
		path := filepath.Join(t.TempDir(), "a.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("body %s should have failed", body)
		}
	}
}

func TestDurJSONRoundTrip(t *testing.T) {
	var d Dur
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil {
		t.Fatal(err)
	}
	if d.D() != 90*time.Second {
		t.Fatalf("got %v", d.D())
	}
	if err := d.UnmarshalJSON([]byte(`"bogus"`)); err == nil {
		t.Fatal("want error for bogus duration")
	}
}

// ---- evaluation ----

func newEngine(t *testing.T, st *store.Store, rules []Rule, now time.Time, chans ...Channel) *Engine {
	t.Helper()
	// Write rules to a file so New exercises the load path.
	path := filepath.Join(t.TempDir(), "alerts.json")
	var sb strings.Builder
	sb.WriteString(`{"rules":[`)
	for i, r := range rules {
		if i > 0 {
			sb.WriteString(",")
		}
		b, _ := json.Marshal(r)
		sb.Write(b)
	}
	sb.WriteString("]}")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(st, path, time.Minute, chans, discardLog(), WithClock(func() time.Time { return now }))
}

func TestDeadmanFiresAndSilences(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	// Last log 10 minutes ago; rule fires after 5 minutes of silence.
	seedLogs(t, st, "api", "hello", now.Add(-10*time.Minute).UnixMilli())

	e := newEngine(t, st, []Rule{{Name: "silent", Kind: KindDeadman, Apps: []string{"api"}, For: Dur(5 * time.Minute)}}, now)

	fired := e.Evaluate(context.Background())
	if len(fired) != 1 {
		t.Fatalf("want 1 firing, got %d", len(fired))
	}
	if !strings.Contains(fired[0].Message, "no logs") {
		t.Errorf("message = %q", fired[0].Message)
	}
	// Immediately after, the repeat window keeps it quiet.
	if again := e.Evaluate(context.Background()); len(again) != 0 {
		t.Fatalf("want silence, got %d firings", len(again))
	}
}

func TestDeadmanQuietWhenLogging(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedLogs(t, st, "api", "fresh", now.Add(-30*time.Second).UnixMilli())

	e := newEngine(t, st, []Rule{{Name: "silent", Kind: KindDeadman, Apps: []string{"api"}, For: Dur(5 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing, got %d", len(fired))
	}
}

func TestDeadmanSkipsAppThatNeverLogged(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api") // registered but no log rows at all
	now := time.Now()
	e := newEngine(t, st, []Rule{{Name: "silent", Kind: KindDeadman, Apps: []string{"api"}, For: Dur(time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing for never-logged app, got %d", len(fired))
	}
}

func TestRestartLoopFires(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedDeaths(t, st, "api",
		now.Add(-9*time.Minute).UnixMilli(),
		now.Add(-6*time.Minute).UnixMilli(),
		now.Add(-3*time.Minute).UnixMilli(),
		now.Add(-1*time.Minute).UnixMilli(),
	)
	e := newEngine(t, st, []Rule{{Name: "loop", Kind: KindRestartLoop, Apps: []string{"api"}, Count: 3, Window: Dur(10 * time.Minute)}}, now)
	fired := e.Evaluate(context.Background())
	if len(fired) != 1 {
		t.Fatalf("want 1 firing, got %d", len(fired))
	}
	if !strings.Contains(fired[0].Message, "4 restarts") {
		t.Errorf("message = %q", fired[0].Message)
	}
}

func TestRestartLoopQuietBelowCount(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedDeaths(t, st, "api", now.Add(-2*time.Minute).UnixMilli(), now.Add(-1*time.Minute).UnixMilli())
	e := newEngine(t, st, []Rule{{Name: "loop", Kind: KindRestartLoop, Apps: []string{"api"}, Count: 3, Window: Dur(10 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing, got %d", len(fired))
	}
}

func TestMetricThresholdWithDuration(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	// Three samples all above 85% of the limit inside the 5m window.
	seedMetrics(t, st, "api",
		store.MetricPoint{TS: now.Add(-4 * time.Minute).UnixMilli(), Mem: 90, MemLimit: 100},
		store.MetricPoint{TS: now.Add(-2 * time.Minute).UnixMilli(), Mem: 95, MemLimit: 100},
		store.MetricPoint{TS: now.Add(-30 * time.Second).UnixMilli(), Mem: 92, MemLimit: 100},
	)
	e := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 85, For: Dur(5 * time.Minute)}}, now)
	fired := e.Evaluate(context.Background())
	if len(fired) != 1 {
		t.Fatalf("want 1 firing, got %d", len(fired))
	}
	if !strings.Contains(fired[0].Message, "mem_pct") {
		t.Errorf("message = %q", fired[0].Message)
	}
}

func TestMetricThresholdBrokenInsideWindowStaysQuiet(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	// One cool sample inside the window breaks the "for 5m" condition.
	seedMetrics(t, st, "api",
		store.MetricPoint{TS: now.Add(-4 * time.Minute).UnixMilli(), Mem: 95, MemLimit: 100},
		store.MetricPoint{TS: now.Add(-2 * time.Minute).UnixMilli(), Mem: 50, MemLimit: 100}, // cooled
		store.MetricPoint{TS: now.Add(-30 * time.Second).UnixMilli(), Mem: 95, MemLimit: 100},
	)
	e := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 85, For: Dur(5 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing, got %d", len(fired))
	}
}

func TestMetricStaleDataStaysQuiet(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	// Samples exist but the newest is an hour old; the app is gone.
	seedMetrics(t, st, "api", store.MetricPoint{TS: now.Add(-time.Hour).UnixMilli(), Mem: 99, MemLimit: 100})
	e := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 85, For: Dur(5 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing on stale data, got %d", len(fired))
	}
}

func TestMetricMemPctIgnoresNoLimitSamples(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	// Only samples without a memory limit: percentage is undefined, no fire.
	seedMetrics(t, st, "api",
		store.MetricPoint{TS: now.Add(-2 * time.Minute).UnixMilli(), Mem: 99, MemLimit: 0},
		store.MetricPoint{TS: now.Add(-30 * time.Second).UnixMilli(), Mem: 99, MemLimit: 0},
	)
	e := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 85, For: Dur(5 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing, got %d", len(fired))
	}
}

func TestLogPatternFires(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedLogs(t, st, "api", "GET / 500", now.Add(-5*time.Minute).UnixMilli(), now.Add(-3*time.Minute).UnixMilli(), now.Add(-1*time.Minute).UnixMilli())
	seedLogs(t, st, "api", "GET / 200", now.Add(-2*time.Minute).UnixMilli())

	e := newEngine(t, st, []Rule{{Name: "e5xx", Kind: KindLogPattern, Apps: []string{"api"}, Pattern: `status=5\d\d|\b500\b`, Hits: 3, Window: Dur(10 * time.Minute)}}, now)
	fired := e.Evaluate(context.Background())
	if len(fired) != 1 {
		t.Fatalf("want 1 firing, got %d", len(fired))
	}
	if !strings.Contains(fired[0].Message, "3 lines") {
		t.Errorf("message = %q", fired[0].Message)
	}
}

func TestLogPatternQuietBelowHits(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedLogs(t, st, "api", "boom 500", now.Add(-1*time.Minute).UnixMilli())
	e := newEngine(t, st, []Rule{{Name: "e5xx", Kind: KindLogPattern, Apps: []string{"api"}, Pattern: `\b500\b`, Hits: 3, Window: Dur(10 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("want no firing, got %d", len(fired))
	}
}

func TestAllAppsExpansion(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	seedApp(t, st, "web")
	now := time.Now()
	// Both apps went silent.
	seedLogs(t, st, "api", "x", now.Add(-time.Hour).UnixMilli())
	seedLogs(t, st, "web", "x", now.Add(-time.Hour).UnixMilli())

	e := newEngine(t, st, []Rule{{Name: "silent", Kind: KindDeadman, For: Dur(5 * time.Minute)}}, now) // no Apps => all
	fired := e.Evaluate(context.Background())
	if len(fired) != 2 {
		t.Fatalf("want 2 firings (one per app), got %d", len(fired))
	}
}

func TestEvaluateRecordsAlertsInStore(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedLogs(t, st, "api", "x", now.Add(-time.Hour).UnixMilli())

	e := newEngine(t, st, []Rule{{Name: "silent", Kind: KindDeadman, Apps: []string{"api"}, For: Dur(5 * time.Minute)}}, now)
	e.Evaluate(context.Background())

	list, err := st.ListAlerts(context.Background(), "api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 stored alert, got %d", len(list))
	}
	if list[0].Rule != "silent" || list[0].Kind != KindDeadman {
		t.Errorf("stored alert = %+v", list[0])
	}
}

// ---- channels ----

func TestWebhookPayloadShape(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad json: %v", err)
		}
	}))
	defer srv.Close()

	if err := NewWebhook(srv.URL).Send(context.Background(), "hello world"); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "hello world" {
		t.Errorf("payload = %v", got)
	}
}

// roundTripFunc lets a test intercept the HTTP request a notifier builds,
// so the endpoint URL and payload can be asserted without a live server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubTransport(c *http.Client, fn func(req *http.Request) (*http.Response, error)) {
	c.Transport = roundTripFunc(fn)
}

func okResp() *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}
}

func TestTelegramBuildsBotAPIRequest(t *testing.T) {
	var gotPath, gotChat, gotText string
	c := NewTelegram("token123", "chat456")
	stubTransport(c.Client, func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		body, _ := io.ReadAll(req.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		gotChat, gotText = m["chat_id"], m["text"]
		return okResp(), nil
	})
	if err := c.Send(context.Background(), "tg msg"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bottoken123/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	if gotChat != "chat456" || gotText != "tg msg" {
		t.Errorf("payload = chat_id=%q text=%q", gotChat, gotText)
	}
}

func TestSlackAndDiscordPayloadShape(t *testing.T) {
	for _, mk := range []struct {
		name string
		url  string
		key  string
	}{
		{"slack", "", "text"},
		{"discord", "", "content"},
	} {
		var got map[string]string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
		}))
		var c Channel
		if mk.name == "slack" {
			c = NewSlack(srv.URL)
		} else {
			c = NewDiscord(srv.URL)
		}
		if err := c.Send(context.Background(), "msg"); err != nil {
			t.Fatalf("%s: %v", mk.name, err)
		}
		if got[mk.key] != "msg" {
			t.Errorf("%s payload = %v", mk.name, got)
		}
		srv.Close()
	}
}

func TestNotifierHTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := NewWebhook(srv.URL).Send(context.Background(), "x"); err == nil {
		t.Fatal("want error on HTTP 500")
	}
}

func TestBuildChannelsSkipsEmpty(t *testing.T) {
	chans := BuildChannels("", "", "", "", "")
	if len(chans) != 0 {
		t.Fatalf("want 0 channels, got %d", len(chans))
	}
	chans = BuildChannels("http://x", "tok", "chat", "http://s", "http://d")
	if len(chans) != 4 {
		t.Fatalf("want 4 channels, got %d", len(chans))
	}
	names := map[string]bool{}
	for _, c := range chans {
		names[c.Name] = true
	}
	for _, want := range []string{"webhook", "telegram", "slack", "discord"} {
		if !names[want] {
			t.Errorf("missing channel %s", want)
		}
	}
}

func TestEngineNotifiesChannelsOnFire(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedLogs(t, st, "api", "x", now.Add(-time.Hour).UnixMilli())

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()

	e := newEngine(t, st, []Rule{{Name: "silent", Kind: KindDeadman, Apps: []string{"api"}, For: Dur(5 * time.Minute)}}, now, NewWebhook(srv.URL))
	fired := e.Evaluate(context.Background())
	if len(fired) != 1 {
		t.Fatalf("want 1 firing, got %d", len(fired))
	}
	if hits != 1 {
		t.Fatalf("channel hits = %d, want 1", hits)
	}
}

func TestEngineReloadsChangedFile(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedLogs(t, st, "api", "x", now.Add(-time.Hour).UnixMilli())

	path := filepath.Join(t.TempDir(), "alerts.json")
	empty := `{"rules":[]}`
	if err := os.WriteFile(path, []byte(empty), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(st, path, time.Minute, nil, discardLog(), WithClock(func() time.Time { return now }))
	if got := len(e.Evaluate(context.Background())); got != 0 {
		t.Fatalf("want 0 firings with empty rules, got %d", got)
	}

	// Add a rule and bump mtime into the future so the probe sees the change.
	newRules := `{"rules":[{"name":"silent","kind":"deadman","apps":["api"],"for":"5m"}]}`
	if err := os.WriteFile(path, []byte(newRules), 0o644); err != nil {
		t.Fatal(err)
	}
	future := now.Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	e.reload()
	if got := len(e.Evaluate(context.Background())); got != 1 {
		t.Fatalf("want 1 firing after reload, got %d", got)
	}
}

func TestChannelsExposedForAPI(t *testing.T) {
	st := testStore(t)
	e := newEngine(t, st, nil, time.Now(), NewWebhook("http://example.invalid"))
	if len(e.TestChannels()) != 1 {
		t.Fatal("want 1 test channel")
	}
	if len(e.Channels()) != 1 || e.Channels()[0] != "webhook" {
		t.Fatalf("channels = %v", e.Channels())
	}
}
