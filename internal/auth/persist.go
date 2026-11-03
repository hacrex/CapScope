package auth

// Backing-file layer for persistent sessions (roadmap Phase 3.1). The file
// sits next to the SQLite database on the same persistent volume and holds:
//   - the HMAC key itself (so the HMACs stay stable across restarts), and
//   - the active sessions as HMAC(token) → {user, expiry}.
//
// Threat model: the file is on the data disk (0600) but contains no bearer
// tokens, only keyed digests — stealing the file alone gives neither a valid
// login nor a way to forge one (knowledge of the token is required; the digests
// are compared, not derived). Deleting the file is always the recovery path:
// CapScope regenerates the key and everyone logs in again.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"time"
)

const (
	maxPersisted = 500     // sessions kept in the backing file
	fileLimit    = 4 << 20 // refuse to read files larger than 4 MiB
)

type persistSession struct {
	UserID  int64  `json:"user_id"`
	User    string `json:"user"`
	Role    string `json:"role"`
	Expires int64  `json:"expires"` // unix seconds
}

type persistFile struct {
	Key      string                    `json:"key"`      // HMAC key, hex
	Sessions map[string]persistSession `json:"sessions"` // HMAC(token) → session
}

// load reads the backing file into memory, creating it when missing. It is
// called once from NewPersistentSessionManager.
func (m *SessionManager) load() {
	if m.path == "" {
		return
	}
	b, err := os.ReadFile(m.path)
	switch {
	case err == nil && len(b) > fileLimit:
		m.log.Warn("auth: session file too large, starting fresh", "path", m.path)
		return
	case err != nil:
		if !os.IsNotExist(err) {
			m.log.Warn("auth: cannot read session file, starting fresh", "path", m.path, "err", err)
		}
		// First boot (or deleted file): create key + empty snapshot right away.
		m.keyBytes()
		m.save()
		return
	}
	var f persistFile
	if err := json.Unmarshal(b, &f); err != nil || f.Key == "" {
		m.log.Warn("auth: session file unreadable, starting fresh", "path", m.path, "err", err)
		m.keyBytes()
		m.save()
		return
	}
	key, err := hex.DecodeString(f.Key)
	if err != nil || len(key) < 16 {
		m.log.Warn("auth: session file has a broken key, regenerating", "path", m.path)
		m.keyBytes()
		m.save()
		return
	}
	m.key = key
	now := time.Now()
	if m.sessions == nil {
		m.sessions = map[string]*session{}
	}
	for mac, s := range f.Sessions {
		if mac == "" || s.User == "" {
			continue
		}
		exp := time.Unix(s.Expires, 0)
		if now.After(exp) {
			continue // expired while we were down
		}
		m.sessions[mac] = &session{userID: s.UserID, name: s.User, role: s.Role, expires: exp}
	}
	if n := len(m.sessions); n > 0 {
		m.log.Info("auth: sessions restored from previous run", "count", n)
	}
}

// save atomically renders the current state to the backing file. Called on
// session create/delete/expiry; the file is tiny, so full rewrites are fine.
// Lockless by design: callers hold nothing — merge under RLock.
func (m *SessionManager) save() {
	if m.path == "" {
		return
	}
	f := persistFile{Sessions: map[string]persistSession{}}
	m.mu.RLock()
	f.Key = hex.EncodeToString(m.keyBytes())
	now := time.Now()
	n := 0
	for mac, s := range m.sessions {
		if n >= maxPersisted || now.After(s.expires) {
			continue
		}
		f.Sessions[mac] = persistSession{UserID: s.userID, User: s.name, Role: s.role, Expires: s.expires.Unix()}
		n++
	}
	m.mu.RUnlock()
	b, err := json.Marshal(&f)
	if err != nil {
		m.log.Warn("auth: cannot marshal session file", "err", err)
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		m.log.Warn("auth: cannot write session file", "path", m.path, "err", err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		m.log.Warn("auth: cannot rotate session file into place", "err", err)
	}
}
