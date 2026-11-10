package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSchemaBootstrapsFreshBroker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "broker.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply base schema: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("apply in-process migrations: %v", err)
	}

	tenantID, rawKey, err := provisionTenant(context.Background(), db, ProvisionRequest{
		Name:  "Release Smoke",
		Email: "release-smoke@example.invalid",
	})
	if err != nil {
		t.Fatalf("provision smoke tenant: %v", err)
	}
	if tenantID == "" || rawKey == "" {
		t.Fatal("fresh broker provisioning returned an empty tenant or key")
	}
}

// A brand-new self-hosted install has an empty database file: the broker must
// seed the embedded baseline itself, with no manual schema.sql step.
func TestRunMigrationsSeedsEmptyDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations on empty database: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}
	var current int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if want := migrations[len(migrations)-1].version; current != want {
		t.Fatalf("schema_version = %d, want %d", current, want)
	}
	if _, _, err := provisionTenant(context.Background(), db, ProvisionRequest{Name: "Fresh", Email: "fresh@example.invalid"}); err != nil {
		t.Fatalf("provision tenant on seeded database: %v", err)
	}
}

// A database that already has unrelated tables must never be seeded over.
func TestRunMigrationsRefusesForeignDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db); err == nil {
		t.Fatal("runMigrations seeded or accepted a database with foreign tables")
	}
}
