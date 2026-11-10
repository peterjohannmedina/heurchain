package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// HeurGraph — per-tenant knowledge-graph index over vault/vec documents.
//
// Hooks:
//   Write path:  XADD to tenant:{t}:heurgraph:pending stream at handleStore
//   Recall path: optional graph leg in runRecallSearchWithGraph and /graph/ask.
//   Consolidation: batched edge-activation re-score at promotion boundary.
//
// Extraction is implemented but disabled by default. Both the global flag and
// an explicit tenant allowlist gate model calls and graph retrieval. Sources
// remain canonical in Redis; graph citations revalidate source fingerprints.
//
// Hard rule: heurgraph NEVER mutates the corpus. No HSet/Del/SRem against
// vec:/vault: keys, no buildMarkdown, no /vault/export. It is a read-only
// index that lives entirely in its own keyspace and SQLite tables.
// ---------------------------------------------------------------------------

// heurgraphPendingKey returns the Redis stream key for pending extraction items.
func heurgraphPendingKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:heurgraph:pending", tenantID)
}

// heurgraphAdjKey returns the Redis ZSET key for entity adjacency.
func heurgraphAdjKey(tenantID, entityID string) string {
	return fmt.Sprintf("tenant:%s:graph:adj:%s", tenantID, entityID)
}

// heurgraphEntKey returns the Redis HASH key for entity metadata cache.
func heurgraphEntKey(tenantID, entityID string) string {
	return fmt.Sprintf("tenant:%s:graph:ent:%s", tenantID, entityID)
}

// heurgraphStreamMaxLen caps the pending stream. XACK only clears the
// consumer group's pending-entries list, not the stream itself, so the
// MAXLEN on XADD is the only thing preventing unbounded growth on the
// 1.5 GB-capped Redis node.
const heurgraphStreamMaxLen = 100000

// errPermanent marks an extraction failure that must not be retried:
// a malformed stream entry, or a source doc that no longer exists.
// Retryable failures (LLM timeout, network, Redis) are left unacked so a
// later tick reclaims the entry from the pending-entries list.
var errPermanent = errors.New("permanent failure")

// Implementation readiness is separate from the default-off runtime rollout gate.
const heurgraphExtractionReady = true

// ---------------------------------------------------------------------------
// Pending-item types (written to the stream, consumed by the worker)
// ---------------------------------------------------------------------------

type heurgraphPendingItem struct {
	DocID     string `json:"doc_id"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
	Namespace string `json:"namespace"`
}

// ---------------------------------------------------------------------------
// Extraction result types (produced by the LLM, stored in SQLite)
// ---------------------------------------------------------------------------

type graphEntity struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Summary  string `json:"summary,omitempty"`
}

type graphAlias struct {
	TenantID string `json:"tenant_id"`
	EntityID string `json:"entity_id"`
	Alias    string `json:"alias"`
}

type graphRelation struct {
	ID              string  `json:"id"`
	TenantID        string  `json:"tenant_id"`
	SourceID        string  `json:"source_id"`
	TargetID        string  `json:"target_id"`
	Predicate       string  `json:"predicate"`
	SourceMemoryID  string  `json:"source_memory_id"`
	SourceSessionID string  `json:"source_session_id,omitempty"`
	AgentID         string  `json:"agent_id,omitempty"`
	Namespace       string  `json:"namespace"`
	SrcAccessCount  int     `json:"src_access_count"`
	SrcCreatedAt    string  `json:"src_created_at"`
	SrcVaulted      bool    `json:"src_vaulted"`
	EdgeActivation  float64 `json:"edge_activation"`
}

// ---------------------------------------------------------------------------
// Write-path trigger — called from handleStore after storeVec+updateBM25Index
// ---------------------------------------------------------------------------

// queueHeurgraphExtraction adds a doc ID to the pending extraction stream.
// Must be O(1) — no LLM calls, no blocking I/O beyond the XADD itself.
// Silo-path stores are skipped by the caller (encrypted content has no
// plaintext to extract).
func queueHeurgraphExtraction(ctx context.Context, rdb *redis.Client, tenantID, docID, agentID, sessionID, namespace string) {
	item := heurgraphPendingItem{
		DocID:     docID,
		AgentID:   agentID,
		SessionID: sessionID,
		Namespace: namespace,
	}
	data, err := json.Marshal(item)
	if err != nil {
		log.Printf("heurgraph: marshal pending item: %v", err)
		return
	}

	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: heurgraphPendingKey(tenantID),
		MaxLen: heurgraphStreamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	}).Err(); err != nil {
		log.Printf("heurgraph: xadd failed tenant=%s doc=%s: %v", tenantID, docID, err)
	}
}

// ---------------------------------------------------------------------------
// Background extraction worker — RunHeurgraphWorker
// ---------------------------------------------------------------------------

// HeurgraphConfig controls the extraction worker's behaviour.
// The sweep interval is not here — RunHeurgraphWorker takes it as an explicit
// parameter, matching RunConsolidationWorker.
type HeurgraphConfig struct {
	Enabled               bool
	TenantAllowlist       []string
	RequestTimeoutSeconds int
	MaxSourceBytes        int
	MaxTriples            int
	ReconcileBatchSize    int
	MaxAttempts           int
	ExtractionModel       string  `yaml:"extraction_model"`
	OllamaURL             string  `yaml:"ollama_url"`
	ResolutionBlockSize   int     `yaml:"resolution_block_size"`
	ActivationCutoff      float64 `yaml:"activation_cutoff"`
}

// heurgraphCfg is written once by SetHeurgraphConfig during boot, before
// RunHeurgraphWorker starts its goroutine, and is read-only thereafter.
// Same lifecycle as consolidationCfg.
var heurgraphCfg HeurgraphConfig

// heurgraphTick counts worker sweeps, used only to rotate which tenant the
// per-tick budget starts with.
var heurgraphTick atomic.Uint64

func SetHeurgraphConfig(cfg *HeurgraphConfig) {
	if cfg == nil {
		return
	}
	heurgraphCfg = *cfg
}

const (
	heurgraphGroup    = "heurgraph-workers"
	heurgraphConsumer = "worker-1"

	// heurgraphMaxItemsPerTick bounds work per tick so a large backlog
	// cannot monopolise the 5-connection background Redis pool.
	heurgraphMaxItemsPerTick = 500

	// heurgraphMaxItemsPerTenant caps any single tenant's share of a tick so
	// one high-volume tenant cannot consume the whole budget and starve the
	// rest — whose backlogs would then be the ones pruned by MAXLEN.
	heurgraphMaxItemsPerTenant = 100

	// heurgraphReclaimMinIdle is how long an unacked entry must sit in the
	// pending-entries list before another tick reclaims it. Must exceed the
	// worst-case extraction latency or in-flight work gets double-processed
	// (harmless — upserts are idempotent — but wasteful).
	heurgraphReclaimMinIdle = 10 * time.Minute
)

// RunHeurgraphWorker starts a goroutine that drains the pending extraction
// stream for all active tenants. Follows the pattern of
// RunConsolidationWorker: own goroutine, own ticker, uses bgRdb.
func RunHeurgraphWorker(ctx context.Context, bgRdb *redis.Client, db *sql.DB, interval time.Duration) {
	if !heurgraphExtractionReady || !heurgraphCfg.Enabled {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Minute
	}

	go func() {
		// Delay initial run — let the broker stabilise.
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}

		drainHeurgraphStream(ctx, bgRdb, db)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("heurgraph worker stopped")
				return
			case <-ticker.C:
				drainHeurgraphStream(ctx, bgRdb, db)
			}
		}
	}()
}

// drainHeurgraphStream processes pending items for every active tenant.
// Tenants come from SQLite (not a Redis SCAN) so suspended and deleted
// tenants stop being processed, matching runConsolidation's behaviour.
//
// A panic here would kill the whole process — this runs on a bare
// goroutine with no http handler recover() above it — so the sweep is
// wrapped.
func drainHeurgraphStream(ctx context.Context, bgRdb *redis.Client, db *sql.DB) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("heurgraph: PANIC recovered in worker sweep: %v", rec)
		}
	}()

	if !heurgraphExtractionReady || !heurgraphCfg.Enabled {
		// Writes keep accumulating in the (MAXLEN-capped) streams; the
		// reconciliation recovers any entries lost to stream retention.
		return
	}

	ctx = context.WithValue(ctx, graphCallBudgetKey{}, &graphCallBudget{remaining: 20})
	rows, err := db.QueryContext(ctx, "SELECT id FROM tenants WHERE status = 'active'")
	if err != nil {
		log.Printf("heurgraph: query tenants failed: %v", err)
		return
	}
	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		if graphTenantEnabled(id) {
			tenants = append(tenants, id)
		}
	}
	rows.Close()

	if len(tenants) == 0 {
		return
	}

	// Rotate the starting tenant each tick. The SELECT has no ORDER BY, so
	// its order is stable in practice; without rotation a tenant early in
	// that order could consume the tick budget every time and permanently
	// starve those after it.
	tick := heurgraphTick.Add(1) - 1
	start := int(tick) % len(tenants)

	budget := heurgraphMaxItemsPerTick
	for i := 0; i < len(tenants); i++ {
		if budget <= 0 {
			log.Printf("heurgraph: tick budget exhausted, %d tenants deferred to next tick",
				len(tenants)-i)
			return
		}
		tenantID := tenants[(start+i)%len(tenants)]

		share := min(budget, heurgraphMaxItemsPerTenant)

		// Per-tenant timeout prevents one stuck tenant blocking the rest.
		tCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		reconcile := func() {
			if tCtx.Err() == nil {
				if e := reconcileGraphTenant(tCtx, bgRdb, db, tenantID); e != nil {
					log.Printf("heurgraph: reconcile tenant=%s: %v", tenantID, e)
				}
			}
		}
		// Alternate priority so a permanent stream backlog cannot starve backfill,
		// and a large backfill cannot permanently starve new queue entries.
		if tick%2 == 0 {
			reconcile()
		}
		n, err := drainTenantStream(tCtx, bgRdb, db, tenantID, share)
		if tick%2 != 0 {
			reconcile()
		}
		cancel()
		if err != nil {
			log.Printf("heurgraph: drain tenant %s: %v", tenantID, err)
		}
		budget -= n
	}
}

// drainTenantStream processes up to maxItems entries from one tenant's
// pending stream: first reclaiming entries abandoned by an earlier tick,
// then reading new ones.
func drainTenantStream(ctx context.Context, bgRdb *redis.Client, db *sql.DB, tenantID string, maxItems int) (int, error) {
	streamKey := heurgraphPendingKey(tenantID)

	// Ensure the consumer group exists. BUSYGROUP means it already does.
	if err := bgRdb.XGroupCreateMkStream(ctx, streamKey, heurgraphGroup, "0").Err(); err != nil &&
		!strings.Contains(err.Error(), "BUSYGROUP") {
		return 0, fmt.Errorf("xgroupcreate: %w", err)
	}

	processed := 0

	// Reclaim entries left unacked by a previous tick (retryable failures,
	// or a crash mid-extraction). Without this the pending-entries list
	// grows forever and those docs are never indexed.
	claimKey := fmt.Sprintf("tenant:%s:heurgraph:claim_cursor", tenantID)
	claimStart, err := bgRdb.Get(ctx, claimKey).Result()
	if err != nil && err != redis.Nil {
		return 0, err
	}
	if claimStart == "" {
		claimStart = "0-0"
	}
	claimed, nextClaim, err := bgRdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   streamKey,
		Group:    heurgraphGroup,
		Consumer: heurgraphConsumer,
		MinIdle:  heurgraphReclaimMinIdle,
		Start:    claimStart,
		Count:    int64(maxItems),
	}).Result()
	if err != nil && err != redis.Nil {
		log.Printf("heurgraph: xautoclaim tenant=%s: %v", tenantID, err)
	} else {
		processed += handleMessages(ctx, bgRdb, db, tenantID, streamKey, claimed)
		if err := bgRdb.Set(ctx, claimKey, nextClaim, 0).Err(); err != nil {
			return processed, err
		}
	}

	if processed >= maxItems {
		return processed, nil
	}

	// Block: -1 makes this non-blocking. Block: 0 would block *forever* and
	// Block: 1s would cost one second per idle tenant per tick, serialised
	// across the whole tenant list.
	items, err := bgRdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    heurgraphGroup,
		Consumer: heurgraphConsumer,
		Streams:  []string{streamKey, ">"},
		Count:    int64(maxItems - processed),
		Block:    -1,
	}).Result()
	if err != nil && err != redis.Nil {
		return processed, fmt.Errorf("xreadgroup: %w", err)
	}

	for _, stream := range items {
		processed += handleMessages(ctx, bgRdb, db, tenantID, streamKey, stream.Messages)
	}

	return processed, nil
}

// handleMessages processes a batch of stream entries, acking only those
// that succeeded or failed permanently. Returns the number handled.
func handleMessages(ctx context.Context, bgRdb *redis.Client, db *sql.DB, tenantID, streamKey string, msgs []redis.XMessage) int {
	n := 0
	for _, msg := range msgs {
		if ctx.Err() != nil {
			break
		}
		err := processHeurgraphItem(ctx, bgRdb, db, tenantID, msg)
		switch {
		case err == nil:
			ackEntry(ctx, bgRdb, streamKey, tenantID, msg.ID)
		case errors.Is(err, errPermanent):
			// Unprocessable — ack so it stops consuming reclaim budget.
			log.Printf("heurgraph: dropping entry %s tenant=%s: %v", msg.ID, tenantID, err)
			ackEntry(ctx, bgRdb, streamKey, tenantID, msg.ID)
		default:
			// Retryable — leave unacked; XAutoClaim picks it up later.
			log.Printf("heurgraph: retryable failure on %s tenant=%s: %v", msg.ID, tenantID, err)
		}
		n++
	}
	return n
}

// ackEntry acknowledges a stream entry. A failed XACK leaves the entry in the
// pending-entries list to be reclaimed and reprocessed later, which is safe
// (upserts are idempotent) but worth surfacing.
func ackEntry(ctx context.Context, bgRdb *redis.Client, streamKey, tenantID, msgID string) {
	if err := bgRdb.XAck(ctx, streamKey, heurgraphGroup, msgID).Err(); err != nil {
		log.Printf("heurgraph: xack failed tenant=%s entry=%s: %v", tenantID, msgID, err)
	}
}

// processHeurgraphItem handles one pending extraction: read doc → extract →
// resolve → upsert → ZADD.
func processHeurgraphItem(ctx context.Context, bgRdb *redis.Client, db *sql.DB, tenantID string, msg redis.XMessage) error {
	raw, ok := msg.Values["data"].(string)
	if !ok {
		return fmt.Errorf("%w: entry has no string 'data' field", errPermanent)
	}
	var item heurgraphPendingItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return fmt.Errorf("%w: unmarshal pending item: %v", errPermanent, err)
	}

	if item.DocID == "" {
		return fmt.Errorf("%w: missing doc_id", errPermanent)
	}
	return indexGraphSource(ctx, bgRdb, db, tenantID, item.DocID)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// computeEdgeActivation computes ACT-R A_i for an edge:
// A_i = log(access_count * (age_seconds+1)^-d).
//
// Unlike activation() in consolidation.go this floors access_count at 1
// rather than returning -Inf, because an edge's usefulness for traversal
// does not vanish just because its source doc was never re-read.
func computeEdgeActivation(accessCount int, ageSeconds float64, namespace string) float64 {
	d := decayRate(namespace)
	if accessCount <= 0 {
		accessCount = 1
	}
	base := math.Max(ageSeconds+1.0, 1.0)
	return math.Log(float64(accessCount) * math.Pow(base, -d))
}
