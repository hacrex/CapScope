// Package auth manages login sessions for the CapScope UI.
//
// Sessions are bearer tokens issued after a successful Basic-style credential
// check against AUTH_USER / AUTH_PASS. Tokens are held in memory (CapScope is
// a single-instance tool — see deploy docs: replica count must be 1) and expire
// after 24 hours. A background reaper sweeps expired tokens hourly.
//
// With an optional backing file (NewPersistentSessionManager), sessions also
// survive restarts: they are stored HMAC-signed next to the database, and the
// key never leaves the host. See persist.go for the threat model.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	sessionLength = 32 // random bytes per token
	ttl           = 24 * time.Hour
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrNotAuthenticated   = errors.New("not authenticated")
)

// Role values.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// SessionManager tracks active login sessions. Safe for concurrent use.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*session // HMAC(token) → session
	key      []byte              // HMAC key from the backing file (or fresh)
	path     string              // backing file, "" = memory only
	log      *slog.Logger
}

// session is one active login, keyed by HMAC(token) so the backing file never
// holds a usable bearer credential.
type session struct {
	userID  int64
	name    string
	role    string
	expires time.Time
}

// NewSessionManager creates a memory-only manager and starts its expiry reaper.
func NewSessionManager(log *slog.Logger) *SessionManager {
	return NewPersistentSessionManager("", log)
}

// NewPersistentSessionManager creates a session manager whose sessions are
// additionally restored from — and written to — the backing file at path.
// An empty path falls back to memory-only state (tests, proxy mode, auth off).
// The reaper and the session file keep expired entries out automatically.
func NewPersistentSessionManager(path string, log *slog.Logger) *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*session),
		path:     path,
		log:      log,
	}
	m.load()
	// The HMAC key is created once at construction (from the backing file or
	// fresh entropy) and never changes afterwards. Publishing it before the
	// reaper goroutine starts, and never writing it again, makes keyBytes a
	// plain read and removes the data race on concurrent logins/validations.
	if m.key == nil {
		m.mu.Lock()
		if m.key == nil {
			m.key = genKey()
		}
		m.mu.Unlock()
	}
	go m.reap()
	return m
}

// genKey returns a fresh 32-byte HMAC key. It degrades to the wall clock only
// when the OS cannot supply entropy; that is a fatal configuration problem but
// we never panic for it.
func genKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return []byte(time.Now().String() + "capscope")
	}
	return b
}

// keyBytes returns the signing key. m.key is immutable after construction (set
// once under m.mu above), so concurrent readers are safe — no lock needed,
// which matters because mac() is called while callers already hold m.mu.
func (m *SessionManager) keyBytes() []byte {
	return m.key
}

// mac returns the persistent identity of a raw bearer token: HMAC-SHA256.
// Raw tokens are never stored, so a leaked session file cannot be replayed.
func (m *SessionManager) mac(token string) string {
	h := hmac.New(sha256.New, m.keyBytes())
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}

// reap deletes expired sessions once an hour and rewrites the backing file.
func (m *SessionManager) reap() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		now := time.Now()
		changed := false
		for mac, s := range m.sessions {
			if now.After(s.expires) {
				delete(m.sessions, mac)
				changed = true
			}
		}
		m.mu.Unlock()
		if changed {
			m.save()
		}
	}
}

// CreateSession issues a new token for userID/name/role and returns it.
// Fails closed when crypto/rand cannot supply entropy: a predictable bearer
// token would be an authentication bypass.
func (m *SessionManager) CreateSession(userID int64, name, role string) (string, error) {
	b := make([]byte, sessionLength)
	if _, err := rand.Read(b); err != nil {
		m.log.Error("auth: crypto/rand failed, refusing to create session", "err", err)
		return "", err
	}
	token := hex.EncodeToString(b)
	m.mu.Lock()
	m.sessions[m.mac(token)] = &session{userID: userID, name: name, role: role, expires: time.Now().Add(ttl)}
	m.mu.Unlock()
	m.save()
	return token, nil
}

// Validate checks a token and returns (userID, name, role, ok). Returns ok=false
// when the token is missing, unknown, or expired (expired tokens are removed).
func (m *SessionManager) Validate(token string) (int64, string, string, bool) {
	if token == "" {
		return 0, "", "", false
	}
	mac := m.mac(token)
	m.mu.RLock()
	s, ok := m.sessions[mac]
	m.mu.RUnlock()
	if !ok {
		return 0, "", "", false
	}
	if time.Now().After(s.expires) {
		m.mu.Lock()
		delete(m.sessions, mac)
		m.mu.Unlock()
		m.save()
		return 0, "", "", false
	}
	return s.userID, s.name, s.role, true
}

// Expiry returns the session's expiration time as a Unix timestamp, or 0 if not found/expired.
func (m *SessionManager) Expiry(token string) int64 {
	m.mu.RLock()
	s, ok := m.sessions[m.mac(token)]
	m.mu.RUnlock()
	if !ok || time.Now().After(s.expires) {
		return 0
	}
	return s.expires.Unix()
}

// Delete removes a session (logout).
func (m *SessionManager) Delete(token string) {
	mac := m.mac(token)
	m.mu.Lock()
	_, ok := m.sessions[mac]
	delete(m.sessions, mac)
	m.mu.Unlock()
	if ok {
		m.save()
	}
}

// RevokeUser invalidates every active session belonging to userID. This keeps
// demoted or deleted users from retaining their previous access until expiry.
func (m *SessionManager) RevokeUser(userID int64) {
	if userID == 0 {
		return
	}
	m.mu.Lock()
	changed := false
	for mac, s := range m.sessions {
		if s.userID == userID {
			delete(m.sessions, mac)
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.save()
	}
}

// Name returns the session name for a token, or "" if invalid.
func (m *SessionManager) Name(token string) string {
	_, name, _, ok := m.Validate(token)
	if !ok {
		return ""
	}
	return name
}

// Role returns the session role for a token, or "" if invalid.
func (m *SessionManager) Role(token string) string {
	_, _, role, ok := m.Validate(token)
	if !ok {
		return ""
	}
	return role
}

// UserID returns the session user ID for a token, or 0 if invalid.
func (m *SessionManager) UserID(token string) int64 {
	userID, _, _, ok := m.Validate(token)
	if !ok {
		return 0
	}
	return userID
}
