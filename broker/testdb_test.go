package main

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestDB returns an in-memory SQLite seeded from the embedded baseline and
// migrated to the latest version, with one tenant ("t1") and one API key.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO tenants (id, name, email) VALUES ('t1', 'Sigma', 'ops@sigma.test')`,
		`INSERT INTO api_keys (tenant_id, key_hash, name) VALUES ('t1', 'x', 'tenant-key')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
	return db
}

func TestMigrationsApplyOnceAndAreIdempotent(t *testing.T) {
	db := newTestDB(t)
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if want := migrations[len(migrations)-1].version; v != want {
		t.Fatalf("schema version = %d, want %d (highest defined migration)", v, want)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}
}
