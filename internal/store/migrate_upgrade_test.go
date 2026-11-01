package store

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

// Reproduces the production crash-loop: a database where the final migration
// (deploys) was already applied by an earlier build but user_version lagged
// behind — e.g. the process died mid-migrate, or an intermediate image shipped
// deploys inside an earlier statement. On restart migrate() re-runs the
// CREATE TABLE and dies with "table deploys already exists". Open() must heal
// such a database instead of failing fatally.
func TestMigrateHealsHalfAppliedDeploys(t *testing.T) {
	path := t.TempDir() + "/half.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	head := len(migrations) - 1 // index of the deploys migration
	for i := 0; i <= head; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("setup mig %d: %v", i, err)
		}
	}
	// Rewind the version so migrate() thinks deploys is still owed, even
	// though the table physically exists.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", head)); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != head {
		t.Fatalf("version=%d err=%v, want %d", v, err, head)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on half-migrated DB failed: %v", err)
	}
	defer st.Close()

	if _, err := st.w.Exec(`INSERT INTO deploys(ts,app,version,note) VALUES(1,'web','v1','')`); err != nil {
		t.Fatalf("deploys insert after heal: %v", err)
	}
	if _, err := st.w.Exec(`UPDATE alerts SET delivery='sent' WHERE 1=0`); err != nil {
		t.Fatalf("alerts.delivery missing after heal: %v", err)
	}
	// Version must land at head afterwards.
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Fatalf("user_version=%d after heal, want %d", v, len(migrations))
	}
}
