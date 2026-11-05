// Package api serves the JSON/SSE API and the embedded web UI.
package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"capscope/internal/alerts"
	"capscope/internal/auth"
	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/hub"
	"capscope/internal/store"
	"capscope/internal/web"
)

// dashTTL is how long an assembled /api/dashboard payload stays fresh. The UI
// polls on its own cadence; this only prevents overlapping requests from each
// re-running every aggregation query.
const dashTTL = 15 * time.Second

// dashCache is a tiny single-entry TTL cache for the dashboard payload, with a
// generation counter so data written after an assembly started cannot be
// cached stale (self-healing: a miss simply rebuilds).
type dashCache struct {
	mu     sync.Mutex
	value  map[string]any
	expiry time.Time
	gen    int64
}

func (c *dashCache) begin() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != nil && time.Now().Before(c.expiry) {
		return 0, true // fresh hit; caller serves c.value via get()
	}
	c.gen++
	return c.gen, false
}

func (c *dashCache) get() (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != nil && time.Now().Before(c.expiry) {
		return c.value, true
	}
	return nil, false
}

func (c *dashCache) store(gen int64, v map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen == c.gen { // no newer assembly superseded this one
		c.value = v
		c.expiry = time.Now().Add(dashTTL)
	}
}

type Server struct {
	cfg      config.Config
	st       *store.Store
	mgr      *collector.Manager
	hub      *hub.Hub
	log      *slog.Logger
	eng      *alerts.Engine       // nil when alerting is disabled
	sessions *auth.SessionManager // session manager (nil when auth is off)
	dash     dashCache            // short-lived /api/dashboard payload cache
}

// WithAlerts attaches the alert engine so /api/alerts works.
func WithAlerts(e *alerts.Engine) func(*Server) { return func(s *Server) { s.eng = e } }

// WithAuth attaches a session manager for bearer-token login auth.
func WithAuth(a *auth.SessionManager) func(*Server) { return func(s *Server) { s.sessions = a } }

// WithUserStore attaches the store for user lookups (multi-user mode).
func WithUserStore(st *store.Store) func(*Server) { return func(s *Server) { s.st = st } }

func New(cfg config.Config, st *store.Store, mgr *collector.Manager, h *hub.Hub, log *slog.Logger, opts ...func(*Server)) *Server {
	s := &Server{cfg: cfg, st: st, mgr: mgr, hub: h, log: log}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/oidc/login", s.oidcLogin)
	mux.HandleFunc("GET /api/oidc/callback", s.oidcCallback)
	mux.HandleFunc("POST /api/share", s.shareCreate)
	mux.HandleFunc("GET /s/{token}", s.sharePage)
	mux.HandleFunc("GET /api/containers", s.containers)
	mux.HandleFunc("GET /api/hosts", s.hosts)
	mux.HandleFunc("GET /api/apps", s.apps)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/logs/export", s.logsExport)
	mux.HandleFunc("GET /api/logs/raw", s.logsRaw)
	mux.HandleFunc("GET /api/logs/stream", s.logStream)
	mux.HandleFunc("GET /api/access", s.access)
	mux.HandleFunc("GET /api/access/export", s.accessExport)
	mux.HandleFunc("GET /api/sql", s.sql)
	mux.HandleFunc("GET /api/metrics", s.metrics)
	mux.HandleFunc("GET /api/snapshots", s.snapshots)
	mux.HandleFunc("GET /api/snapshots/{id}", s.snapshot)
	mux.HandleFunc("GET /api/snapshots/{id}/export", s.snapshotExport)
	mux.HandleFunc("GET /api/snapshots/{id}/incident.html", s.snapshotIncident)
	mux.HandleFunc("GET /api/alerts", s.alertsList)
	mux.HandleFunc("GET /api/alerts/rules", s.alertsRules)
	mux.HandleFunc("POST /api/alerts/test", s.alertsTest)
	mux.HandleFunc("POST /api/alerts/test-rule", s.alertsTestRule)
	mux.HandleFunc("POST /api/alerts/rules", s.alertsRulesSave)
	mux.HandleFunc("POST /api/alerts/rules/validate", s.alertsRulesValidate)
	mux.HandleFunc("GET /api/deploys", s.deploysList)
	mux.HandleFunc("POST /api/deploys", s.deploysCreate)
	mux.HandleFunc("GET /api/dashboard", s.dashboard)
	mux.HandleFunc("GET /api/crashloops", s.crashLoops)
	mux.HandleFunc("GET /metrics", s.prometheus)
	// User management (Phase 3.3): admin only.
	mux.HandleFunc("GET /api/users", s.usersList)
	mux.HandleFunc("POST /api/users", s.usersCreate)
	mux.HandleFunc("POST /api/users/{id}/role", s.usersSetRole)
	mux.HandleFunc("DELETE /api/users/{id}", s.usersDelete)
	sub, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /", http.FileServerFS(sub))
	return s.auth(mux)
}

// sessionCookie carries the bearer token so streaming clients that cannot set
// an Authorization header (EventSource) authenticate like fetch() callers do.
// SameSite=Lax: sent on same-site GET requests (including EventSource), so
// live-tail streaming works behind reverse proxies like CapRover. Still blocks
// cross-site POSTs for CSRF protection.
const (
	sessionCookie = "capscope_token"
	cookieMaxAge  = 24 * 60 * 60 // session lifetime is 24h (see auth package)
)

// newSessionCookie writes the login cookie. Secure is set on HTTPS responses.
// Behind TLS-terminating reverse proxies (CapRover), X-Forwarded-Proto indicates
// the original client scheme so the Secure flag is set correctly.
func newSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	secure := r.TLS != nil
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		secure = strings.ToLower(proto) == "https"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Secure:   secure,
	})
}

// requestToken resolves the caller's session token from an Authorization
// header first, falling back to the login cookie.
func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// proxyAuth trusts the identity header set by a reverse proxy in front of
// CapScope (AUTH_PROVIDER=proxy, PROXY_USER_HEADER=Remote-User by default).
// Authelia, Authentik, Cloudflare Access and friends set it after their own
// authentication; CapScope never sees a password in this mode.
//
// The header is whoever can reach CapScope's port: anything that connects
// directly — bypassing the proxy — can claim any identity. Publish only the
// proxy, keep CapScope on an internal network, and HTTPS stays on the proxy.
// Login-adjacent endpoints stay reachable so the SPA can boot and report state.
func (s *Server) proxyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The SPA boots and reports state without a session; login is proxied
		// so authentication stays with the reverse proxy entirely. /s/ share
		// links are capability links: the token authorizes the redirect, and
		// the view the SPA then loads is auth-protected as usual.
		if r.URL.Path == "/healthz" || r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/s/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		user := strings.TrimSpace(r.Header.Get(s.cfg.ProxyUserHeader))
		if user == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="capscope"`)
			http.Error(w, "unauthorized: "+s.cfg.ProxyUserHeader+" not set — connect through the reverse proxy that sets it", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.cfg.AuthProvider == "proxy" {
		return s.proxyAuth(next)
	}
	if s.cfg.AuthProvider == "oidc" {
		return s.oidcAuth(next)
	}
	if s.cfg.AuthPass == "" || s.sessions == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/s/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/oidc/login" || strings.HasPrefix(r.URL.Path, "/api/oidc/callback") {
			next.ServeHTTP(w, r)
			return
		}
		token := requestToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="capscope"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, _, _, ok := s.sessions.Validate(token); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="capscope"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAdmin wraps next so that only admin-role sessions can proceed.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := requestToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="capscope"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _, role, ok := s.sessions.Validate(token)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="capscope"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if role != auth.RoleAdmin {
			http.Error(w, "forbidden: admin required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// oidcAuth is the middleware for AUTH_PROVIDER=oidc. It redirects unauthenticated
// requests to /api/oidc/login to start the OIDC flow.
func (s *Server) oidcAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/s/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/oidc/login" || strings.HasPrefix(r.URL.Path, "/api/oidc/callback") {
			next.ServeHTTP(w, r)
			return
		}
		token := requestToken(r)
		if token == "" {
			http.Redirect(w, r, "/api/oidc/login?redirect_to="+url.QueryEscape(r.URL.Path), http.StatusFound)
			return
		}
		if _, _, _, ok := s.sessions.Validate(token); !ok {
			http.Redirect(w, r, "/api/oidc/login?redirect_to="+url.QueryEscape(r.URL.Path), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// login validates username/password against AUTH_USER/AUTH_PASS and issues a
// bearer token. Body: {"username": "...", "password": "..."}.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthProvider == "proxy" {
		fail(w, 400, errors.New("AUTH_PROVIDER=proxy — authentication is handled by the reverse proxy setting "+s.cfg.ProxyUserHeader))
		return
	}
	if s.cfg.AuthProvider == "oidc" {
		fail(w, 400, errors.New("AUTH_PROVIDER=oidc — use /api/oidc/login instead"))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		fail(w, 400, err)
		return
	}
	user, err := s.st.GetUserByName(creds.Username)
	if err != nil {
		fail(w, 401, auth.ErrInvalidCredentials)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(creds.Password)); err != nil {
		fail(w, 401, auth.ErrInvalidCredentials)
		return
	}
	token := s.sessions.CreateSession(user.ID, user.Name, user.Role)
	// Mirror the token into an HttpOnly cookie: EventSource cannot send an
	// Authorization header, so /api/logs/stream (live tail) needs the cookie.
	newSessionCookie(w, r, token, cookieMaxAge)
	writeJSON(w, map[string]any{"token": token, "user": user.Name, "role": user.Role})
}

// logout deletes the caller's session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.AuthProvider == "proxy" {
		// Nothing to delete: sessions live in the reverse proxy. The UI stays
		// in the proxy's own flow for logging out.
		writeJSON(w, map[string]string{"status": "logged out"})
		return
	}
	if token := requestToken(r); token != "" {
		s.sessions.Delete(token)
	}
	newSessionCookie(w, r, "", -1) // clear the login cookie
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "logged out"})
}

// session returns the authenticated user, or 401 when no valid session exists.
// When auth is disabled (no AUTH_PASS configured), it reports auth_enabled:false
// with an anonymous user so the SPA can skip the login overlay entirely.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthProvider == "proxy" {
		user := strings.TrimSpace(r.Header.Get(s.cfg.ProxyUserHeader))
		if user == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Sessionless: the reverse proxy owns the credential. Report a nominal
		// expiry so the SPA's expiry-warning banner stays quiet.
		writeJSON(w, map[string]any{"auth_enabled": true, "user": user, "role": auth.RoleViewer, "expires_at": time.Now().Add(24 * time.Hour).Unix()})
		return
	}
	if s.cfg.AuthProvider == "oidc" {
		// OIDC: the session cookie is set by /api/oidc/callback. Validate it.
		token := requestToken(r)
		if token == "" {
			writeJSON(w, map[string]any{"auth_enabled": true, "user": "anonymous", "expires_at": 0})
			return
		}
		userID, name, role, ok := s.sessions.Validate(token)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"auth_enabled": true, "user": name, "role": role, "user_id": userID, "expires_at": s.sessions.Expiry(token)})
		return
	}
	if s.cfg.AuthPass == "" || s.sessions == nil {
		writeJSON(w, map[string]any{"auth_enabled": false, "user": "anonymous", "expires_at": 0})
		return
	}
	token := requestToken(r)
	_, name, role, ok := s.sessions.Validate(token)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"auth_enabled": true, "user": name, "role": role, "expires_at": s.sessions.Expiry(token)})
}

// oidcLogin starts the OIDC authorization code flow with PKCE.
// Body (optional JSON): {"redirect_to": "/logs"} where to go after login.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthProvider != "oidc" {
		fail(w, 400, errors.New("OIDC is not configured (AUTH_PROVIDER=oidc)"))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	redirectURI := s.cfg.Listen // the callback must match what the provider expects
	if s.cfg.OIDCClientID == "" || s.cfg.OIDCIssuer == "" {
		fail(w, 500, errors.New("OIDC is not fully configured"))
		return
	}
	// Build the provider fresh each time (Discovery is cheap and cached by the provider)
	prov, err := auth.NewProvider(s.cfg.OIDCIssuer, s.cfg.OIDCClientID, s.cfg.OIDCClientSecret, s.cfg.AuthUser, s.log)
	if err != nil {
		fail(w, 500, fmt.Errorf("oidc discovery failed: %w", err))
		return
	}
	redirectTo := r.URL.Query().Get("redirect_to")
	if redirectTo == "" {
		redirectTo = "/"
	}
	authURL, state, err := prov.AuthorizationURL(redirectURI, redirectTo)
	if err != nil {
		fail(w, 500, fmt.Errorf("oidc: build auth URL failed: %w", err))
		return
	}
	// Store the state in a short-lived cookie so the callback can verify it.
	http.SetCookie(w, &http.Cookie{
		Name:     "oidc_state",
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600, // 10 minutes
		Secure:   r.TLS != nil,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// oidcCallback handles the OIDC authorization response.
// Query params: code, state, error, error_description.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthProvider != "oidc" {
		fail(w, 400, errors.New("OIDC is not configured (AUTH_PROVIDER=oidc)"))
		return
	}
	if r.URL.Query().Get("error") != "" {
		fail(w, 400, errors.New("oidc: "+r.URL.Query().Get("error_description")))
		return
	}
	code := r.URL.Query().Get("code")
	stateParam := r.URL.Query().Get("state")
	if code == "" || stateParam == "" {
		fail(w, 400, errors.New("oidc: missing code or state"))
		return
	}
	// Verify state against the cookie.
	cookie, err := r.Cookie("oidc_state")
	if err != nil || cookie.Value != stateParam {
		fail(w, 400, errors.New("oidc: state mismatch"))
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     "oidc_state",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	prov, err := auth.NewProvider(s.cfg.OIDCIssuer, s.cfg.OIDCClientID, s.cfg.OIDCClientSecret, s.cfg.AuthUser, s.log)
	if err != nil {
		fail(w, 500, fmt.Errorf("oidc discovery failed: %w", err))
		return
	}
	redirectURI := s.cfg.Listen
	name, err := prov.Callback(redirectURI, stateParam, code)
	if err != nil {
		fail(w, 400, fmt.Errorf("oidc: callback failed: %w", err))
		return
	}
// Find or create the user in the store.
	u, err := s.st.GetUserByName(name)
	if err != nil {
		// Auto-provision the first user as admin.
		users, err := s.st.ListUsers()
		if err != nil {
			fail(w, 500, err)
			return
		}
		role := auth.RoleViewer
		if len(users) == 0 {
			role = auth.RoleAdmin
		}
		userID, err := s.st.CreateUser(name, "", role)
		if err != nil {
			fail(w, 500, fmt.Errorf("oidc: create user failed: %w", err))
			return
		}
		u = &store.User{ID: userID, Name: name, Role: role}
	}
	token := s.sessions.CreateSession(u.ID, u.Name, u.Role)
	newSessionCookie(w, r, token, cookieMaxAge)
	redirectTo := "/"
	// Read the redirect target from a cookie set by the login page, default "/".
	if c, err := r.Cookie("oidc_redirect"); err == nil {
		redirectTo = c.Value
		http.SetCookie(w, &http.Cookie{Name: "oidc_redirect", Value: "", Path: "/", MaxAge: -1})
	}
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// shareCreate issues an expiring capability link for the current view. Body:
// {"qs":"tab=logs&app=web&range=1d&q=x","hash":"crash=12","ttl":"24h"}.
// The link expires server-side and redirects to the saved view.
func (s *Server) shareCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		QS   string `json:"qs"`
		Hash string `json:"hash"`
		TTL  string `json:"ttl"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		fail(w, 400, err)
		return
	}
	qs := strings.TrimPrefix(strings.TrimSpace(body.QS), "?")
	hash := strings.TrimPrefix(body.Hash, "#")
	// Whitelist the query-string characters on the way in: the redirect target
	// must stay a same-origin URL that demo/applyURL can parse.
	if qs == "" && hash == "" {
		fail(w, 400, errors.New("share: qs or hash required"))
		return
	}
	if len(qs) > 2048 || len(hash) > 64 || strings.ContainsAny(qs+hash, "\n\r\"'<>\\") {
		fail(w, 400, errors.New("share: qs/hash malformed or too long"))
		return
	}
	ttl := 7 * 24 * time.Hour
	if d, err := config.ParseDuration(body.TTL); err == nil && d > 0 {
		ttl = d
	}
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > 90*24*time.Hour {
		ttl = 90 * 24 * time.Hour
	}
	sh, err := s.st.CreateShare(r.Context(), qs, hash, ttl)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"url": "/s/" + sh.Token, "expires_at": sh.ExpiresMs})
}

// ---- user management (Phase 3.3) ----

// usersList returns all users (admin only).
func (s *Server) usersList(w http.ResponseWriter, r *http.Request) {
	token := requestToken(r)
	if _, _, role, ok := s.sessions.Validate(token); !ok || role != auth.RoleAdmin {
		http.Error(w, "forbidden: admin required", http.StatusForbidden)
		return
	}
	users, err := s.st.ListUsers()
	if err != nil {
		fail(w, 500, err)
		return
	}
	// Never return password hashes.
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id":         u.ID,
			"name":       u.Name,
			"role":       u.Role,
			"created_ms": u.CreatedMs,
		})
	}
	writeJSON(w, map[string]any{"users": out})
}

// usersCreate creates a new user (admin only).
func (s *Server) usersCreate(w http.ResponseWriter, r *http.Request) {
	token := requestToken(r)
	if _, _, role, ok := s.sessions.Validate(token); !ok || role != auth.RoleAdmin {
		http.Error(w, "forbidden: admin required", http.StatusForbidden)
		return
	}
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		fail(w, 400, err)
		return
	}
	if body.Name == "" || body.Password == "" {
		fail(w, 400, errors.New("name and password are required"))
		return
	}
	if body.Role != auth.RoleAdmin && body.Role != auth.RoleViewer {
		body.Role = auth.RoleViewer
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, err)
		return
	}
	id, err := s.st.CreateUser(body.Name, string(hash), body.Role)
	if err != nil {
		fail(w, 400, fmt.Errorf("user already exists: %s", body.Name))
		return
	}
	writeJSON(w, map[string]any{"id": id, "name": body.Name, "role": body.Role})
}

// usersSetRole changes a user's role (admin only).
func (s *Server) usersSetRole(w http.ResponseWriter, r *http.Request) {
	token := requestToken(r)
	if _, _, role, ok := s.sessions.Validate(token); !ok || role != auth.RoleAdmin {
		http.Error(w, "forbidden: admin required", http.StatusForbidden)
		return
	}
	idStr := r.PathValue("id")
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		fail(w, 400, errors.New("invalid user id"))
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.st.UpdateUserRole(id, body.Role); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// usersDelete removes a user (admin only).
func (s *Server) usersDelete(w http.ResponseWriter, r *http.Request) {
	token := requestToken(r)
	if _, _, role, ok := s.sessions.Validate(token); !ok || role != auth.RoleAdmin {
		http.Error(w, "forbidden: admin required", http.StatusForbidden)
		return
	}
	idStr := r.PathValue("id")
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		fail(w, 400, errors.New("invalid user id"))
		return
	}
	// Prevent self-delete.
	if _, _, _, ok := s.sessions.Validate(token); ok {
		if uid := s.sessions.UserID(token); uid == id {
			fail(w, 400, errors.New("cannot delete yourself"))
			return
		}
	}
	if err := s.st.DeleteUser(id); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// sharePage redirects to the saved view when the capability token is still
// alive. Unknown/expired links read exactly the same (410, no oracle).
func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	sh, err := s.st.GetShare(ctx, r.PathValue("token"))
	if err != nil {
		http.Error(w, "share link not found or expired", http.StatusGone)
		return
	}
	target := "/?" + sh.QS
	if sh.Hash != "" {
		target += "#" + sh.Hash
	}
	if target == "/?" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"containers": s.mgr.Containers(r.URL.Query().Get("host"))})
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListHosts(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	if list == nil {
		list = []store.Host{}
	}
	writeJSON(w, map[string]any{"hosts": list})
}

func (s *Server) apps(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.AppSummaries(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	if a == nil {
		a = []store.AppSummary{}
	}
	writeJSON(w, map[string]any{"apps": a})
}

// parseLogQuery reads the shared filter parameters used by /api/logs and /api/logs/stream.
func parseLogQuery(r *http.Request) (store.LogQuery, error) {
	v := r.URL.Query()
	q := store.LogQuery{
		App:       v.Get("app"),
		Container: v.Get("container"),
		Q:         v.Get("q"),
	}
	switch v.Get("stream") {
	case "stdout", "o":
		q.Stream = "o"
	case "stderr", "e":
		q.Stream = "e"
	}
	if l := v.Get("level"); l != "" {
		for _, x := range strings.Split(l, ",") {
			if x = strings.TrimSpace(x); x != "" {
				q.Levels = append(q.Levels, x)
			}
		}
	}
	if b, _ := strconv.ParseBool(v.Get("regex")); b && q.Q != "" {
		re, err := regexp.Compile("(?i)" + q.Q)
		if err != nil {
			return q, fmt.Errorf("invalid regex: %w", err)
		}
		q.Regex = re
	}
	q.SinceMs = parseSince(v.Get("since"))
	q.UntilMs, _ = strconv.ParseInt(v.Get("until"), 10, 64)
	q.AfterID, _ = strconv.ParseInt(v.Get("after_id"), 10, 64)
	// The UI speaks since_id for /api/logs catch-ups; accept it as an alias so
	// both names work instead of one silently being ignored.
	if q.AfterID == 0 {
		q.AfterID, _ = strconv.ParseInt(v.Get("since_id"), 10, 64)
	}
	q.BeforeID, _ = strconv.ParseInt(v.Get("before_id"), 10, 64)
	q.Asc = v.Get("asc") == "1"
	q.Limit, _ = strconv.Atoi(v.Get("limit"))
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500 // mirror store.QueryLogs so paging math is exact here too
	}
	return q, nil
}

// parseSince accepts a relative duration ("15m", "2h", "1d") or an epoch in milliseconds.
func parseSince(s string) int64 {
	if s == "" {
		return 0
	}
	if d, err := config.ParseDuration(s); err == nil {
		return time.Now().Add(-d).UnixMilli()
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	rows, err := s.st.QueryLogs(r.Context(), q)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"logs": rows})
}

// access serves the GoAccess-style dashboard aggregates for one window:
// totals, status-code distribution, top paths/IPs/referrers and a timeline.
func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	rng := time.Hour
	if d, err := config.ParseDuration(v.Get("range")); err == nil && d > 0 {
		rng = d
	}
	bucket := rng.Milliseconds() / 300
	if bucket < 30000 {
		bucket = 30000
	}
	topN := 20
	if n, err := strconv.Atoi(v.Get("top")); err == nil && n > 0 && n <= 100 {
		topN = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	agg, err := s.st.QueryAccessFiltered(ctx, v.Get("app"), v.Get("status"), time.Now().Add(-rng).UnixMilli(), 0, bucket, topN)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, agg)
}

// csvHeader writes the download fields common to both CSV exports.
func csvHeader(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
}

// logsExport streams the log table as CSV, paginating by id so exports can
// reach beyond the normal 5000-row API cap (200k lines at most).
// logsExport streams the matching rows as CSV; `fmt=json` streams the same
// rows as one JSON document (Phase 6.2: any-range export for BI tools).
func (s *Server) logsExport(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if r.URL.Query().Get("fmt") == "json" {
		s.logsExportJSON(w, r, q)
		return
	}
	if r.URL.Query().Get("fmt") == "png" {
		s.logsExportPNG(w, r, q)
		return
	}
	csvHeader(w, "capscope-logs.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ts", "ts_ms", "container_id", "container", "app", "stream", "level", "line"})
	defer cw.Flush()
	// Page at the store's maximum: parseLogQuery defaults limit to 500, which
	// would make paging needlessly slow (and, on the FTS path, fragile).
	q.Limit = 5000
	err = s.eachLogRow(r.Context(), q, 200_000, func(l store.LogRow) error { // matches the documented export ceiling (docs/API.md)
		return cw.Write([]string{
			time.Unix(0, l.TS).UTC().Format(time.RFC3339Nano), itoa64(l.TSMs),
			l.ContainerID, l.Container, l.App, streamName(l.Stream), l.Level, l.Line,
		})
	})
	if err != nil {
		_ = err // headers are already sent; a mid-stream error leaves a truncated file either way
	}
}

// eachLogRow pages the log store ascending up to exportCap and calls fn per
// row. Shared by the CSV and raw-text exports so both reach the same ceiling
// with the same FTS overlap guard.
func (s *Server) eachLogRow(ctx context.Context, q store.LogQuery, exportCap int, fn func(store.LogRow) error) error {
	written := 0
	lastID := int64(0) // highest id already written; rows are always ascending
	for written < exportCap && ctx.Err() == nil {
		rows, err := s.st.QueryLogs(ctx, q)
		if err != nil || len(rows) == 0 {
			if err != nil {
				return err
			}
			return nil
		}
		// Skip rows already emitted. This matters on the FTS fast path, which
		// re-runs its own query and may return an overlapping newest-first
		// page regardless of AfterID.
		start := 0
		for start < len(rows) && rows[start].ID <= lastID {
			start++
		}
		fresh := rows[start:]
		if len(fresh) == 0 {
			return nil // no forward progress: everything returned was already written
		}
		pageSize := q.Limit
		if pageSize <= 0 || pageSize > 5000 {
			pageSize = 500
		}
		for _, l := range fresh {
			if written == exportCap {
				return nil
			}
			if err := fn(l); err != nil {
				return err
			}
			written++
			lastID = l.ID
		}
		if len(fresh) < pageSize {
			return nil // partial page: result set exhausted (or regex scan cap hit)
		}
		// Force subsequent pages strictly ascending from the last written id.
		q.AfterID = lastID
	}
	return nil
}

// logsRaw streams the raw line text, one row per line, no CSV quoting —
// the shape pipe/grep/awk expects beside the CSV export.
func (s *Server) logsRaw(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", `attachment; filename="capscope-logs.txt"`)
	q.Limit = 5000
	const exportCap = 200_000 // same ceiling as the CSV export
	first := true
	_ = s.eachLogRow(r.Context(), q, exportCap, func(l store.LogRow) error {
		if !first {
			w.Write([]byte{'\n'})
		}
		first = false
		_, err := w.Write([]byte(l.Line))
		return err
	})
}

// logsExportJSON streams the same paging shape as the CSV export but as a
// valid JSON document — `{"logs":[...]}` — so any range fits into jq or a BI
// pipeline without the /api/logs limit page cap.
func (s *Server) logsExportJSON(w http.ResponseWriter, r *http.Request, q store.LogQuery) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="capscope-logs.json"`)
	q.Limit = 5000
	const exportCap = 200_000 // same ceiling as the CSV export
	w.Write([]byte(`{"logs":[`))
	first := true
	_ = s.eachLogRow(r.Context(), q, exportCap, func(l store.LogRow) error {
		b, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if !first {
			w.Write([]byte(","))
		}
		first = false
		_, err = w.Write(b)
		return err
	})
	w.Write([]byte(`]}`))
}

// logsExportPNG streams the matching rows as a PNG image (visual export).
// Renders each log row as monospace text on a PNG canvas using a built-in
// 5x5 bitmap font (no external font dependencies).
func (s *Server) logsExportPNG(w http.ResponseWriter, r *http.Request, q store.LogQuery) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Disposition", `attachment; filename="capscope-logs.png"`)
	q.Limit = 5000
	const exportCap = 200_000 // same ceiling as the CSV export

	var rows []store.LogRow
	_ = s.eachLogRow(r.Context(), q, exportCap, func(l store.LogRow) error {
		rows = append(rows, l)
		return nil
	})
	if len(rows) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	const (
		charW   = 6
		charH   = 8
		marginX = 10
		marginY = 10
		lineGap = 2
	)

	type line struct {
		text string
	}
	lines := make([]line, 0, len(rows))
	maxTextLen := 0
	for _, l := range rows {
		ts := time.Unix(0, l.TS).UTC().Format("15:04:05.000")
		text := fmt.Sprintf("%s %s %s %s %s %s %s", ts, l.ContainerID, l.Container, l.App, l.Stream, l.Level, l.Line)
		if len(text) > maxTextLen {
			maxTextLen = len(text)
		}
		lines = append(lines, line{text: text})
	}
	if maxTextLen == 0 {
		maxTextLen = 1
	}

	imgW := marginX*2 + maxTextLen*charW
	imgH := marginY*2 + len(lines)*(charH+lineGap)

	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)

	black := color.RGBA{0, 0, 0, 255}
	y := marginY
	for _, ln := range lines {
		x := marginX
		for _, ch := range ln.text {
			bits, ok := font5x5[ch]
			if !ok {
				x += charW
				continue
			}
			for row := 0; row < 5; row++ {
				for col := 0; col < 5; col++ {
					if bits>>(uint(row)*5+uint(col))&1 != 0 {
						img.Set(x+col, y+row, black)
					}
				}
			}
			x += charW
		}
		y += charH + lineGap
	}

	_ = png.Encode(w, img)
}

// font5x5 is a minimal 5x5 bitmap font for common ASCII characters.
// Each uint32 encodes 5 rows (bits 0-4 of each byte group), each bit is a column.
var font5x5 = map[rune]uint32{
	' ': 0x00000,
	'!': 0x02828,
	'"': 0x0A000,
	'#': 0x0A1FA,
	'$': 0x081FA,
	'%': 0x110A2,
	'&': 0x0A1BA,
	'\'': 0x02000,
	'(': 0x04180,
	')': 0x08110,
	'*': 0x042A4,
	'+': 0x00284,
	',': 0x00280,
	'-': 0x00020,
	'.': 0x00060,
	'/': 0x10108,
	'0': 0x0A11A,
	'1': 0x02022,
	'2': 0x001FA,
	'3': 0x020FA,
	'4': 0x088FA,
	'5': 0x021FA,
	'6': 0x021BA,
	'7': 0x0001E,
	'8': 0x021BA,
	'9': 0x021FA,
	':': 0x00202,
	';': 0x00206,
	'<': 0x01024,
	'=': 0x00200,
	'>': 0x04081,
	'?': 0x0000A,
	'@': 0x0A1BA,
	'A': 0x0211A,
	'B': 0x0213A,
	'C': 0x04118,
	'D': 0x02118,
	'E': 0x021FE,
	'F': 0x0211E,
	'G': 0x041BA,
	'H': 0x0111A,
	'I': 0x021FE,
	'J': 0x0011C,
	'K': 0x01124,
	'L': 0x0011E,
	'M': 0x0911A,
	'N': 0x0911A,
	'O': 0x0A11A,
	'P': 0x0213A,
	'Q': 0x0A11A,
	'R': 0x0213A,
	'S': 0x020FA,
	'T': 0x000FE,
	'U': 0x0111A,
	'V': 0x0110A,
	'W': 0x0910A,
	'X': 0x09069,
	'Y': 0x09060,
	'Z': 0x010E1,
	'[': 0x04118,
	'\\': 0x10084,
	']': 0x08110,
	'^': 0x00500,
	'_': 0x0000E,
	'`': 0x00000,
	'a': 0x001FA,
	'b': 0x0019E,
	'c': 0x000BA,
	'd': 0x00AFA,
	'e': 0x000FA,
	'f': 0x001FE,
	'g': 0x00AF0,
	'h': 0x0019E,
	'i': 0x00022,
	'j': 0x00026,
	'k': 0x001A4,
	'l': 0x0001E,
	'm': 0x001DA,
	'n': 0x0019A,
	'o': 0x0009A,
	'p': 0x0013A,
	'q': 0x00AFA,
	'r': 0x00180,
	's': 0x000FA,
	't': 0x000FE,
	'u': 0x0019A,
	'v': 0x000A4,
	'w': 0x000AA,
	'x': 0x000A4,
	'y': 0x009A0,
	'z': 0x000E1,
	'{': 0x04148,
	'|': 0x00100,
	'}': 0x08140,
	'~': 0x00540,
}

// streamName maps a stream code to a display name.
func streamName(s string) string {

	if s == "e" {
		return "stderr"
	}
	return "stdout"
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// sql runs one read-only ad-hoc statement over the stored tables. Validation
// and the row cap live in store.QuerySQL; a 5s deadline keeps a heavy scan
// from pinning the read pool.
func (s *Server) sql(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	res, err := s.st.QuerySQL(ctx, r.URL.Query().Get("query"), limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidQuery) {
			fail(w, 400, err)
		} else {
			fail(w, 500, err)
		}
		return
	}
	writeJSON(w, res)
}

// accessExport downloads the raw parsed access rows as CSV.
func (s *Server) accessExport(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	rng := 24 * time.Hour
	if d, err := config.ParseDuration(v.Get("range")); err == nil && d > 0 {
		rng = d
	}
	limit, _ := strconv.ParseInt(v.Get("limit"), 10, 64)
	rows, err := s.st.QueryAccessRowsFiltered(r.Context(), v.Get("app"), v.Get("status"), time.Now().Add(-rng).UnixMilli(), 0, limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	csvHeader(w, "capscope-access.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ts", "ts_ms", "container", "app", "ip", "method", "path", "status", "bytes", "referer", "user_agent"})
	for _, x := range rows {
		_ = cw.Write([]string{
			time.UnixMilli(x.TS).UTC().Format(time.RFC3339), itoa64(x.TS),
			x.Container, x.App, x.IP, x.Method, x.Path, strconv.Itoa(x.Status),
			itoa64(x.Bytes), x.Referer, x.UA,
		})
	}
	cw.Flush()
}

func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	// Gapless live tail: when the browser's EventSource reconnects it sends
	// Last-Event-ID with the id of the last row it rendered. Filtering hub
	// delivery to id > that value means the reconnect replays no duplicates
	// and misses nothing the subscriber already saw.
	if q.AfterID == 0 {
		if lid := strings.TrimSpace(r.Header.Get("Last-Event-ID")); lid != "" {
			if n, err := strconv.ParseInt(lid, 10, 64); err == nil {
				q.AfterID = n
			}
		}
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // keep nginx (CapRover) from buffering the stream
	sub := s.hub.Subscribe(q)
	defer s.hub.Unsubscribe(sub)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	var lastDropped int64
	for {
		select {
		case <-r.Context().Done():
			return
		case row := <-sub.C:
			b, _ := json.Marshal(row)
			// id: gives the browser its resume cursor — EventSource echoes it
			// back as Last-Event-ID on the next (auto)reconnect.
			fmt.Fprintf(w, "id: %d\n", row.ID)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			if d := sub.Dropped.Load(); d != lastDropped {
				lastDropped = d
				fmt.Fprintf(w, "event: dropped\ndata: %d\n\n", d)
			} else {
				fmt.Fprint(w, ": ping\n\n")
			}
			fl.Flush()
		}
	}
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	rng := time.Hour
	if d, err := config.ParseDuration(v.Get("range")); err == nil && d > 0 {
		rng = d
	}
	bucket := rng.Milliseconds() / 300
	if bucket < 5000 {
		bucket = 5000
	}
	since := time.Now().Add(-rng).UnixMilli()
	pts, err := s.st.QueryMetrics(r.Context(), v.Get("app"), v.Get("container"), v.Get("host"), since, 0, bucket)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"bucket_ms": bucket, "points": pts})
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.st.ListSnapshots(r.Context(), r.URL.Query().Get("app"), r.URL.Query().Get("host"), limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"snapshots": list})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sn, err := s.snapshotEnriched(r.Context(), id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, sn)
}

// snapshotEnriched loads a snapshot and attaches the newest deploy before the
// crash (V9), so both the JSON view and the markdown export answer
// "did a deploy just before this crash?". The lookup is best effort.
func (s *Server) snapshotEnriched(ctx context.Context, id int64) (store.Snapshot, error) {
	sn, err := s.st.GetSnapshot(ctx, id)
	if err != nil {
		return sn, err
	}
	if d, derr := s.st.DeployBefore(ctx, sn.App, sn.TS); derr == nil && d.ID != 0 {
		sn.Deploy = &d
	}
	return sn, nil
}

func (s *Server) snapshotExport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sn, err := s.snapshotEnriched(r.Context(), id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="crash-%s-%d.md"`, sn.App, sn.ID))
	fmt.Fprint(w, SnapshotMarkdown(sn))
}

// SnapshotMarkdown renders a snapshot as an incident-handoff document.
func SnapshotMarkdown(sn store.Snapshot) string {
	var b strings.Builder
	t := time.UnixMilli(sn.TS).UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "# Crash report: %s\n\n", sn.App)
	fmt.Fprintf(&b, "- Time: %s\n- Kind: %s\n- Container: %s\n- Image: %s\n- Exit code: %d\n- OOM killed: %v\n- Restart count: %d\n- Uptime before death: %ds\n",
		t, sn.Kind, sn.Container, sn.Image, sn.ExitCode, sn.OOMKilled, sn.RestartCount, sn.UptimeSec)
	if sn.RestartsWindow >= 2 {
		fmt.Fprintf(&b, "- Deaths in restart window: %d\n", sn.RestartsWindow)
	}
	if sn.MemLimit > 0 {
		fmt.Fprintf(&b, "- Memory limit: %d bytes\n", sn.MemLimit)
	}
	if sn.Error != "" {
		fmt.Fprintf(&b, "- Docker error: %s\n", sn.Error)
	}
	if sn.Deploy != nil {
		gap := sn.TS - sn.Deploy.TS
		fmt.Fprintf(&b, "- Last deploy: %s at %s (%dm before crash)\n", sn.Deploy.Version, time.UnixMilli(sn.Deploy.TS).UTC().Format(time.RFC3339), gap/60000)
	}
	fmt.Fprintf(&b, "\n## Summary\n\n%s\n", sn.Summary)
	if len(sn.Metrics) > 0 {
		fmt.Fprintf(&b, "\n## Resource trend (last %d samples)\n\n| time (UTC) | cpu %% | memory MiB |\n|---|---|---|\n", len(sn.Metrics))
		for _, m := range sn.Metrics {
			fmt.Fprintf(&b, "| %s | %.1f | %.1f |\n", time.UnixMilli(m.TS).UTC().Format("15:04:05"), m.CPU, float64(m.Mem)/1048576)
		}
	}
	fmt.Fprintf(&b, "\n## Last %d log lines\n\n```\n", len(sn.Logs))
	for _, l := range sn.Logs {
		st := "out"
		if l.Stream == "e" {
			st = "err"
		}
		fmt.Fprintf(&b, "%s %s %s\n", time.UnixMilli(l.TSMs).UTC().Format("15:04:05.000"), st, l.Line)
	}
	b.WriteString("```\n")
	return b.String()
}
