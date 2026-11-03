package alerts

// Tests for A6 (container event rules), metric smoothing and the cooldown
// field, added with the Dozzle-parity batch.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"capscope/internal/store"
)

// httptestNewServer is a scoped server; the URL is live for the whole test.
func httptestNewServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// ---- rule validation ----

func TestEventRuleValidation(t *testing.T) {
	f := File{Rules: []Rule{
		{Name: "die", Kind: KindEvent, Event: "die"},                                // default exclude applied
		{Name: "die-all", Kind: KindEvent, Event: "die", Exclude: []int{}},          // explicit empty kept
		{Name: "die-few", Kind: KindEvent, Event: "die", Exclude: []int{0}},         // as written
		{Name: "boom", Kind: KindEvent, Event: "oom"},                               //
		{Name: "health", Kind: KindEvent, Event: "health_status"},                   // status defaults to unhealthy
		{Name: "upd", Kind: KindEvent, Event: "update_available"},                   //
		{Name: "cooldown", Kind: KindEvent, Event: "oom", Cooldown: Dur(time.Hour)}, // wins over repeat default
	}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := f.Rules[0].Exclude; len(got) != 4 {
		t.Errorf("default exclude = %v, want [0 130 143 137]", got)
	}
	if got := f.Rules[1].Exclude; len(got) != 0 {
		t.Errorf("explicit empty exclude = %v, want none", got)
	}
	if f.Rules[4].Status != "unhealthy" {
		t.Errorf("health_status status = %q, want unhealthy", f.Rules[4].Status)
	}
	if f.Rules[6].Repeat.D() != time.Hour {
		t.Errorf("cooldown = %v, want 1h silence", f.Rules[6].Repeat.D())
	}
	for _, ev := range []string{"", "start", "DIE"} {
		bad := File{Rules: []Rule{{Name: "x", Kind: KindEvent, Event: ev}}}
		if err := bad.Validate(); err == nil {
			t.Errorf("event %q should have been rejected", ev)
		}
	}
}

// ---- event matching and firing decisions ----

func TestEventMatches(t *testing.T) {
	r := Rule{Kind: KindEvent, Event: "die"} // no app filter
	ev := ContainerEvent{Action: "die", App: "web", ID: "c1"}
	if !r.eventMatches(ev) {
		t.Error("rule without apps should match any app")
	}
	r.Apps = []string{"api"}
	if r.eventMatches(ev) {
		t.Error("rule for api should not match web")
	}
	ev.App = "api"
	if !r.eventMatches(ev) {
		t.Error("rule for api should match api events")
	}
	if r.eventMatches(ContainerEvent{Action: "oom", App: "api"}) {
		t.Error("wrong action should not match")
	}
}

func TestEventFiringDie(t *testing.T) {
	r := Rule{Kind: KindEvent, Event: "die", Exclude: []int{0, 130, 143, 137}}
	if _, ok := r.eventFiring(ContainerEvent{Action: "die", App: "web", Container: "web-1", ExitCode: 1}); !ok {
		t.Error("exit 1 should fire")
	}
	for _, x := range []int{0, 130, 143, 137} {
		if _, ok := r.eventFiring(ContainerEvent{Action: "die", ExitCode: x}); ok {
			t.Errorf("exit %d should be excluded", x)
		}
	}
	// An excluded exit code can be re-enabled.
	r.Exclude = []int{130}
	if _, ok := r.eventFiring(ContainerEvent{Action: "die", ExitCode: 137}); !ok {
		t.Error("137 should fire when only 130 is excluded")
	}
	msg, ok := r.eventFiring(ContainerEvent{Action: "die", App: "web", Container: "web-1", ExitCode: 1})
	if !ok || !strings.Contains(msg, "exit code 1") || !strings.Contains(msg, "web-1") {
		t.Errorf("die message = %q", msg)
	}
}

func TestEventFiringOOMAndHealth(t *testing.T) {
	oom := Rule{Kind: KindEvent, Event: "oom"}
	msg, ok := oom.eventFiring(ContainerEvent{Action: "oom", App: "web", Container: "web-1"})
	if !ok || !strings.Contains(msg, "OOM") || !strings.Contains(msg, "web-1") {
		t.Errorf("oom message = %q", msg)
	}
	health := Rule{Kind: KindEvent, Event: "health_status", Status: "unhealthy"} // default of validate()
	if _, ok := health.eventFiring(ContainerEvent{Action: "health_status", App: "w", Container: "w-1", Attr: map[string]string{"healthStatus": "healthy"}}); ok {
		t.Error("healthy should not fire an unhealthy rule")
	}
	if _, ok := health.eventFiring(ContainerEvent{Action: "health_status", Attr: map[string]string{}}); ok {
		t.Error("missing status should not fire")
	}
	if _, ok := health.eventFiring(ContainerEvent{Action: "health_status", Attr: map[string]string{"healthStatus": "unhealthy"}}); !ok {
		t.Error("unhealthy should fire")
	}
	custom := Rule{Kind: KindEvent, Event: "health_status", Status: "stopping"}
	if _, ok := custom.eventFiring(ContainerEvent{Action: "health_status", Attr: map[string]string{"healthStatus": "stopping"}}); !ok {
		t.Error("custom status should fire")
	}
}

func TestEventFiringUpdateAvailable(t *testing.T) {
	r := Rule{Kind: KindEvent, Event: "update_available"}
	msg, ok := r.eventFiring(ContainerEvent{Action: "update_available", App: "web", Container: "web-1",
		Attr: map[string]string{"image": "nginx:1.25", "local": "sha256:1111", "remote": "sha256:2222"}})
	if !ok || !strings.Contains(msg, "nginx:1.25") || !strings.Contains(msg, "->") {
		t.Errorf("update message = %q", msg)
	}
}

// ---- engine handleEvent: firing, silence and cooldown ----

func TestHandleEventFiresAndSilences(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	e := newEngine(t, st, []Rule{{Name: "die", Kind: KindEvent, Event: "die"}}, now, stubChannel(t, "webhook", nil))
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "web", Container: "web-1", ExitCode: 1, TS: now})
	list, err := st.ListAlerts(context.Background(), "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Kind != KindEvent || !strings.Contains(list[0].Message, "exit code 1") {
		t.Fatalf("stored alerts = %+v", list)
	}
	// The repeat (silence) window keeps an immediate second event quiet.
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "web", Container: "web-1", ExitCode: 2, TS: now.Add(time.Second)})
	list, _ = st.ListAlerts(context.Background(), "web", 10)
	if len(list) != 1 {
		t.Errorf("want silence after first firing, got %d alerts", len(list))
	}
}

func TestHandleEventCooldownShortensSilence(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	// Cooldown 1h wins over repeat 30m: the second event a second later must
	// still be silent.
	e := newEngine(t, st, []Rule{{Name: "die", Kind: KindEvent, Event: "die", Cooldown: Dur(time.Hour), Repeat: Dur(30 * time.Minute)}}, now)
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "web", ExitCode: 1, TS: now})
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "web", ExitCode: 2, TS: now.Add(time.Second)})
	list, _ := st.ListAlerts(context.Background(), "web", 10)
	if len(list) != 1 {
		t.Fatalf("cooldown not applied: %d alerts", len(list))
	}
}

func TestHandleEventAppFilterAndExclusions(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	e := newEngine(t, st, []Rule{{Name: "die", Kind: KindEvent, Event: "die", Apps: []string{"api"}}}, now)
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "web", ExitCode: 1})   // wrong app
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "api", ExitCode: 143}) // excluded exit
	list, _ := st.ListAlerts(context.Background(), "", 10)
	if len(list) != 0 {
		t.Fatalf("want no firings, got %+v", list)
	}
	e.handleEvent(context.Background(), ContainerEvent{Action: "die", App: "api", ExitCode: 1})
	list, _ = st.ListAlerts(context.Background(), "", 10)
	if len(list) != 1 {
		t.Fatalf("want 1 firing, got %+v", list)
	}
}

func TestFeedEventStoresEventForRun(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	e := newEngine(t, st, []Rule{{Name: "oom", Kind: KindEvent, Event: "oom"}}, now)
	e.FeedEvent(ContainerEvent{Action: "oom", App: "web", Container: "web-1", TS: now})
	select {
	case got := <-e.evCh:
		if got.App != "web" || got.Action != "oom" {
			t.Fatalf("queued event = %+v", got)
		}
	default:
		t.Fatal("FeedEvent did not queue the event")
	}
}

// ---- metric smoothing (Window on a metric rule) ----

func TestMetricSmoothingTakesTheMean(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	// 95% and 50% average to 72.5%: below the 85% limit, so NO fire even
	// though one sample alone would exceed it.
	seedMetrics(t, st, "api",
		store.MetricPoint{TS: now.Add(-2 * time.Minute).UnixMilli(), Mem: 95, MemLimit: 100},
		store.MetricPoint{TS: now.Add(-time.Minute).UnixMilli(), Mem: 50, MemLimit: 100},
	)
	e := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 70, Window: Dur(5 * time.Minute)}}, now)
	if fired := e.Evaluate(context.Background()); len(fired) == 0 {
		t.Fatal("want firing at avg 72.5 over limit 70")
	}
	// Same samples against limit 85: mean 72.5 stays quiet. The unsmoothed
	// semantics would have fired (95 > 85 for a moment).
	e2 := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 85, Window: Dur(5 * time.Minute)}}, now)
	if fired := e2.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("mean 72.5 must not exceed 85: %+v", fired)
	}
}

func TestMetricSmoothingMessageAndStale(t *testing.T) {
	st := testStore(t)
	seedApp(t, st, "api")
	now := time.Now()
	seedMetrics(t, st, "api",
		store.MetricPoint{TS: now.Add(-time.Minute).UnixMilli(), Mem: 90, MemLimit: 100},
		store.MetricPoint{TS: now.Add(-30 * time.Second).UnixMilli(), Mem: 90, MemLimit: 100},
	)
	e := newEngine(t, st, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 85, Window: Dur(5 * time.Minute)}}, now)
	fired := e.Evaluate(context.Background())
	if len(fired) != 1 || !strings.Contains(fired[0].Message, "avg") || !strings.Contains(fired[0].Message, "2 samples") {
		t.Fatalf("smoothed message = %+v", fired)
	}
	// Stale data (newest sample older than 2x window) stays quiet.
	st2 := testStore(t)
	seedApp(t, st2, "api")
	seedMetrics(t, st2, "api", store.MetricPoint{TS: now.Add(-time.Hour).UnixMilli(), Mem: 99, MemLimit: 100})
	e2 := newEngine(t, st2, []Rule{{Name: "mem", Kind: KindMetric, Apps: []string{"api"}, Metric: "mem_pct", Op: "gt", Value: 50, Window: Dur(5 * time.Minute)}}, now)
	if fired := e2.Evaluate(context.Background()); len(fired) != 0 {
		t.Fatalf("stale data fired: %+v", fired)
	}
}

// ---- ${ENV} placeholders in channel endpoints ----

func TestExpandEnvAndRefusal(t *testing.T) {
	t.Setenv("CAP_TEST_TOKEN", "s3cret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "nope")
	got, err := expandEnv("http://h/${CAP_TEST_TOKEN}/x")
	if err != nil || got != "http://h/s3cret/x" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := expandEnv("http://h/${AWS_SECRET_ACCESS_KEY}"); err == nil {
		t.Fatal("AWS_ prefix must be refused")
	}
	for _, p := range []string{"DOCKER_", "KUBERNETES_", "DOZZLE_", "docker_"} {
		if _, err := expandEnv("http://h/${" + p + "X}"); err == nil {
			t.Errorf("prefix %s must be refused", p)
		}
	}
	// Unknown variables expand to empty (os.Expand semantics), not errors.
	if got, _ := expandEnv("http://h/${CAP_MISSING}"); got != "http://h/" {
		t.Errorf("missing var = %q", got)
	}
}

func TestWebhookExpandsEnvAtSendTime(t *testing.T) {
	var gotPath string
	srv := httptestNewServer(t, func(w http.ResponseWriter, r *http.Request) { gotPath = r.URL.Path })
	t.Setenv("CAP_TEST_HOOKID", "from-env")
	c := NewWebhook(srv.URL + "/hook/${CAP_TEST_HOOKID}")
	if err := c.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/hook/from-env" {
		t.Errorf("endpoint not expanded: %q", gotPath)
	}
}

// ---- ntfy channel ----

func TestNtfyChannel(t *testing.T) {
	var gotPath, gotTitle, gotToken, gotBody string
	srv := httptestNewServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("X-Title")
		gotToken = r.Header.Get("Authorization")
		b := readAll(t, r)
		gotBody = b
	})
	c := NewNtfy(srv.URL+"/my-topic", "tok123")
	if err := c.Send(context.Background(), "ntfy msg"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/my-topic" || gotTitle != "CapScope alert" || gotBody != "ntfy msg" {
		t.Errorf("path=%q title=%q body=%q", gotPath, gotTitle, gotBody)
	}
	if gotToken != "Bearer tok123" {
		t.Errorf("authorization = %q", gotToken)
	}
	// Token-less channel sends no Authorization header; empty ${ENV} token ok.
	srv2 := httptestNewServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	if err := NewNtfy(srv2.URL+"/t", "").Send(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}
