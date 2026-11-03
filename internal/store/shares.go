package store

// Share links (V10): expiring permalinks. One row per generated link; the
// payload is the query-string view (tab/app/range/filter and, optionally, the
// crash hash) the link restores. A link "stops resolving" after expiry —
// getShare reports unknown for expired rows, and creation sweeps old ones.
// The token itself is the capability: /s/<token> is reachable without a
// session (the SPA will still demand a login for the actual data).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

type Share struct {
	Token     string `json:"token"`
	CreatedMs int64  `json:"created_ms"`
	ExpiresMs int64  `json:"expires_ms"`
	QS        string `json:"qs"` // view state as a URL query string, no leading '?'
	Hash      string `json:"hash,omitempty"`
}

// shareTTL clamps the requested lifetime. Negative TTLs are a test hook:
// they create already-expired rows so expiry behavior needs no clock mocks.
func shareTTL(ttl time.Duration) time.Duration {
	if ttl < 0 {
		return ttl
	}
	return max(ttl, time.Minute)
}

func (s *Store) CreateShare(ctx context.Context, qs, hash string, ttl time.Duration) (Share, error) {
	if qs == "" && hash == "" {
		return Share{}, fmt.Errorf("share: nothing to share")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Share{}, err
	}
	share := Share{
		Token:     hex.EncodeToString(b),
		CreatedMs: time.Now().UnixMilli(),
		ExpiresMs: time.Now().Add(shareTTL(ttl)).UnixMilli(),
		QS:        qs,
		Hash:      hash,
	}
	// Opportunistic hygiene: expired links are dropped whenever a new one is
	// created (no background reaper needed for a table-sized feature).
	_, _ = s.w.ExecContext(ctx, `DELETE FROM shares WHERE expires_ms < ?`, share.CreatedMs)
	res, err := s.w.ExecContext(ctx,
		`INSERT INTO shares(token, created_ms, expires_ms, qs, hash) VALUES(?,?,?,?,?)`,
		share.Token, share.CreatedMs, share.ExpiresMs, share.QS, share.Hash)
	if err != nil {
		return Share{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Share{}, fmt.Errorf("share: insert affected %d rows", n)
	}
	return share, nil
}

// GetShare resolves one link by token. Unknown and expired are the same case.
func (s *Store) GetShare(ctx context.Context, token string) (Share, error) {
	if token == "" {
		return Share{}, sql.ErrNoRows
	}
	row := s.r.QueryRowContext(ctx,
		`SELECT token, created_ms, expires_ms, qs, hash FROM shares WHERE token=?`, token)
	var sh Share
	if err := row.Scan(&sh.Token, &sh.CreatedMs, &sh.ExpiresMs, &sh.QS, &sh.Hash); err != nil {
		return Share{}, err
	}
	if time.Now().UnixMilli() >= sh.ExpiresMs {
		return Share{}, sql.ErrNoRows // expired: stop resolving
	}
	return sh, nil
}
