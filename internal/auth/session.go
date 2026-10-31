// Package auth manages login sessions for the CapScope UI.
//
// Sessions are bearer tokens issued after a successful Basic-style credential
// check against AUTH_USER / AUTH_PASS. Tokens are held in memory (CapScope is
// a single-instance tool — see deploy docs: replica count must be 1) and expire
// after 24 hours. A background reaper sweeps expired tokens hourly.
package auth

import (
	"crypto/rand"
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

// SessionManager tracks active login sessions. Safe for concurrent use.
type SessionManager struct {
	mu      sync.RWMutex
	sessions map[string]*session
	log     *slog.Logger
}

type session struct {
	user    string
	expires time.Time
}

// NewSessionManager creates a manager and starts its expiry reaper.
func NewSessionManager(log *slog.Logger) *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*session),
		log:      log,
	}
	go m.reap()
	return m
}

// reap deletes expired sessions once an hour.
func (m *SessionManager) reap() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		now := time.Now()
		for token, s := range m.sessions {
			if now.After(s.expires) {
				delete(m.sessions, token)
			}
		}
		m.mu.Unlock()
	}
}

// CreateSession issues a new token for user and returns it.
func (m *SessionManager) CreateSession(user string) string {
	b := make([]byte, sessionLength)
	if _, err := rand.Read(b); err != nil {
		m.log.Warn("auth: crypto/rand failed, falling back to time-based token", "err", err)
		b = []byte(time.Now().String() + user)
	}
	token := hex.EncodeToString(b)
	m.mu.Lock()
	m.sessions[token] = &session{user: user, expires: time.Now().Add(ttl)}
	m.mu.Unlock()
	return token
}

// Validate checks a token and returns the owning user, or "" and false when
// the token is missing, unknown, or expired (expired tokens are removed).
func (m *SessionManager) Validate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	m.mu.RLock()
	s, ok := m.sessions[token]
	m.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(s.expires) {
		m.mu.Lock()
		delete(m.sessions, token)
		m.mu.Unlock()
		return "", false
	}
	return s.user, true
}

// Delete removes a session (logout).
func (m *SessionManager) Delete(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}
