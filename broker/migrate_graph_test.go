package main

import "testing"

func TestGraphMigrationAcceptsHistoricalGraphV9(t *testing.T) {
	db := newTestDB(t)
	// A historical development schema already has the additive graph objects
	// under a v9 marker. Adopting v10 must preserve them without duplicate
	// ALTER errors.
	if _, err := db.Exec(`DELETE FROM schema_version WHERE version >= 10`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO graph_extractions(tenant_id,source_memory_id,source_hash,model,status) VALUES('t1','source','fingerprint','model','complete')`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := db.QueryRow(`SELECT source_hash FROM graph_extractions WHERE tenant_id='t1' AND source_memory_id='source'`).Scan(&hash); err != nil || hash != "fingerprint" {
		t.Fatalf("existing graph data changed: %q %v", hash, err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 11 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
