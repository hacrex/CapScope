package alerts

// Update monitoring (A7, Dozzle's container-update events): on a daily
// jittered timer, compare each running container's image manifest digest with
// what the registry serves and feed an "update_available" pseudo-event into
// the alert engine. It is read-only toward Docker — /images/{id}/json only —
// and talks to registries anonymously (the token dance crates pull-through).
// Digest-pinned images, and images where no local digest is known, are skipped.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ImageLister supplies the images the updater checks; the collector
// implements it over its tracked containers.
type ImageLister interface {
	CurrentImages(ctx context.Context) []ImageRef
}

// ImageRef is the image declared by one running container.
type ImageRef struct {
	App       string
	Container string
	Image     string // name[:tag], e.g. "nginx:1.25"
}

// digestFunc resolves the local manifest digests ("sha256:...") of one image.
// Wired in main to the Docker client's ImageInspect.
type digestFunc func(ctx context.Context, image string) ([]string, error)

// feedFunc fans one pseudo-event into the engine. Wired to Engine.FeedEvent.
type feedFunc func(ContainerEvent)

// Updater runs the daily registry comparison.
type Updater struct {
	lister   ImageLister
	digest   digestFunc
	log      *slog.Logger
	interval time.Duration // between checks (default 24h)

	// httpClient overridable in tests; endpoint is derived from the image's
	// registry host so httptest servers work by name override only.
	http *http.Client
	// endpoint maps a registry host to its base URL; tests override this to
	// point at a local stub. nil => https://<host>.
	endpoint func(host string) string
	now      func() time.Time
}

// NewUpdater builds an Updater checking every interval (<= 0 => 24h).
func NewUpdater(lister ImageLister, digest digestFunc, log *slog.Logger, interval time.Duration) *Updater {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Updater{
		lister: lister, digest: digest, log: log, interval: interval,
		http: &http.Client{Timeout: 20 * time.Second},
		now:  time.Now,
	}
}

// Run sleeps a random fraction of the interval (so a fleet of CapScopes does
// not stampede the registry on the hour), then checks daily jittered.
func (u *Updater) Run(ctx context.Context, feed feedFunc) {
	delay := time.Duration(rand.Int64N(int64(u.interval) / 2))
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		u.check(ctx, feed)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// check compares every tracked image once and reports what changed.
func (u *Updater) check(ctx context.Context, feed feedFunc) {
	if u.lister == nil || feed == nil {
		return
	}
	imgs := u.lister.CurrentImages(ctx)
	seen := map[string]bool{} // image name => dedup across replicas
	for _, ir := range imgs {
		if ir.Image == "" || seen[ir.Image] {
			continue
		}
		seen[ir.Image] = true
		ir := ir
		remote, ok := u.remoteDigest(ctx, ir.Image)
		if !ok {
			continue // pinned, undecodable, or the registry is unreachable
		}
		local, err := u.localDigest(ctx, ir.Image)
		if err != nil || local == "" {
			u.log.Debug("update check: no local manifest digest", "image", ir.Image, "err", err)
			continue
		}
		if equalDigest(local, remote) {
			continue
		}
		u.log.Info("update available", "image", ir.Image, "app", ir.App, "local", local, "remote", remote)
		feed(ContainerEvent{
			Action:    "update_available",
			App:       ir.App,
			Container: ir.Container,
			Attr: map[string]string{
				"image":  ir.Image,
				"local":  local,
				"remote": remote,
			},
			TS: u.now(),
		})
	}
}

func (u *Updater) localDigest(ctx context.Context, image string) (string, error) {
	ds, err := u.digest(ctx, image)
	if err != nil || len(ds) == 0 {
		return "", err
	}
	// RepoDigests entries look like "reg/repo@sha256:..."; any of them is the
	// digest as this node pulled it.
	for _, d := range ds {
		if _, dg, ok := strings.Cut(d, "@"); ok && strings.HasPrefix(dg, "sha256:") {
			return dg, nil
		}
	}
	return "", nil
}

func equalDigest(a, b string) bool { return strings.EqualFold(a, b) }

func (u *Updater) remoteDigest(ctx context.Context, image string) (string, bool) {
	ir, ok := parseImage(image)
	if !ok {
		return "", false
	}
	get := func(bearer string) (*http.Response, error) {
		ep := "https://" + ir.host
		if u.endpoint != nil {
			ep = u.endpoint(ir.host)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep+"/v2/"+ir.repo+"/manifests/"+ir.tag, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
		}, ", "))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return u.http.Do(req)
	}
	resp, err := get("")
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// Registry dance: parse the WWW-Authenticate challenge, fetch an
		// anonymous pull token, retry once.
		realm, service, scope := parseChallenge(resp.Header.Get("WWW-Authenticate"), ir.repo)
		resp.Body.Close()
		if realm == "" {
			return "", false
		}
		token, err := u.pullToken(ctx, realm, service, scope)
		if err != nil {
			u.log.Debug("update check: token fetch failed", "image", image, "err", err)
			return "", false
		}
		resp, err = get(token)
	}
	if err != nil {
		u.log.Debug("update check: registry unreachable", "image", image, "err", err)
		return "", false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		u.log.Debug("update check: registry said no", "image", image, "status", resp.StatusCode)
		return "", false
	}
	dg := resp.Header.Get("Docker-Content-Digest")
	if dg == "" {
		return "", false
	}
	return dg, true
}

// registryRef is a parsed image reference.
type registryRef struct {
	host, repo, tag string
}

// parseImage splits an image reference into host/repo/tag. Multi-digest
// references ("repo@sha256:...") do not make sense to check and fail it.
func parseImage(name string) (registryRef, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return registryRef{}, false
	}
	if strings.Contains(name, "@") {
		return registryRef{}, false // digest-pinned: cannot drift
	}
	host := ""
	repo := name
	if i := strings.Index(name, "/"); i >= 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host = first
			repo = name[i+1:]
		}
	}
	tag := "latest"
	if i := strings.LastIndex(repo, ":"); i >= 0 {
		repo, tag = repo[:i], repo[i+1:]
	}
	if host == "" {
		host = "registry-1.docker.io" // normalized Docker Hub endpoint
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo // official images live under library/
		}
	}
	return registryRef{host: host, repo: strings.TrimPrefix(repo, "/"), tag: tag}, true
}

// parseChallenge pulls realm/service from a Bearer challenge and derives the
// pull scope. Lines look like: Bearer realm="https://auth...",service="...".
func parseChallenge(h, repo string) (realm, service, scope string) {
	for _, f := range strings.Split(h, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		v = strings.Trim(v, `"'`)
		k = strings.TrimPrefix(k, "Bearer ") // the scheme may ride on realm's field
		switch k {
		case "realm":
			realm = v
		case "service":
			service = v
		case "scope":
			scope = v
		}
	}
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	// Only trust https realms: a registry could point the challenge at an
	// internal http URL and pullToken would happily GET it (SSRF). Loopback
	// realms are allowed for local dev/test registries.
	if realm != "" && !allowedRealm(realm) {
		realm = ""
	}
	return realm, service, scope
}

// allowedRealm reports whether a challenge realm is safe to fetch tokens from.
// https is always allowed; http is allowed only on loopback (local dev/test
// registries). Remote http realms are rejected to avoid SSRF.
func allowedRealm(realm string) bool {
	if strings.HasPrefix(realm, "https://") {
		return true
	}
	u, err := url.Parse(realm)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

// pullToken fetches an anonymous pull token from a challenge realm.
func (u *Updater) pullToken(ctx context.Context, realm, service, scope string) (string, error) {
	q := url.Values{"scope": {scope}, "service": {service}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	type tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	var tr tokenResp
	// Cap the token response: a malicious registry could stream an enormous
	// JSON body to exhaust memory (the manifest path above already caps at 4 KiB).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&tr); err != nil {
		return "", err
	}
	if tr.Token == "" {
		tr.Token = tr.AccessToken
	}
	return tr.Token, nil
}
