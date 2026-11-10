package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// A drifted database: the v1–v2 base, with v4–v8 recorded in schema_version
// but never applied, so api_keys has no principal/role columns.
func TestReconcileRepairsSkippedMigrations(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	for v := 4; v <= 8; v++ {
		if _, err := db.Exec(`INSERT INTO schema_version (version, description) VALUES (?, 'stamped, never applied')`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO tenants (id, name, email) VALUES ('t1', 'Drift', 'drift@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (tenant_id, key_hash, name) VALUES ('t1', 'existing-hash', 'default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (tenant_id, key_hash, name, principal, role) VALUES ('t1', 'principal-fixture', 'x', 'p', 'mcp')`); err == nil {
		t.Fatal("fixture should reproduce the missing principal/role columns")
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range reconciledColumns {
		if ok, err := columnExists(tx, c.table, c.column); err != nil || !ok {
			t.Fatalf("%s.%s not reconciled (err=%v)", c.table, c.column, err)
		}
	}
	_ = tx.Rollback()
	for _, table := range []string{"silos", "silo_keys", "silo_grants", "engagements", "engagement_audit", "isolation_certificates", "graph_entities", "graph_relations"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s not reconciled", table)
		}
	}
	var role string
	if err := db.QueryRow(`SELECT role FROM api_keys WHERE key_hash = 'existing-hash'`).Scan(&role); err != nil || role != "tenant" {
		t.Fatalf("existing key role = %q (err=%v), want tenant", role, err)
	}
	// The principal/role statements now prepare and run.
	if _, err := db.Exec(`INSERT INTO api_keys (tenant_id, key_hash, name, principal, role) VALUES ('t1', 'principal-fixture', 'x', 'p', 'mcp')`); err != nil {
		t.Fatalf("principal insert after reconcile: %v", err)
	}
	var id int64
	var tenant, owner string
	var revoked bool
	if err := db.QueryRow(`SELECT id, tenant_id, COALESCE(principal, ''), revoked_at IS NOT NULL FROM api_keys
		WHERE key_hash = ? ORDER BY revoked_at IS NOT NULL, id DESC LIMIT 1`, "principal-fixture").Scan(&id, &tenant, &owner, &revoked); err != nil || owner != "p" {
		t.Fatalf("principal lookup after reconcile: owner=%q err=%v", owner, err)
	}
	// Re-running is a no-op.
	if err := runMigrations(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var max int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&max); err != nil || max != 11 {
		t.Fatalf("schema_version = %d (err=%v), want 11", max, err)
	}
}
