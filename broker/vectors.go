package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

type storedVector struct {
	RecordKind       string              `json:"-"`
	ExpectedRevision *int64              `json:"-"`
	ChangeReason     string              `json:"-"`
	Receipt          *memoryWriteReceipt `json:"-"`
	ID               string              `json:"id"`
	AgentID          string              `json:"agent_id"`
	Tags             []string            `json:"tags,omitempty"`
	Embedding        []float32           `json:"embedding"`
	Metadata         json.RawMessage     `json:"metadata"`
	Text             string              `json:"text,omitempty"`
	CreatedAt        string              `json:"created_at"`
	Model            string              `json:"embedding_model,omitempty"`
	Namespace        string              `json:"namespace,omitempty"`
}

type searchResult struct {
	ID        string          `json:"id"`
	Score     float32         `json:"score"`
	BM25Score *float64        `json:"bm25_score,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
	Text      string          `json:"text,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// parseTags decodes the tags Redis hash field. Empty / malformed → nil.
func parseTags(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// tagsIntersect returns true if `have` contains any element of `want`.
// Empty `want` is treated as no filter (always true).
func tagsIntersect(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	if len(have) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(want))
	for _, t := range want {
		set[t] = struct{}{}
	}
	for _, h := range have {
		if _, ok := set[h]; ok {
			return true
		}
	}
	return false
}

func vecKey(tenantID, vecID string) string {
	return fmt.Sprintf("tenant:%s:vec:%s", tenantID, vecID)
}

func vecSetKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:vecs", tenantID)
}

func storeVec(ctx context.Context, rdb *redis.Client, tenantID string, sv storedVector) error {
	embJSON, err := json.Marshal(sv.Embedding)
	if err != nil {
		return fmt.Errorf("marshal embedding: %w", err)
	}
	if sv.CreatedAt == "" {
		sv.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	fields := map[string]interface{}{
		"embedding":  string(embJSON),
		"metadata":   string(sv.Metadata),
		"created_at": sv.CreatedAt,
		// Seed access_count so the ACT-R activation formula
		// (log(access_count * age^-d)) is finite for freshly stored docs.
		//
		// Without it a never-retrieved doc evaluates to -Inf, and the two
		// selectors treat that differently. The absolute-threshold selector
		// vaults it on the first sweep, before it has been read once — the
		// failure this seed prevents. The rank-cut selector is already safe
		// there (activationRanked floors the count at 1 and lets age order the
		// population), but it is the same seed that keeps both paths honest, so
		// it is written for the corpus rather than for one selector's benefit.
		"access_count": 1,
	}
	if sv.AgentID != "" {
		fields["agent_id"] = sv.AgentID
	}
	if sv.Model != "" {
		// Record which embedding model produced this vector.
		//
		// Stored for future cross-model detection and for debugging: two
		// vectors from different models are not comparable by cosine distance,
		// so a mixed corpus silently returns nonsense. No retrieval path reads
		// this field yet — nothing compares it or filters on it — so recording
		// it is groundwork, not an active guard. A real guard has to compare
		// the stored value against the query's model on the read path.
		fields["embedding_model"] = sv.Model
	}
	if sv.Text != "" {
		fields["text"] = sv.Text
	}
	if sv.Namespace != "" {
		fields["namespace"] = sv.Namespace
	} else {
		fields["namespace"] = "notes"
	}
	if len(sv.Tags) > 0 {
		tagsJSON, err := json.Marshal(sv.Tags)
		if err != nil {
			return fmt.Errorf("marshal tags: %w", err)
		}
		fields["tags"] = string(tagsJSON)
	}

	return storeMemoryFields(ctx, rdb, tenantID, sv, fields)
}

// vecScanCap bounds how many vectors a single /query brute-force cosine scan
// will fetch and score. 0 = unbounded. Set once at boot from
// broker.yaml `query.scan_cap`; read-only thereafter.
var vecScanCap int

// SetVecScanCap injects the operator-tuned scan bound.
func SetVecScanCap(n int) {
	if n < 0 {
		n = 0
	}
	vecScanCap = n
}

// listVecIDs returns the tenant's vector IDs for the brute-force /query scan,
// bounded by vecScanCap.
//
// Uses SSCAN rather than SMEMBERS when a cap is set: SMEMBERS materializes the
// entire set in one reply regardless of how many IDs we intend to score, so
// truncating afterwards bounds the CPU of the scan but not the network/memory
// cost of the fetch — the request path would still pay full O(corpus) for a
// large tenant, which is the thing the cap exists to prevent. SSCAN pages, so
// we can stop early and never materialize the rest.
//
// Truncation costs recall: neither SSCAN nor SMEMBERS orders by relevance, so
// the cap drops an arbitrary slice, not the worst matches. The warning below is
// deliberately actionable — an operator seeing it should move that tenant to
// the Qdrant-backed /search rather than raise the cap.
func listVecIDs(ctx context.Context, rdb *redis.Client, tenantID string) ([]string, error) {
	key := vecSetKey(tenantID)

	if vecScanCap <= 0 {
		ids, err := rdb.SMembers(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("list vectors: %w", err)
		}
		return ids, nil
	}

	ids := make([]string, 0, vecScanCap)
	var cursor uint64
	for {
		page, next, err := rdb.SScan(ctx, key, cursor, "", int64(vecScanCap)).Result()
		if err != nil {
			return nil, fmt.Errorf("scan vectors: %w", err)
		}
		ids = append(ids, page...)
		cursor = next
		if len(ids) >= vecScanCap || cursor == 0 {
			break
		}
	}

	if len(ids) >= vecScanCap {
		ids = ids[:vecScanCap]
		// SCARD is O(1), so reporting true corpus size costs nothing.
		total, err := rdb.SCard(ctx, key).Result()
		if err != nil {
			total = int64(len(ids))
		}
		if total > int64(vecScanCap) {
			log.Printf("query: tenant %s corpus %d exceeds scan_cap %d; scoring a partial set (recall reduced) - consider Qdrant-backed /search",
				tenantID, total, vecScanCap)
		}
	}
	return ids, nil
}

func searchVecs(ctx context.Context, rdb *redis.Client, tenantID, agentID string, tagFilter []string, query []float32, topK int, threshold float32, crossAgent bool) ([]searchResult, error) {
	ids, err := listVecIDs(ctx, rdb, tenantID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []searchResult{}, nil
	}

	// Phase 7 (cooperative): when the flag is on and the caller declares an
	// agent, agent_id becomes "who is asking" and the result set is what that
	// agent may see (own + shared + non-isolated peers + legacy) rather than an
	// exact author match. Loaded once per request so the per-document check is
	// a map lookup. Flag off => nil => the exact-match path below is unchanged.
	var isolated map[string]bool
	if crossAgent && agentID != "" {
		isolated = loadIsolatedAgents(ctx, rdb, tenantID)
	}

	// Fetch all vector hashes in a single pipeline round-trip.
	pipe := rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	forgotten := make([]*redis.IntCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, vecKey(tenantID, id))
		forgotten[i] = pipe.Exists(ctx, memoryTombstoneKey(tenantID, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("pipeline fetch: %w", err)
	}

	results := make([]searchResult, 0, len(ids))
	for i, id := range ids {
		// A lifecycle tombstone hides any residue under this ID.
		if tombstoneHidden(forgotten[i]) {
			continue
		}
		data, err := cmds[i].Result()
		if err != nil || len(data) == 0 {
			continue
		}
		if !matchesMemoryContext(ctx, id, data) {
			continue
		}

		// Filter by agent when requested. Flag off: legacy exact-author match.
		// Flag on: cooperative visibility (own + shared + non-isolated + legacy).
		if agentID != "" {
			if crossAgent {
				if !visibleToRequester(agentID, authorField(data), data["visibility"], data["visibility_agents"], isolated) {
					continue
				}
			} else if data["agent_id"] != agentID {
				continue
			}
		}

		// Filter by tag when requested. Untagged vectors are excluded when a
		// filter is supplied; surfaced normally when no filter is supplied.
		vecTags := parseTags(data["tags"])
		if !tagsIntersect(vecTags, tagFilter) {
			continue
		}

		var embedding []float32
		if err := json.Unmarshal([]byte(data["embedding"]), &embedding); err != nil {
			continue
		}

		score := cosine(query, embedding)
		if score < threshold {
			continue
		}

		meta := json.RawMessage(data["metadata"])
		if len(meta) == 0 || !json.Valid(meta) {
			meta = json.RawMessage(`{}`)
		}

		results = append(results, searchResult{
			ID:        id,
			Score:     score,
			AgentID:   data["agent_id"],
			Tags:      vecTags,
			Metadata:  meta,
			Text:      data["text"],
			CreatedAt: data["created_at"],
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}

	// Record retrievals so the ACT-R activation model reflects real usage.
	ids = make([]string, len(results))
	for i := range results {
		ids[i] = results[i].ID
	}
	bumpAccessCounts(ctx, rdb, tenantID, ids)

	return results, nil
}

// bumpAccessCounts increments the access_count field for each retrieved
// document so the consolidation activation model can distinguish frequently
// recalled memories from cold ones. Best-effort: errors are non-fatal to the
// read path.
func bumpAccessCounts(ctx context.Context, rdb *redis.Client, tenantID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	pipe := rdb.Pipeline()
	for _, id := range ids {
		pipe.Eval(ctx, `if redis.call('EXISTS', KEYS[1]) == 1 then return redis.call('HINCRBY', KEYS[1], 'access_count', 1) end return 0`, []string{vecKey(tenantID, id)})
	}
	_, _ = pipe.Exec(ctx)
}

// cosine returns the cosine similarity between two float32 slices.
// Returns 0 if slices are empty or of different length.
func cosine(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}
