package alerts

// Tests for the update-available checker (A7): image parsing, digest
// comparison and the registry token dance, all against a local stub.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseImage(t *testing.T) {
	cases := []struct {
		in          string
		host, repos string
		tag         string
		ok          bool
	}{
		{"nginx:1.25", "registry-1.docker.io", "library/nginx", "1.25", true},
		{"nginx", "registry-1.docker.io", "library/nginx", "latest", true},
		{"user/img", "registry-1.docker.io", "user/img", "latest", true},
		{"user/img:9", "registry-1.docker.io", "user/img", "9", true},
		{"reg.example.com:5000/team/img:v7", "reg.example.com:5000", "team/img", "v7", true},
		{"localhost/img", "localhost", "img", "latest", true},
		{"nginx@sha256:abc", "", "", "", false}, // digest-pinned: no drift possible
		{"", "", "", "", false},
	}
	for _, tc := range cases {
		got, ok := parseImage(tc.in)
		if ok != tc.ok {
			t.Errorf("parseImage(%q) ok=%v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.host != tc.host || got.repo != tc.repos || got.tag != tc.tag {
			t.Errorf("parseImage(%q) = %+v, want host=%s repo=%s tag=%s", tc.in, got, tc.host, tc.repos, tc.tag)
		}
	}
}

// stubRegistry serves the challenge -> token -> manifest flow. Manifest
// digests ride in the Docker-Content-Digest response header.
func stubRegistry(t *testing.T, headerDigest string, challenge bool) *Updater {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"t0ken"}`)
		case challenge && r.Header.Get("Authorization") == "":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm=%q,service=%q`, "http://"+r.Host+"/token", "stub"))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Header().Set("Docker-Content-Digest", headerDigest)
			fmt.Fprint(w, "{}")
		}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	up := NewUpdater(nil, nil, discardLog(), time.Hour)
	up.endpoint = func(string) string { return srv.URL } // every registry host routes to the stub
	return up
}

func localDigestFn(dg string) digestFunc {
	return func(ctx context.Context, image string) ([]string, error) {
		return []string{"stub/repo@" + dg}, nil
	}
}

type listerStub []ImageRef

func (l listerStub) CurrentImages(ctx context.Context) []ImageRef { return l }

// runCheck wires a collecting feed into the updater and runs one check.
func runCheck(t *testing.T, up *Updater, imgs listerStub) []ContainerEvent {
	t.Helper()
	up.lister = imgs
	var fed []ContainerEvent
	up.check(context.Background(), func(ev ContainerEvent) { fed = append(fed, ev) })
	return fed
}

func TestUpdateAvailableFiresPseudoEvent(t *testing.T) {
	up := stubRegistry(t, "sha256:2222", true)
	up.digest = localDigestFn("sha256:1111")
	fed := runCheck(t, up, listerStub{{App: "web", Container: "web-1", Image: "stub/repo:1.0"}})
	if len(fed) != 1 {
		t.Fatalf("want 1 pseudo-event, got %d", len(fed))
	}
	ev := fed[0]
	if ev.Action != "update_available" || ev.App != "web" || ev.Container != "web-1" {
		t.Errorf("event = %+v", ev)
	}
	if ev.Attr["local"] != "sha256:1111" || ev.Attr["remote"] != "sha256:2222" || ev.Attr["image"] != "stub/repo:1.0" {
		t.Errorf("attrs = %v", ev.Attr)
	}
}

func TestUpdateCheckDigestsMatchStaysQuiet(t *testing.T) {
	up := stubRegistry(t, "sha256:1111", true)
	up.digest = localDigestFn("sha256:1111")
	if fed := runCheck(t, up, listerStub{{App: "web", Container: "web-1", Image: "stub/repo:1.0"}}); len(fed) != 0 {
		t.Fatalf("matched digests must not fire: %+v", fed)
	}
}

func TestUpdateChallengePathServesToken(t *testing.T) {
	// The challenge variant is what real registries (Docker Hub) do; make
	// sure the token retry reached the manifest endpoint.
	up := stubRegistry(t, "sha256:2222", true)
	up.digest = localDigestFn("sha256:1111")
	if fed := runCheck(t, up, listerStub{{App: "a", Container: "a-1", Image: "stub/repo:1"}}); len(fed) == 0 {
		t.Fatal("challenge flow should still resolve a manifest and fire")
	}
}

func TestUpdateCheckUnreachableStaysQuiet(t *testing.T) {
	up := NewUpdater(nil, localDigestFn("sha256:1111"), discardLog(), time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close() // connection refused from here on
	up.endpoint = func(string) string { return dead }
	if fed := runCheck(t, up, listerStub{{App: "a", Container: "a-1", Image: "stub/repo:1"}}); len(fed) != 0 {
		t.Fatalf("unreachable registry must not fire: %+v", fed)
	}
}

func TestUpdateCheckNoLocalDigestStaysQuiet(t *testing.T) {
	up := stubRegistry(t, "sha256:2222", false)
	up.digest = func(ctx context.Context, image string) ([]string, error) { return nil, nil }
	if fed := runCheck(t, up, listerStub{{App: "a", Container: "a-1", Image: "stub/repo:1"}}); len(fed) != 0 {
		t.Fatalf("unknown local digest must not fire: %+v", fed)
	}
}

func TestUpdateCheckSkipsPinnedAndBlankImages(t *testing.T) {
	up := stubRegistry(t, "sha256:2222", false)
	up.digest = localDigestFn("sha256:1111")
	if fed := runCheck(t, up, listerStub{
		{App: "a", Container: "a-1", Image: "stub/repo@sha256:0000"}, // pinned
		{App: "a", Container: "a-2", Image: ""},                      // blank
	}); len(fed) != 0 {
		t.Fatalf("pinned/blank images must not fire: %+v", fed)
	}
}

func TestUpdaterDedupsReplicas(t *testing.T) {
	up := stubRegistry(t, "sha256:2222", false)
	up.digest = localDigestFn("sha256:1111")
	fed := runCheck(t, up, listerStub{
		{App: "web", Container: "web-1", Image: "stub/repo:1.0"},
		{App: "web", Container: "web-2", Image: "stub/repo:1.0"},
	})
	if len(fed) != 1 {
		t.Fatalf("replica dedup failed: %d events", len(fed))
	}
}

func TestParseChallenge(t *testing.T) {
	realm, service, scope := parseChallenge(`Bearer realm="https://auth/x",service="svc",scope="repository:repo:pull"`, "repo")
	if realm != "https://auth/x" || service != "svc" || scope != "repository:repo:pull" {
		t.Fatalf("got %q %q %q", realm, service, scope)
	}
	// Without an explicit scope, derive the pull scope from the repo.
	realm, _, scope = parseChallenge(`Bearer realm="https://auth/x"`, "my/repo")
	if realm != "https://auth/x" || scope != "repository:my/repo:pull" {
		t.Fatalf("got %q %q", realm, scope)
	}
}
