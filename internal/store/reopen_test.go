package store

import (
	"path/filepath"
	"testing"
)

func TestReopenExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	_ = s.w.QueryRow("PRAGMA user_version").Scan(&v)
	t.Logf("user_version after first open = %d", v)
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	s2.Close()
}
