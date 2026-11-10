package main

import (
	"database/sql"
	_ "embed"
	"fmt"
	"log"
)

// ---------------------------------------------------------------------------
// Version-gated schema migrations, applied at boot.
//
// Why in-process: operators may run more than one broker node against the same
// database, and re-applying schema.sql by hand guarantees drift (its ALTER
// TABLE statements are not idempotent — SQLite errors on duplicate columns).
// The runner applies each migration exactly once, transactionally, keyed on
// schema_version.
//
// Baseline contract: the runner requires schema_version >= 2 (the v1–v2 base
// in schema.sql, embedded below). A brand-new database with no tables is
// seeded from that baseline automatically; a database that has tables but no
// readable schema_version is refused rather than guessed at. The runner owns v4 and above; version 3 was a hosted-only
// table and is deliberately absent, so existing schema_version rows at 3 are
// simply skipped by the version gate.
// ---------------------------------------------------------------------------

// baseSchema is the v1–v2 SQLite baseline applied to a brand-new database.
//
//go:embed schema.sql
var baseSchema string

// seedEmptyDatabase applies baseSchema when the database has no tables at all,
// so a fresh self-hosted install needs no manual schema step. It never touches
// a database that already has tables.
func seedEmptyDatabase(db *sql.DB) error {
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		return fmt.Errorf("inspect database: %w", err)
	}
	if tables > 0 {
		return nil
	}
	if _, err := db.Exec(baseSchema); err != nil {
		return fmt.Errorf("seed baseline schema: %w", err)
	}
	log.Println("schema: empty database seeded with the embedded baseline")
	return nil
}

type migration struct {
	version     int
	description string
	stmts       []string
	// apply runs after stmts in the same transaction, for steps that must
	// inspect the schema first.
	apply func(*sql.Tx) error
}

// Versions 4-10 belong to features that are not part of the self-hosted OSS
// edition. They stay defined as empty placeholders so the version sequence
// matches the mainline 0.6.1 broker: a database created by either build reports
// the same schema_version, and an upgrade never re-runs or skips a step.
var migrations = func() []migration {
	var ms []migration
	for v := 4; v <= 10; v++ {
		ms = append(ms, migration{version: v, description: "reserved (not in the OSS edition)"})
	}
	return append(ms, auditAgentIndexMigration)
}()

// auditAgentIndexMigration backs /agent-provenance, which groups audit_log by
// agent_id inside a bounded created_at window. idx_audit_tenant only covers
// (tenant_id, created_at), so without this the database filters every row the
// tenant has ever written before grouping. It is the highest defined version.
var auditAgentIndexMigration = migration{
	version:     11,
	description: "index audit_log by tenant + agent + time for agent provenance",
	stmts: []string{
		`CREATE INDEX IF NOT EXISTS idx_audit_tenant_agent ON audit_log(tenant_id, agent_id, created_at)`,
	},
}

// runMigrations applies all pending migrations in version order, each in its
// own transaction. Called at boot before the HTTP server starts; a failed
// migration is fatal (the broker must not serve on a half-migrated schema).
func runMigrations(db *sql.DB) error {
	if err := seedEmptyDatabase(db); err != nil {
		return err
	}
	var current int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	if err != nil {
		return fmt.Errorf("schema_version unreadable — this database has tables but no HeurChain schema version (this runner owns v4 and above): %w", err)
	}
	if current < 2 {
		return fmt.Errorf("schema_version is %d; expected the v1–v2 baseline from schema.sql", current)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration v%d failed at %q: %w", m.version, stmt[:min(60, len(stmt))], err)
			}
		}
		if m.apply != nil {
			if err := m.apply(tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration v%d failed: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_version (version, description) VALUES (?, ?)`,
			m.version, m.description); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d: record version: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration v%d: commit: %w", m.version, err)
		}
		log.Printf("schema migration v%d applied: %s", m.version, m.description)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
