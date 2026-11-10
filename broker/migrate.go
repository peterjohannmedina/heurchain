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
	// inspect the schema first (see migrate_reconcile.go).
	apply func(*sql.Tx) error
}

var migrations = []migration{
	{
		version:     4,
		description: "Cryptographic client silos: silos, silo_keys (wrapped DEKs), silo_grants; principal/role on api_keys",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS silos (
				id              TEXT PRIMARY KEY,
				tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
				name            TEXT NOT NULL,
				description     TEXT,
				isolation       TEXT NOT NULL DEFAULT 'isolated' CHECK (isolation IN ('pooled','isolated','quarantine')),
				crypto          TEXT NOT NULL DEFAULT 'firm' CHECK (crypto IN ('firm','byok')),
				shard_mode      TEXT NOT NULL DEFAULT 'dedicated' CHECK (shard_mode IN ('dedicated','shared')),
				status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','shredded')),
				retention_days  INTEGER,
				created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
				shredded_at     DATETIME,
				UNIQUE(tenant_id, name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_silos_tenant ON silos(tenant_id, status)`,
			`CREATE TABLE IF NOT EXISTS silo_keys (
				silo_id         TEXT PRIMARY KEY REFERENCES silos(id) ON DELETE CASCADE,
				wrapped_dek     BLOB NOT NULL,
				kek_id          TEXT NOT NULL,
				key_version     INTEGER NOT NULL DEFAULT 1,
				dek_fingerprint TEXT NOT NULL,
				created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`ALTER TABLE api_keys ADD COLUMN principal TEXT`,
			`ALTER TABLE api_keys ADD COLUMN role TEXT NOT NULL DEFAULT 'tenant'`,
			`CREATE TABLE IF NOT EXISTS silo_grants (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				key_id          INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
				silo_id         TEXT NOT NULL REFERENCES silos(id) ON DELETE CASCADE,
				perm            TEXT NOT NULL CHECK (perm IN ('read','write','admin')),
				created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
				revoked_at      DATETIME,
				UNIQUE(key_id, silo_id, perm)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_silo_grants_silo ON silo_grants(silo_id)`,
		},
	},
	{
		version:     5,
		description: "Engagements: contract boundary, consultant staffing, checksum-chained lifecycle audit",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS engagements (
				id              TEXT PRIMARY KEY,
				tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
				customer_id     TEXT NOT NULL,
				contract_ref    TEXT,
				silo_id         TEXT NOT NULL REFERENCES silos(id),
				status          TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','paused','closed','shred')),
				starts_at       TEXT,
				ends_at         TEXT,
				created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_engagements_tenant ON engagements(tenant_id, status)`,
			`CREATE INDEX IF NOT EXISTS idx_engagements_silo ON engagements(silo_id, status)`,
			`CREATE TABLE IF NOT EXISTS engagement_consultants (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				engagement_id   TEXT NOT NULL REFERENCES engagements(id) ON DELETE CASCADE,
				key_id          INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
				principal       TEXT NOT NULL,
				role            TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('lead','member','reviewer')),
				added_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
				removed_at      DATETIME,
				UNIQUE(engagement_id, key_id)
			)`,
			`CREATE TABLE IF NOT EXISTS engagement_audit (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				engagement_id   TEXT NOT NULL REFERENCES engagements(id),
				actor           TEXT NOT NULL,
				from_status     TEXT,
				to_status       TEXT NOT NULL,
				reason          TEXT,
				prev_checksum   TEXT,
				checksum        TEXT NOT NULL,
				payload_json    TEXT NOT NULL,
				created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_engagement_audit_eng ON engagement_audit(engagement_id, id)`,
		},
	},
	{
		version:     6,
		description: "silo_id + engagement_id telemetry columns on usage_events and audit_log",
		stmts: []string{
			`ALTER TABLE usage_events ADD COLUMN silo_id TEXT`,
			`ALTER TABLE usage_events ADD COLUMN engagement_id TEXT`,
			`ALTER TABLE audit_log ADD COLUMN silo_id TEXT`,
			`ALTER TABLE audit_log ADD COLUMN engagement_id TEXT`,
			`CREATE INDEX IF NOT EXISTS idx_usage_engagement ON usage_events(tenant_id, engagement_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_silo ON audit_log(tenant_id, silo_id, created_at)`,
		},
	},
	{
		version:     7,
		description: "Trust artifacts: isolation_certificates, destruction_certificates, silo_shred_tombstones",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS isolation_certificates (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				tenant_id       TEXT NOT NULL,
				silo_id         TEXT NOT NULL REFERENCES silos(id),
				engagement_id   TEXT,
				version         INTEGER NOT NULL,
				dek_fingerprint TEXT NOT NULL,
				payload_json    TEXT NOT NULL,
				sig             TEXT NOT NULL,
				pubkey          TEXT NOT NULL,
				created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
				UNIQUE(silo_id, version)
			)`,
			`CREATE TABLE IF NOT EXISTS destruction_certificates (
				id                    INTEGER PRIMARY KEY AUTOINCREMENT,
				tenant_id             TEXT NOT NULL,
				silo_id               TEXT NOT NULL,
				destroyed_fingerprint TEXT NOT NULL,
				payload_json          TEXT NOT NULL,
				sig                   TEXT NOT NULL,
				pubkey                TEXT NOT NULL,
				created_at            DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			// replayed by scripts/restore-heurchain.sh after any backup restore
			`CREATE TABLE IF NOT EXISTS silo_shred_tombstones (
				silo_id         TEXT PRIMARY KEY,
				tenant_id       TEXT NOT NULL,
				dek_fingerprint TEXT NOT NULL,
				shredded_at     DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
		},
	},

	{
		version:     8,
		description: "heurgraph: entity/relation/alias tables for knowledge-graph memory index",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS graph_entities (
				id         TEXT NOT NULL,
				tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
				name       TEXT NOT NULL,
				type       TEXT NOT NULL,
				summary    TEXT,
				degree     INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at TEXT NOT NULL DEFAULT (datetime('now')),
				PRIMARY KEY (tenant_id, id))`,
			`CREATE INDEX IF NOT EXISTS idx_gent_name ON graph_entities(tenant_id, name)`,
			`CREATE INDEX IF NOT EXISTS idx_gent_type ON graph_entities(tenant_id, type)`,
			`CREATE TABLE IF NOT EXISTS graph_aliases (
				tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
				entity_id TEXT NOT NULL,
				alias     TEXT NOT NULL,
				PRIMARY KEY (tenant_id, alias),
				FOREIGN KEY (tenant_id, entity_id) REFERENCES graph_entities(tenant_id, id) ON DELETE CASCADE)`,
			`CREATE INDEX IF NOT EXISTS idx_galias_entity ON graph_aliases(tenant_id, entity_id)`,
			`CREATE TABLE IF NOT EXISTS graph_relations (
				id                TEXT NOT NULL,
				tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
				source_id         TEXT NOT NULL,
				target_id         TEXT NOT NULL,
				predicate         TEXT NOT NULL,
				source_memory_id  TEXT NOT NULL,
				source_session_id TEXT,
				agent_id          TEXT,
				namespace         TEXT NOT NULL DEFAULT 'notes',
				src_access_count  INTEGER NOT NULL DEFAULT 0,
				src_created_at    TEXT NOT NULL,
				src_vaulted       INTEGER NOT NULL DEFAULT 0,
				-- Sentinel below any sane activation_cutoff so an edge inserted
				-- without being scored is skipped by traversal rather than ranked
				-- top (0 would read as maximally active).
				edge_activation   REAL NOT NULL DEFAULT -999,
				activation_at     TEXT NOT NULL DEFAULT (datetime('now')),
				created_at        TEXT NOT NULL DEFAULT (datetime('now')),
				PRIMARY KEY (tenant_id, id),
				FOREIGN KEY (tenant_id, source_id) REFERENCES graph_entities(tenant_id, id) ON DELETE CASCADE,
				FOREIGN KEY (tenant_id, target_id) REFERENCES graph_entities(tenant_id, id) ON DELETE CASCADE)`,
			`CREATE INDEX IF NOT EXISTS idx_grel_source ON graph_relations(tenant_id, source_id)`,
			`CREATE INDEX IF NOT EXISTS idx_grel_target ON graph_relations(tenant_id, target_id)`,
			`CREATE INDEX IF NOT EXISTS idx_grel_mem    ON graph_relations(tenant_id, source_memory_id)`,
			`CREATE INDEX IF NOT EXISTS idx_grel_act    ON graph_relations(tenant_id, edge_activation)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_grel_idem ON graph_relations(tenant_id, source_memory_id, predicate, source_id, target_id)`,
		},
	},
}

// auditAgentIndexMigration backs /agent-provenance, which groups audit_log by
// agent_id inside a bounded created_at window. idx_audit_tenant only covers
// (tenant_id, created_at), so without this the database filters every row the
// tenant has ever written before grouping. Appended last in migrate_reconcile's
// init so it stays the highest defined version.
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
