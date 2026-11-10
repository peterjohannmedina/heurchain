package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConsolidationConfig holds operator-tunable parameters for the ACT-R
// memory-consolidation worker. Wire from main.go via SetConsolidationConfig
// at startup; defaults below match the historical hardcoded values so a
// missing config block produces unchanged behavior.
type ConsolidationConfig struct {
	Interval           time.Duration
	PromotionThreshold float64
	DecayRates         map[string]float64

	// PromoteLive arms the destructive half of consolidation. False = dry-run:
	// log what would be vaulted, change nothing.
	PromoteLive bool

	// DecayDefault is the decay rate for namespaces absent from DecayRates.
	DecayDefault float64

	// DemoteFraction > 0 switches consolidation to a relative cut (demote the
	// lowest-activation N of each tenant's corpus) instead of comparing A_i to
	// PromotionThreshold. 0 keeps the legacy absolute-threshold behaviour.
	// See demote.go and docs/architecture/archive-vs-working-set.md 5a.
	DemoteFraction float64

	// DemoteFloor caps documents demoted per sweep when DemoteFraction > 0.
	DemoteFloor int
}

// consolidationCfg is read by decayRate / activation / consolidateTenant.
// Package-level state matches the pre-existing pattern in this file
// (workers run as goroutines outside the Server struct) and keeps the
// activation() signature stable for callers in promoteSingleDoc.
var consolidationCfg = &ConsolidationConfig{
	Interval:           time.Hour,
	PromotionThreshold: -2.0,
	DecayRates: map[string]float64{
		"ops":   3.0,
		"notes": 1.0,
		"self":  0.1,
	},
	// Dry-run unless an operator arms it in broker.yaml.
	PromoteLive:  false,
	DecayDefault: 1.0,
}

// SetConsolidationConfig injects operator-tunable parameters loaded from
// broker.yaml. Safe to call once at startup; later calls overwrite.
func SetConsolidationConfig(cfg *ConsolidationConfig) {
	if cfg == nil {
		return
	}
	if cfg.Interval <= 0 {
		cfg.Interval = consolidationCfg.Interval
	}
	if cfg.PromotionThreshold == 0 {
		cfg.PromotionThreshold = consolidationCfg.PromotionThreshold
	}
	if cfg.DecayRates == nil {
		cfg.DecayRates = consolidationCfg.DecayRates
	}
	if cfg.DecayDefault <= 0 {
		cfg.DecayDefault = consolidationCfg.DecayDefault
	}
	if cfg.DemoteFloor <= 0 {
		cfg.DemoteFloor = 1000
	}
	consolidationCfg = cfg
}

// decayRate returns the per-namespace decay rate, falling back to the
// configured default for unknown namespaces.
//
// The empty namespace hits this fallback, and that is the common case for any
// document written before namespace persistence landed — so DecayDefault
// effectively governs the whole legacy corpus, not an edge case.
func decayRate(namespace string) float64 {
	if r, ok := consolidationCfg.DecayRates[namespace]; ok {
		return r
	}
	return consolidationCfg.DecayDefault
}

// activationAccessCount reads a document's access_count for the
// absolute-threshold selector.
//
// An absent field means 1, not 0. The two are different facts and only one of
// them justifies -Inf: 0 is "recorded as never read", while absent is "this
// record predates the field", which is what every vector written before the
// seed in storeVec looks like. Reading both as 0 made activation return -Inf
// for the whole pre-existing corpus, and because -Inf is below every finite
// threshold the first sweep — or the first press of "Flush working memory",
// which runs this selector directly — would vault that entire corpus in one
// pass. Treating absent as the seed value gives those records the same footing
// as a freshly stored one; an explicit 0 keeps its deliberate -Inf.
func activationAccessCount(data map[string]string) int {
	acStr := data["access_count"]
	if acStr == "" {
		return 1
	}
	ac, err := strconv.Atoi(acStr)
	if err != nil {
		return 1
	}
	return ac
}

// activation computes ACT-R A_i = log(access_count × (age_seconds+1)^-d)
// for a single document. Never-accessed items return -Inf so they sort
// below any finite threshold.
func activation(accessCount int, ageSeconds float64, namespace string) float64 {
	if accessCount <= 0 {
		return math.Inf(-1)
	}
	d := decayRate(namespace)
	base := ageSeconds + 1.0
	if base <= 0 {
		base = 1.0
	}
	return math.Log(float64(accessCount) * math.Pow(base, -d))
}

// consolidateTenant scans all live vector entries for a tenant and promotes
// any document whose activation A_i falls below the configured
// promotion threshold into the cold vault.
//
// ACT-R FORGETTING SEMANTICS — this is intentional, not a bug:
// once a document is vaulted it is REMOVED from the active retrieval
// set. /query, /search, BM25, and RRF will not surface it. The data is
// preserved at `tenant:%s:vault:%s` and can be recovered out-of-band
// via /vault/export, but does NOT participate in normal retrieval ranking.
//
// Rationale: in a working-memory model, low-activation items should stop
// competing for ranking slots so high-activation items dominate. If
// recoverable-but-rankable behavior is needed, the vault must be wired
// into the retrieval pool explicitly as a low-prior pool fused via RRF.
//
// Returns count of promoted documents.
func consolidateTenant(ctx context.Context, rdb *redis.Client, tenantID string) (int, error) {
	pattern := fmt.Sprintf("tenant:%s:vec:*", tenantID)
	prefix := fmt.Sprintf("tenant:%s:vec:", tenantID)
	var cursor uint64
	promoted := 0
	const scanCount = int64(100)

	for {
		// A cancelled or timed-out context (e.g. the 30s per-tenant deadline
		// set in runConsolidation) is the sole exit condition for large
		// tenants — there is no fixed per-cycle document cap. Bail cleanly
		// between SCAN pages so the deadline actually stops the pass.
		if err := ctx.Err(); err != nil {
			return promoted, nil
		}

		keys, nextCursor, err := rdb.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return promoted, fmt.Errorf("scan tenant %s: %w", tenantID, err)
		}

		// Pipeline all HGETALLs for this page — one round-trip instead of N.
		pipe := rdb.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(keys))
		for i, key := range keys {
			cmds[i] = pipe.HGetAll(ctx, key)
		}
		_, _ = pipe.Exec(ctx)

		for i, key := range keys {
			if len(key) <= len(prefix) {
				continue
			}
			docID := key[len(prefix):]

			data, err := cmds[i].Result()
			if err != nil {
				log.Printf("consolidation: hgetall failed %s: %v", key, err)
				continue
			}
			if len(data) == 0 {
				continue
			}

			createdAtStr := data["created_at"]
			if createdAtStr == "" {
				continue
			}

			var createdAt time.Time
			if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
				createdAt = t
			} else if unixSec, err := strconv.ParseInt(createdAtStr, 10, 64); err == nil {
				createdAt = time.Unix(unixSec, 0)
			} else {
				log.Printf("consolidation: invalid created_at %s: %v", key, err)
				continue
			}

			ageSeconds := time.Since(createdAt).Seconds()
			if ageSeconds < 0 {
				ageSeconds = 0
			}

			accessCount := activationAccessCount(data)

			namespace := data["namespace"]

			ai := activation(accessCount, ageSeconds, namespace)
			if ai < consolidationCfg.PromotionThreshold {
				// Dry-run: report the decision, touch nothing. Lets an
				// operator retune promotion_threshold against real activation
				// values before arming an irreversible-from-retrieval move.
				if !consolidationCfg.PromoteLive {
					log.Printf("consolidation: DRY-RUN would vault doc %s for tenant %s (A_i=%.4f, ns=%q, ac=%d)",
						docID, tenantID, ai, namespace, accessCount)
					continue
				}

				if err := vaultDoc(ctx, rdb, tenantID, docID); err != nil {
					log.Printf("consolidation: promotion failed %s: %v", key, err)
					continue
				}
				log.Printf("consolidation: promoted doc %s for tenant %s (A_i=%.4f)", docID, tenantID, ai)
				promoted++
			}
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
		// Yield between SCAN pages so the worker doesn't monopolise the pool.
		time.Sleep(50 * time.Millisecond)
	}

	return promoted, nil
}

// runConsolidation sweeps every active tenant once.
//
// A panic here would kill the whole process — this runs on a bare goroutine
// with no http handler recover() above it — so the sweep is wrapped, matching
// drainHeurgraphStream. Note the panic surface is identical in dry-run mode:
// PromoteLive gates only the destructive write, while the scan, hash parsing
// and activation math run either way.
func runConsolidation(ctx context.Context, rdb *redis.Client, db *sql.DB) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("consolidation: PANIC recovered in worker sweep: %v", rec)
		}
	}()

	rows, err := db.QueryContext(ctx, "SELECT id FROM tenants WHERE status = 'active'")
	if err != nil {
		log.Printf("consolidation: query tenants failed: %v", err)
		return
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		tenants = append(tenants, id)
	}

	for _, tenantID := range tenants {
		// Per-tenant timeout prevents one stuck tenant from blocking the rest.
		// Ranked mode makes two passes over the corpus, so it gets longer.
		timeout := 30 * time.Second
		if consolidationCfg.DemoteFraction > 0 {
			timeout = 60 * time.Second
		}
		tCtx, cancel := context.WithTimeout(ctx, timeout)
		n, err := consolidateOne(tCtx, rdb, tenantID)
		cancel()
		if err != nil {
			log.Printf("consolidation: tenant %s error: %v", tenantID, err)
		} else if n > 0 {
			log.Printf("consolidation: tenant %s promoted %d documents", tenantID, n)
		}
	}
}

// consolidateOne runs one consolidation pass for a single tenant using the
// selector the configuration is set to.
//
// Both the worker and /vault/flush-working-memory go through here. The flush
// used to call consolidateTenant directly, which meant that with any
// DemoteFraction configured — the shipped setting — pressing "Flush working
// memory to vault" ran the absolute-threshold selector while the background
// worker ran the rank-cut one. The button therefore applied a different, and
// considerably more aggressive, policy than the one the operator had
// configured: it vaulted every document below promotion_threshold, a value
// which the rank-cut configuration never consults. The two paths now agree, and
// under an absolute configuration this is still exactly consolidateTenant.
func consolidateOne(ctx context.Context, rdb *redis.Client, tenantID string) (int, error) {
	if consolidationCfg.DemoteFraction > 0 {
		return consolidateTenantRanked(ctx, rdb, tenantID)
	}
	return consolidateTenant(ctx, rdb, tenantID)
}

// RunConsolidationWorker starts a goroutine that runs consolidateTenant
// for all tenants every interval. Stops when ctx is cancelled.
// Initial run is delayed 2 min so the broker reaches steady state first.
func RunConsolidationWorker(ctx context.Context, rdb *redis.Client, db *sql.DB, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}

	go func() {
		// Delay initial run — let the HTTP server stabilise before hammering Redis.
		select {
		case <-time.After(2 * time.Minute):
		case <-ctx.Done():
			return
		}

		runConsolidation(ctx, rdb, db)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("consolidation worker stopped")
				return
			case <-ticker.C:
				runConsolidation(ctx, rdb, db)
			}
		}
	}()
}

// promoteSingleDoc reads a doc, computes activation, promotes if below threshold,
// and returns (promoted, activation, err).
func promoteSingleDoc(ctx context.Context, rdb *redis.Client, tenantID, docID string) (bool, float64, error) {
	key := vecKey(tenantID, docID)
	data, err := rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return false, 0, err
	}
	if len(data) == 0 {
		return false, 0, fmt.Errorf("document not found")
	}

	createdAtStr := data["created_at"]
	if createdAtStr == "" {
		return false, 0, fmt.Errorf("missing created_at")
	}

	var createdAt time.Time
	if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
		createdAt = t
	} else if unixSec, err := strconv.ParseInt(createdAtStr, 10, 64); err == nil {
		createdAt = time.Unix(unixSec, 0)
	} else {
		return false, 0, fmt.Errorf("invalid created_at: %s", createdAtStr)
	}

	ageSeconds := time.Since(createdAt).Seconds()
	if ageSeconds < 0 {
		ageSeconds = 0
	}

	accessCount := activationAccessCount(data)

	namespace := data["namespace"]
	ai := activation(accessCount, ageSeconds, namespace)

	// Deliberately NOT gated on PromoteLive: this is the single-document
	// /promote endpoint, an explicit operator action on a named doc. The
	// dry-run flag governs the unattended sweep, which acts on the whole
	// corpus without anyone asking.
	if ai < consolidationCfg.PromotionThreshold {
		if err := vaultDoc(ctx, rdb, tenantID, docID); err != nil {
			return false, ai, err
		}
		return true, ai, nil
	}

	return false, ai, nil
}

// handlePromote POST /promote body: {doc_id, tenant_id}
// Manually triggers promotion of a single document.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	apiKey := r.Header.Get(s.config.Auth.APIKeyHeader)
	tenantID := r.Header.Get(s.config.Auth.TenantHeader)
	if apiKey == "" {
		jsonError(w, "missing api key", http.StatusUnauthorized)
		return
	}
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if _, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		DocID    string `json:"doc_id"`
		TenantID string `json:"tenant_id"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.DocID == "" {
		jsonError(w, "doc_id is required", http.StatusBadRequest)
		return
	}
	// doc_id was the one identifier reaching a Redis key without the charset
	// and length check every other id field gets.
	if !validID.MatchString(req.DocID) {
		jsonError(w, "invalid doc_id", http.StatusBadRequest)
		return
	}
	if req.TenantID == "" {
		jsonError(w, "tenant_id is required", http.StatusBadRequest)
		return
	}
	if tenantID != req.TenantID {
		jsonError(w, "tenant mismatch", http.StatusForbidden)
		return
	}

	promoted, activationScore, err := promoteSingleDoc(ctx, s.rdb, tenantID, req.DocID)
	if err != nil {
		log.Printf("promote error tenant=%s doc=%s: %v", tenantID, req.DocID, err)
		jsonError(w, "promotion failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"promoted":   promoted,
		"activation": activationScore,
	})
}
