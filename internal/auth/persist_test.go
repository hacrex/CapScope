package auth

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A restart must not log anyone out: the second manager over the same file
// restores the first manager's sessions.
func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capscope.db.sessions")

	m1 := NewPersistentSessionManager(path, quietLogger())
	token := m1.CreateSession(1, "admin", RoleAdmin)
	userID, name, role, ok := m1.Validate(token)
	if !ok || name != "admin" || role != RoleAdmin || userID != 1 {
		t.Fatalf("fresh token invalid: %d %q %q %v", userID, name, role, ok)
	}

	m2 := NewPersistentSessionManager(path, quietLogger())
	userID, name, role, ok = m2.Validate(token)
	if !ok || name != "admin" || role != RoleAdmin || userID != 1 {
		t.Fatalf("session did not survive the restart: %d %q %q %v", userID, name, role, ok)
	}
	// A brand-new token from the restored manager works and lands in the file.
	token2 := m2.CreateSession(2, "viewer", RoleViewer)
	_, _, _, ok = NewPersistentSessionManager(path, quietLogger()).Validate(token2)
	if !ok {
		t.Fatal("third manager lost the newest token")
	}
}

func TestSessionsDeletedTokenStopsValidating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sessions")
	m := NewPersistentSessionManager(path, quietLogger())
	token := m.CreateSession(1, "admin", RoleAdmin)
	m.Delete(token)
	if _, _, _, ok := m.Validate(token); ok {
		t.Fatal("deleted token still valid")
	}
	if _, _, _, ok := NewPersistentSessionManager(path, quietLogger()).Validate(token); ok {
		t.Fatal("logout not persisted")
	}
}

func TestRevokeUserStopsAllSessions(t *testing.T) {
	m := NewSessionManager(quietLogger())
	token1 := m.CreateSession(7, "viewer", RoleViewer)
	token2 := m.CreateSession(7, "viewer", RoleViewer)
	other := m.CreateSession(8, "other", RoleViewer)
	m.RevokeUser(7)
	if _, _, _, ok := m.Validate(token1); ok {
		t.Fatal("first user session still valid after revocation")
	}
	if _, _, _, ok := m.Validate(token2); ok {
		t.Fatal("second user session still valid after revocation")
	}
	if _, _, _, ok := m.Validate(other); !ok {
		t.Fatal("revocation affected another user")
	}
}

func TestExpiredSessionsNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sessions")
	m := NewPersistentSessionManager(path, quietLogger())
	token := m.CreateSession(1, "admin", RoleAdmin)

	// Age the session past its TTL by editing the backing file directly.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Key      string `json:"key"`
		Sessions map[string]struct {
			UserID  int64  `json:"user_id"`
			User    string `json:"user"`
			Role    string `json:"role"`
			Expires int64  `json:"expires"`
		} `json:"sessions"`
	}
	if json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for k, s := range f.Sessions {
		s.Expires = time.Now().Add(-time.Hour).Unix()
		f.Sessions[k] = s
	}
	nb, _ := json.Marshal(&f)
	if err := os.WriteFile(path, nb, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, _, ok := NewPersistentSessionManager(path, quietLogger()).Validate(token); ok {
		t.Fatal("expired session was restored")
	}
}

func TestMemoryOnlyManagerIsIsolated(t *testing.T) {
	m := NewSessionManager(quietLogger())
	token := m.CreateSession(1, "admin", RoleAdmin)
	if _, _, _, ok := m.Validate(token); !ok {
		t.Fatal("same-manager token invalid")
	}
	if _, _, _, ok := NewPersistentSessionManager("", quietLogger()).Validate(token); ok {
		t.Fatal("memory-only sessions must not leak into other managers")
	}
}

// The backing file must never contain the raw bearer token itself — only
// HMAC digests — so a leaked sessions file alone cannot mint a login.
func TestSessionFileHasNoRawTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sessions")
	m := NewPersistentSessionManager(path, quietLogger())
	token := m.CreateSession(1, "admin", RoleAdmin)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); len(token) > 0 && containsHex(got, token) {
		t.Fatal("raw bearer token present in the session file")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestBrokenSessionFileStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.sessions")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewPersistentSessionManager(path, quietLogger())
	token := m.CreateSession(1, "admin", RoleAdmin)
	if _, _, _, ok := m.Validate(token); !ok {
		t.Fatal("fresh session invalid after broken file recovery")
	}
	// The file is now valid JSON with the new key.
	var f map[string]any
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &f) != nil || f["key"] == "" {
		t.Fatalf("session file not rewritten: %v %v %v", b, err, f)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func containsHex(haystack, needle string) bool {
	return len(needle) > 0 && len(needle) <= len(haystack) && strings.Contains(haystack, needle)
}
