package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// v9 repairs databases whose schema_version records v4–v8 although their
// statements never ran. Such a database can report schema_version 8 with no
// api_keys.principal/role, so every statement that uses those columns failed to
// prepare. The runner keys on MAX(version) and would never revisit v4.
//
// Everything here is idempotent: columns are added only when PRAGMA
// table_info lacks them, and the v4–v8 CREATE ... IF NOT EXISTS statements are
// replayed. On a correctly migrated database v9 changes nothing.
var reconciledColumns = []struct{ table, column, ddl string }{
	{"api_keys", "principal", `ALTER TABLE api_keys ADD COLUMN principal TEXT`},
	{"api_keys", "role", `ALTER TABLE api_keys ADD COLUMN role TEXT NOT NULL DEFAULT 'tenant'`},
	{"usage_events", "silo_id", `ALTER TABLE usage_events ADD COLUMN silo_id TEXT`},
	{"usage_events", "engagement_id", `ALTER TABLE usage_events ADD COLUMN engagement_id TEXT`},
	{"audit_log", "silo_id", `ALTER TABLE audit_log ADD COLUMN silo_id TEXT`},
	{"audit_log", "engagement_id", `ALTER TABLE audit_log ADD COLUMN engagement_id TEXT`},
}

func init() {
	var creates []string
	for _, m := range migrations {
		if m.version < 4 || m.version > 8 {
			continue
		}
		for _, stmt := range m.stmts {
			s := strings.ToUpper(strings.TrimSpace(stmt))
			if strings.HasPrefix(s, "ALTER TABLE") {
				continue // covered by reconciledColumns, guarded by table_info
			}
			if !strings.Contains(s, "IF NOT EXISTS") {
				panic(fmt.Sprintf("v9 reconcile cannot replay non-idempotent v%d statement", m.version))
			}
			creates = append(creates, stmt)
		}
	}
	migrations = append(migrations, migration{
		version:     9,
		description: "Reconcile v3–v8 objects on databases whose schema_version skipped them",
		apply: func(tx *sql.Tx) error {
			for _, c := range reconciledColumns {
				present, err := columnExists(tx, c.table, c.column)
				if err != nil {
					return err
				}
				if !present {
					if _, err := tx.Exec(c.ddl); err != nil {
						return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
					}
				}
			}
			for _, stmt := range creates {
				if _, err := tx.Exec(stmt); err != nil {
					return fmt.Errorf("replay %q: %w", stmt[:min(60, len(stmt))], err)
				}
			}
			return nil
		},
	})
	migrations = append(migrations, graphSchemaMigration)
	migrations = append(migrations, auditAgentIndexMigration)
}

func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
