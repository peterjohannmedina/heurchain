package main

import (
	"container/heap"
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Rank-based demotion.
//
// The absolute-threshold model (activation() vs PromotionThreshold) has no
// stable operating point: A_i = log(access_count) - d*log(age+1) is unbounded
// downward, so as a corpus ages every document eventually crosses any fixed
// threshold. Measured on the production corpus, 64% of documents have never
// been re-read; for those log(access_count) = 0, so A_i is strictly negative
// and falling, and every (decay_rate, threshold) pair converges on vaulting
// that same 64% — differing only in how long it takes.
//
// A relative cut — "demote the lowest-activation 20% of this tenant" — is
// scale- and age-invariant by construction. Decay then does what its name
// implies: it weights age against use in a *ranking*, rather than setting a
// global offset that the threshold turns into an on/off switch.
//
// See docs/architecture/archive-vs-working-set.md 4 and 5a.
// ---------------------------------------------------------------------------

// demoteCandidate is one document's activation, carried through ranking.
type demoteCandidate struct {
	docID      string
	activation float64
	namespace  string
	accessCnt  int
}

// candidateHeap is a MAX-heap on activation, used to retain the K *lowest*
// activations while streaming: push everything, and once the heap exceeds K,
// pop the largest. What remains is the bottom K.
//
// Bounded by K rather than by corpus size on purpose — the 2M-vector ceiling
// under a 1.5 GB MemoryMax makes materialising every document's activation an
// OOM risk, which is the same reasoning vault.go uses to avoid a docID set.
type candidateHeap []demoteCandidate

func (h candidateHeap) Len() int      { return len(h) }
func (h candidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h candidateHeap) Less(i, j int) bool {
	// Max-heap: "less" means higher activation sorts first.
	if h[i].activation != h[j].activation {
		return h[i].activation > h[j].activation
	}
	// Deterministic tie-break so a sweep is reproducible when many documents
	// share an activation — common, since 64% share access_count = 1.
	return h[i].docID > h[j].docID
}

func (h *candidateHeap) Push(x any) { *h = append(*h, x.(demoteCandidate)) }

func (h *candidateHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// keepLowest pushes a candidate and evicts the highest activation once the
// heap exceeds limit, leaving the `limit` lowest-activation candidates.
func keepLowest(h *candidateHeap, c demoteCandidate, limit int) {
	if limit <= 0 {
		return
	}
	heap.Push(h, c)
	if h.Len() > limit {
		heap.Pop(h)
	}
}

// scanActivations streams a tenant's live documents and returns the
// `limit` lowest-activation candidates, plus the total documents scanned.
//
// Read-only: no key is written or deleted here. Ranking is deliberately
// separated from acting on the ranking so the caller can log, dry-run, or
// bail without a partially-demoted corpus.
func scanActivations(ctx context.Context, rdb *redis.Client, tenantID string, limit int) (candidateHeap, int, error) {
	// SSCAN the tenant's own vec set rather than SCAN-ing the keyspace by
	// pattern. SCAN's cost is O(total keys in the database), not O(matches):
	// production holds ~481k keys of which one tenant owns 737, so a pattern
	// scan took 3.25s to find them while reading the set takes ~9ms. The first
	// deployment of this function used SCAN and blew its 60s deadline after
	// only 98 documents.
	//
	// SSCAN also pages, so a large set never materialises in one reply.
	h := candidateHeap{}
	heap.Init(&h)
	scanned := 0
	var cursor uint64
	const pageSize = int64(200)

	for {
		// The per-tenant deadline is the exit condition, checked between pages
		// so it actually takes effect.
		if err := ctx.Err(); err != nil {
			return h, scanned, ctx.Err()
		}

		ids, next, err := rdb.SScan(ctx, vecSetKey(tenantID), cursor, "", pageSize).Result()
		if err != nil {
			return h, scanned, fmt.Errorf("sscan tenant %s: %w", tenantID, err)
		}

		if len(ids) > 0 {
			pipe := rdb.Pipeline()
			cmds := make([]*redis.SliceCmd, len(ids))
			for i, id := range ids {
				// Only the three fields activation needs — HGetAll would pull
				// the embedding (~9.5 KB each) for every document in the
				// corpus. HMGet returns positional values, nil when absent.
				cmds[i] = pipe.HMGet(ctx, vecKey(tenantID, id), "created_at", "access_count", "namespace")
			}
			_, _ = pipe.Exec(ctx)

			for i, id := range ids {
				vals, err := cmds[i].Result()
				if err != nil || len(vals) < 3 {
					continue
				}
				createdAt, ok := parseCreatedAt(asString(vals[0]))
				if !ok {
					continue // undateable; never demote what cannot be scored
				}
				age := time.Since(createdAt).Seconds()
				if age < 0 {
					age = 0
				}
				ac := 0
				if s := asString(vals[1]); s != "" {
					if n, err := strconv.Atoi(s); err == nil {
						ac = n
					}
				}
				ns := asString(vals[2])

				// activationRanked, not activation(): the latter returns -Inf
				// for access_count = 0, which collapses every never-read
				// document to one indistinguishable value and makes the
				// ranking arbitrary across 64% of the corpus.
				scanned++
				keepLowest(&h, demoteCandidate{
					docID:      id,
					activation: activationRanked(ac, age, ns),
					namespace:  ns,
					accessCnt:  ac,
				}, limit)
			}
		}

		cursor = next
		if cursor == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond) // yield, as consolidateTenant does
	}
	return h, scanned, nil
}

// activationRanked is activation() with the -Inf special case removed.
//
// activation() returns -Inf when access_count <= 0 so that never-read
// documents sort below any finite threshold. Under a *ranking* that is
// actively harmful: 64% of the production corpus has access_count <= 1, and
// collapsing them all to -Inf makes their relative order meaningless — age
// would stop discriminating exactly where it matters most. Flooring the count
// at 1 keeps age as the tie-breaker within that population.
func activationRanked(accessCount int, ageSeconds float64, namespace string) float64 {
	if accessCount < 1 {
		accessCount = 1
	}
	d := decayRate(namespace)
	base := ageSeconds + 1.0
	if base <= 0 {
		base = 1.0
	}
	return math.Log(float64(accessCount)) - d*math.Log(base)
}

// asString renders one HMGet element. Absent fields come back as nil, which
// must read as "" rather than panicking a whole sweep.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// parseCreatedAt accepts both formats the corpus contains: RFC3339 (current)
// and a bare Unix second count (legacy).
func parseCreatedAt(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if unix, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(unix, 0), true
	}
	return time.Time{}, false
}

// consolidateTenantRanked demotes the lowest-activation DemoteFraction of a
// tenant's corpus. Returns the number demoted.
//
// Two passes: rank read-only, then act. Slower than the single-pass absolute
// threshold, but a relative cut cannot be evaluated until the corpus size is
// known, and acting mid-scan would let earlier pages be judged against a
// different population than later ones.
func consolidateTenantRanked(ctx context.Context, rdb *redis.Client, tenantID string) (int, error) {
	total, err := rdb.SCard(ctx, vecSetKey(tenantID)).Result()
	if err != nil {
		return 0, fmt.Errorf("scard %s: %w", tenantID, err)
	}
	if total == 0 {
		return 0, nil
	}

	want := int(float64(total) * consolidationCfg.DemoteFraction)
	if want > consolidationCfg.DemoteFloor {
		log.Printf("consolidation: tenant %s demote count %d exceeds cap %d; capping",
			tenantID, want, consolidationCfg.DemoteFloor)
		want = consolidationCfg.DemoteFloor
	}
	if want <= 0 {
		return 0, nil // corpus too small for the fraction to select anything
	}

	h, scanned, err := scanActivations(ctx, rdb, tenantID, want)
	consolidationMetrics.rankScanned.Add(uint64(scanned))
	if err != nil {
		consolidationMetrics.rankIncomplete.Add(1)
		// A timeout mid-rank yields a partial, unrepresentative ranking.
		// Demoting from it would pick "lowest among what we happened to see".
		log.Printf("consolidation: tenant %s ranking incomplete (%d scanned): %v — skipping sweep",
			tenantID, scanned, err)
		return 0, nil
	}
	if h.Len() == 0 {
		return 0, nil
	}

	// SCARD counts set members; scanned counts documents whose hash actually
	// resolved with a readable created_at. A large gap means the set has
	// drifted from the hashes — SREM without DEL, or vice versa — so `want`
	// was computed against a population that does not exist, and demoting
	// bottom-N of a partial view would cut deeper than the configured
	// fraction. Skip rather than guess.
	if scanned < want {
		log.Printf("consolidation: tenant %s scanned %d usable docs but SCARD reported %d (want %d) — skipping sweep; vecs set may be stale",
			tenantID, scanned, total, want)
		return 0, nil
	}

	demoted := 0
	for _, c := range h {
		if err := ctx.Err(); err != nil {
			return demoted, nil
		}
		if !consolidationCfg.PromoteLive {
			consolidationMetrics.dryRunSelected.Add(1)
			log.Printf("consolidation: DRY-RUN would vault doc %s for tenant %s (rank-cut, A_i=%.4f, ns=%q, ac=%d)",
				c.docID, tenantID, c.activation, c.namespace, c.accessCnt)
			continue
		}
		if err := vaultDoc(ctx, rdb, tenantID, c.docID); err != nil {
			log.Printf("consolidation: promotion failed %s/%s: %v", tenantID, c.docID, err)
			continue
		}
		log.Printf("consolidation: promoted doc %s for tenant %s (rank-cut, A_i=%.4f, ac=%d, %d/%d of corpus)",
			c.docID, tenantID, c.activation, c.accessCnt, want, total)
		demoted++
	}
	return demoted, nil
}

// vaultDoc moves one document from the active set into the cold vault.
//
// The Redis Lua move reads the complete hash server-side rather than reusing
// the fields ranking fetched: ranking pulls three fields, and writing only
// those to the vault would silently discard the embedding, text, tags, and
// metadata — the entire document.
func vaultDoc(ctx context.Context, rdb *redis.Client, tenantID, docID string) (err error) {
	unlock := lockMemoryTenant(tenantID)
	defer unlock()
	defer func() {
		if err != nil {
			consolidationMetrics.promotionFailures.Add(1)
		}
	}()
	moved, err := moveActiveToVault(ctx, rdb, tenantID, docID)
	if err != nil {
		return err
	}
	if !moved {
		return nil // already gone; nothing to move
	}
	consolidationMetrics.promotions.Add(1)

	// Best-effort, matching consolidateTenant: the document has already moved,
	// so a semantic-index reconciliation failure is logged rather than fatal.
	// Observe the current source under the semantic writer lease. A fresh
	// store after the vault move must be indexed, not deleted by this cleanup.
	if err := indexSemanticDocument(ctx, rdb, defaultQdrantClient, tenantID, docID); err != nil {
		consolidationMetrics.qdrantDeleteFailures.Add(1)
		log.Printf("consolidation: semantic index reconciliation failed tenant=%s doc=%s: %v", tenantID, docID, err)
	}
	return nil
}
