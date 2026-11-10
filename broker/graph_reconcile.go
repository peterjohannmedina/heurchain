package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/redis/go-redis/v9"
)

type graphCallBudgetKey struct{}
type graphCallBudget struct{ remaining int }

// Reconciliation is deliberately independent of stream retention. Persist the
// SSCAN cursor and overflow page in the graph keyspace, so restarts resume the
// sweep and Redis' advisory COUNT cannot silently drop the rest of a page.
func reconcileGraphTenant(ctx context.Context, rdb *redis.Client, db *sql.DB, tenant string) error {
	if !graphTenantEnabled(tenant) {
		return nil
	}
	batch := heurgraphCfg.ReconcileBatchSize
	if batch <= 0 {
		batch = 20
	}
	key := fmt.Sprintf("tenant:%s:heurgraph:reconcile", tenant)
	state, err := rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return err
	}
	cursor, _ := strconv.ParseUint(state["cursor"], 10, 64)
	var pending []string
	if state["pending"] != "" {
		if err = json.Unmarshal([]byte(state["pending"]), &pending); err != nil {
			return fmt.Errorf("reconcile checkpoint: %w", err)
		}
	}
	if len(pending) == 0 {
		pending, cursor, err = rdb.SScan(ctx, vecSetKey(tenant), cursor, "", int64(batch)).Result()
		if err != nil {
			return err
		}
	}
	processed := 0
	checkpoint := func() error {
		rest, _ := json.Marshal(pending[processed:])
		return rdb.HSet(ctx, key, "cursor", cursor, "pending", string(rest)).Err()
	}
	// Save the fetched page before any slow operation. Then advance after each
	// completed attempt, so a later cancellation cannot discard earlier progress.
	if err = checkpoint(); err != nil {
		return err
	}
	for processed < len(pending) && processed < batch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = indexGraphSource(ctx, rdb, db, tenant, pending[processed]); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errGraphBudgetDeferred) {
				break // No attempt was made; keep this source at the head.
			}
			log.Printf("heurgraph: reconciliation tenant=%s doc=%s: %v", tenant, pending[processed], err)
		}
		processed++
		if err = checkpoint(); err != nil {
			return err
		}
	}
	// A separate bounded ledger scan removes deleted/vaulted sources. A source
	// restored later reappears in the active SSCAN and gets a fresh ledger entry.
	rows, err := db.QueryContext(ctx, `SELECT source_memory_id FROM graph_extractions WHERE tenant_id=? AND source_memory_id>? ORDER BY source_memory_id LIMIT ?`, tenant, state["cleanup_cursor"], batch)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	last := ""
	for _, id := range ids {
		_, eligible, e := readGraphSource(ctx, rdb, tenant, id)
		if e != nil {
			return e
		}
		if !eligible {
			if e = removeGraphSource(ctx, rdb, db, tenant, id); e != nil {
				return e
			}
		}
		last = id
	}
	if len(ids) < batch {
		last = ""
	}
	return rdb.HSet(ctx, key, "cleanup_cursor", last).Err()
}
