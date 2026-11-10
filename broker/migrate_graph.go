package main

import "database/sql"

// v9 belongs to master schema reconciliation. Graph provenance follows it at
// v10, including on databases already stamped v9. Guards also accept a
// historical development schema that used v9 for graph provenance.
var graphSchemaMigration = migration{
	version:     10,
	description: "heurgraph: source fingerprint, verbatim evidence and extraction ledger",
	apply: func(tx *sql.Tx) error {
		for _, column := range []struct{ name, ddl string }{
			{"source_hash", "ALTER TABLE graph_relations ADD COLUMN source_hash TEXT NOT NULL DEFAULT ''"},
			{"evidence", "ALTER TABLE graph_relations ADD COLUMN evidence TEXT NOT NULL DEFAULT ''"},
		} {
			present, err := columnExists(tx, "graph_relations", column.name)
			if err != nil {
				return err
			}
			if !present {
				if _, err := tx.Exec(column.ddl); err != nil {
					return err
				}
			}
		}
		for _, statement := range []string{
			"CREATE INDEX IF NOT EXISTS idx_grel_source_order ON graph_relations(tenant_id,source_id,id)",
			"CREATE INDEX IF NOT EXISTS idx_grel_target_order ON graph_relations(tenant_id,target_id,id)",
			"CREATE INDEX IF NOT EXISTS idx_gent_lower_name ON graph_entities(tenant_id,lower(name))",
			"CREATE INDEX IF NOT EXISTS idx_galias_lower_alias ON graph_aliases(tenant_id,lower(alias))",
			`CREATE TABLE IF NOT EXISTS graph_extractions (
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    source_memory_id TEXT NOT NULL, source_hash TEXT NOT NULL, model TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status = 'complete'), updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (tenant_id, source_memory_id))`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	},
}
