package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Serialize ordinary stores per tenant within this broker. BM25's aggregate
// statistics and a document's dense projection both use read/modify/write.
// This is not a distributed transaction: index failures are reported to callers.
var recallWriteLocks [256]sync.Mutex

func lockMemoryTenant(tenant string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenant))
	lock := &recallWriteLocks[h.Sum32()%uint32(len(recallWriteLocks))]
	lock.Lock()
	return lock.Unlock
}

func (s *Server) storeWithIndexes(ctx context.Context, tenant string, sv storedVector) (storeErr, indexErr error) {
	unlock := lockMemoryTenant(tenant)
	defer unlock()
	if err := storeVec(ctx, s.rdb, tenant, sv); err != nil {
		return err, nil
	}
	return nil, reindexActiveDoc(ctx, s.rdb, tenant, sv.ID)
}

type recallDiagnostics struct {
	Options               RecallOptions `json:"options"`
	EmbeddingModel        string        `json:"embedding_model,omitempty"`
	EmbeddingCacheHit     bool          `json:"embedding_cache_hit"`
	QueryID               string        `json:"query_id"`
	EmbeddingMilliseconds int64         `json:"embedding_ms"`
	RetrievalMilliseconds int64         `json:"retrieval_ms"`
	TotalMilliseconds     int64         `json:"total_ms"`
	BM25Candidates        int           `json:"bm25_candidates"`
	FusedCandidates       int           `json:"fused_candidates"`
	ReturnedDocuments     int           `json:"returned_documents"`
	BM25Status            string        `json:"bm25_status"`
	DenseStatus           string        `json:"dense_status"`
	EligibleDocuments     int           `json:"eligible_documents"`
	DenseCandidates       int           `json:"dense_candidates"`
	DenseLimit            int           `json:"dense_candidate_limit"`
	UnresolvedIDs         int           `json:"unresolved_dense_ids"`
	MissingText           int           `json:"missing_text"`
	RerankStatus          string        `json:"rerank_status,omitempty"`
	RerankCandidates      int           `json:"rerank_candidates,omitempty"`
}

type recallModelContextKey struct{}
type recallScopeContextKey struct{}

func recallFilterWithModel(agent string, tags []string, model string, scope RecallOptions) map[string]interface{} {
	filter := recallFilter(agent, tags, scope)
	if model == "" {
		return filter
	}
	if filter == nil {
		filter = map[string]interface{}{}
	}
	must, _ := filter["must"].([]interface{})
	compatible := append(compatibleEmbedModels(model), "")
	filter["must"] = append(must, map[string]interface{}{"should": []interface{}{
		map[string]interface{}{"key": "embedding_model", "match": map[string]interface{}{"any": compatible}},
		map[string]interface{}{"is_empty": map[string]interface{}{"key": "embedding_model"}},
	}})
	return filter
}

// Search retains exact-agent and any-tag semantics. Filter pushdown keeps
// unrelated documents from consuming the bounded dense candidate budget.
func recallFilter(agent string, tags []string, options ...RecallOptions) map[string]interface{} {
	must := []interface{}{}
	if agent != "" {
		must = append(must, map[string]interface{}{"key": "agent_id", "match": map[string]interface{}{"value": agent}})
	}
	if len(tags) > 0 {
		must = append(must, map[string]interface{}{"key": "tags", "match": map[string]interface{}{"any": tags}})
	}
	filter := map[string]interface{}{}
	if len(options) > 0 {
		o := options[0]
		if o.MemoryKind != "" {
			must = append(must, map[string]interface{}{"key": "memory_kind", "match": map[string]interface{}{"value": o.MemoryKind}})
		}
		if o.AsOf != "" {
			filter["must_not"] = []interface{}{
				map[string]interface{}{"key": "valid_from", "range": map[string]interface{}{"gt": o.AsOf}},
				map[string]interface{}{"key": "valid_until", "range": map[string]interface{}{"lte": o.AsOf}},
			}
		}
	}
	if len(must) == 0 && len(filter) == 0 {
		return nil
	}
	filter["must"] = must
	return filter
}

func eligibleRecallIDs(ctx context.Context, rdb *redis.Client, tenant string, ids []string, agent string, tags []string, options ...RecallOptions) ([]string, error) {
	var scope RecallOptions
	if len(options) > 0 {
		scope = options[0]
	}
	if agent == "" && len(tags) == 0 && scope.AsOf == "" && scope.MemoryKind == "" && ctx.Value(memoryFilterContextKey{}) == nil {
		return ids, nil
	}
	out := make([]string, 0, len(ids))
	for start := 0; start < len(ids); start += 128 {
		end := start + 128
		if end > len(ids) {
			end = len(ids)
		}
		pipe := rdb.Pipeline()
		cmds := make([]*redis.SliceCmd, end-start)
		for i, id := range ids[start:end] {
			// ids is the tenant's whole active set, so this read must stay
			// field-scoped: HGETALL would stream every stored embedding
			// through Redis on each filtered recall.
			cmds[i] = pipe.HMGet(ctx, vecKey(tenant, id), memoryFilterFields...)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
		for i, cmd := range cmds {
			values, err := cmd.Result()
			if err != nil {
				return nil, err
			}
			data := hashFieldMap(memoryFilterFields, values)
			if (agent == "" || data["agent_id"] == agent) && tagsIntersect(parseTags(data["tags"]), tags) && matchesMemoryContext(ctx, ids[start+i], data) && recallMetadataEligible(data["metadata"], scope) {
				out = append(out, ids[start+i])
			}
		}
	}
	return out, nil
}

// resolveRecallHits also supports old points without source_id. The reverse
// mapping is built from this tenant's active IDs, never from another tenant or
// the vault. Ambiguous aliases fail closed until reindexed with source_id.
func resolveRecallHits(hits []QdrantSearchResult, active, eligible []string) ([]QdrantSearchResult, int) {
	aliases := make(map[string]string, len(active))
	allowed := make(map[string]bool, len(eligible))
	for _, id := range eligible {
		allowed[id] = true
	}
	for _, id := range active {
		point := id
		ensureUUID(&point)
		if old, ok := aliases[point]; ok && old != id {
			aliases[point] = ""
		} else if !ok {
			aliases[point] = id
		}
	}
	out := make([]QdrantSearchResult, 0, len(hits))
	seen := map[string]bool{}
	unresolved := 0
	for _, hit := range hits {
		point := hit.ID
		ensureUUID(&point)
		id, _ := hit.Payload["source_id"].(string)
		if id != "" {
			check := id
			ensureUUID(&check)
			if check != point {
				unresolved++
				continue
			}
		} else {
			id = aliases[point]
		}
		if id == "" || !allowed[id] {
			unresolved++
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		hit.ID = id
		out = append(out, hit)
	}
	return out, unresolved
}

func runRecallSearch(ctx context.Context, rdb *redis.Client, tenant, text, agent string, tags []string, vector []float32, limit, candidates int, options ...RecallOptions) ([]SearchResult, recallDiagnostics, error) {
	var scope RecallOptions
	if len(options) > 0 {
		scope = options[0]
	}
	if err := scope.normalize(); err != nil {
		return nil, recallDiagnostics{}, err
	}
	ctx = context.WithValue(ctx, recallScopeContextKey{}, scope)
	diag := recallDiagnostics{BM25Status: "ok", DenseStatus: "ok", DenseLimit: candidates}
	diag.Options = scope
	if scope.Mode == "lexical" {
		diag.DenseStatus = "skipped"
	}
	if scope.Mode == "dense" {
		diag.BM25Status = "skipped"
	}
	active, err := rdb.SMembers(ctx, vecSetKey(tenant)).Result()
	if err != nil {
		return nil, diag, fmt.Errorf("read active corpus: %w", err)
	}
	eligible, err := eligibleRecallIDs(ctx, rdb, tenant, active, agent, tags, scope)
	if err != nil {
		return nil, diag, fmt.Errorf("read eligibility: %w", err)
	}
	diag.EligibleDocuments = len(eligible)
	type lexicalReply struct {
		scores map[string]float64
		err    error
	}
	type denseReply struct {
		hits []QdrantSearchResult
		err  error
	}
	lexicalCh := make(chan lexicalReply, 1)
	denseCh := make(chan denseReply, 1)
	go func() {
		if scope.Mode == "dense" {
			lexicalCh <- lexicalReply{}
			return
		}
		defer func() {
			if value := recover(); value != nil {
				lexicalCh <- lexicalReply{err: fmt.Errorf("lexical panic: %v", value)}
			}
		}()
		scores, err := searchBM25Candidates(ctx, rdb, tenant, agent, tags, tokenize(text), maxQdrantCandidates)
		lexicalCh <- lexicalReply{scores, err}
	}()
	go func() {
		if scope.Mode == "lexical" {
			denseCh <- denseReply{}
			return
		}
		defer func() {
			if value := recover(); value != nil {
				denseCh <- denseReply{err: fmt.Errorf("dense panic: %v", value)}
			}
		}()
		model, _ := ctx.Value(recallModelContextKey{}).(string)
		hits, err := defaultQdrantClient.Search(ctx, tenant, vector, candidates, recallFilterWithModel(agent, tags, model, scope))
		denseCh <- denseReply{hits, err}
	}()
	lexical, dense := <-lexicalCh, <-denseCh
	if lexical.err != nil {
		log.Printf("search: bm25 leg failed tenant=%s: %v", tenant, lexical.err)
		diag.BM25Status = "failed"
		lexical.scores = nil
	}
	if dense.err != nil {
		log.Printf("search: dense leg failed tenant=%s: %v", tenant, dense.err)
		diag.DenseStatus = "failed"
		dense.hits = nil
	}
	if (lexical.err != nil && dense.err != nil) || (scope.Mode == "lexical" && lexical.err != nil) || (scope.Mode == "dense" && dense.err != nil) {
		return nil, diag, fmt.Errorf("search backends unavailable")
	}
	diag.DenseCandidates = len(dense.hits)
	diag.BM25Candidates = len(lexical.scores)
	dense.hits, diag.UnresolvedIDs = resolveRecallHits(dense.hits, active, eligible)
	model, _ := ctx.Value(recallModelContextKey{}).(string)
	dense.hits, err = validateSemanticHits(ctx, rdb, tenant, agent, tags, model, dense.hits)
	if err != nil {
		diag.DenseStatus = "failed"
		dense.hits = nil
		if lexical.err != nil {
			return nil, diag, fmt.Errorf("search backends unavailable: validate dense hits: %w", err)
		}
	}
	// Do not spend the final limit until records are active, hydrated and eligible.
	ranked := fuseResultsWithOptions(lexical.scores, dense.hits, 0, scope)
	diag.FusedCandidates = len(ranked)
	var identifiers map[string]bool
	if scope.RerankMode == "exact_identifiers" {
		identifiers, diag.RerankStatus = recallQueryIdentifiers(text)
	}
	hydrationLimit := limit
	if len(identifiers) > 0 {
		// One fixed pool, bounded by the existing candidate budget (at most 200).
		// Do not let hydration, source caps or reranking expand that budget.
		hydrationLimit = min(max(candidates, limit), maxQdrantCandidates)
		ranked = ranked[:min(len(ranked), hydrationLimit)]
	}
	results := make([]SearchResult, 0, hydrationLimit)
	parentCounts := map[string]int{}
	for start := 0; start < len(ranked) && len(results) < hydrationLimit; start += 64 {
		end := start + 64
		if end > len(ranked) {
			end = len(ranked)
		}
		pipe := rdb.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, end-start)
		forgotten := make([]*redis.IntCmd, end-start)
		for i, res := range ranked[start:end] {
			cmds[i] = pipe.HGetAll(ctx, vecKey(tenant, res.ID))
			forgotten[i] = pipe.Exists(ctx, memoryTombstoneKey(tenant, res.ID))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, diag, fmt.Errorf("hydrate results: %w", err)
		}
		for i, cmd := range cmds {
			// Every BM25 and dense candidate passes through here, so
			// resurrected residue of a forgotten ID stops here.
			if tombstoneHidden(forgotten[i]) {
				continue
			}
			data, err := cmd.Result()
			if err != nil {
				return nil, diag, err
			}
			var metadata map[string]interface{}
			_ = json.Unmarshal([]byte(data["metadata"]), &metadata)
			hydratedText := vaultRecordText(data, metadata)
			if len(data) == 0 || hydratedText == "" {
				diag.MissingText++
				continue
			}
			if (agent != "" && data["agent_id"] != agent) || !tagsIntersect(parseTags(data["tags"]), tags) || !recallMetadataEligible(data["metadata"], scope) || !matchesMemoryContext(ctx, ranked[start+i].ID, data) {
				continue
			}
			res := ranked[start+i]
			if res.QdrantScore != nil && !semanticHitMatches(data, res.Payload, model) {
				continue
			}
			parent := res.ID
			if source, ok := metadata["source"].(map[string]interface{}); ok {
				if id, ok := source["parent_id"].(string); ok && id != "" {
					parent = id
				}
			}
			if len(identifiers) == 0 && scope.MaxPerSource > 0 && parentCounts[parent] >= scope.MaxPerSource {
				continue
			}
			parentCounts[parent]++
			res.Text = hydratedText
			res.AgentID = data["agent_id"]
			res.Tags = parseTags(data["tags"])
			if json.Valid([]byte(data["metadata"])) {
				res.Metadata = json.RawMessage(data["metadata"])
			}
			results = append(results, res)
			if len(results) == hydrationLimit {
				break
			}
		}
	}
	if len(identifiers) > 0 {
		diag.RerankCandidates = len(results)
		rerankRecallIdentifiers(results, identifiers)
	}
	if len(identifiers) > 0 {
		selected := make([]SearchResult, 0, limit)
		parentCounts = map[string]int{}
		for _, res := range results {
			parent := res.ID
			var metadata struct {
				Source struct {
					ParentID string `json:"parent_id"`
				} `json:"source"`
			}
			if json.Unmarshal(res.Metadata, &metadata) == nil && metadata.Source.ParentID != "" {
				parent = metadata.Source.ParentID
			}
			if scope.MaxPerSource > 0 && parentCounts[parent] >= scope.MaxPerSource {
				continue
			}
			parentCounts[parent]++
			selected = append(selected, res)
			if len(selected) == limit {
				break
			}
		}
		results = selected
	}
	ids := make([]string, len(results))
	for i, res := range results {
		ids[i] = res.ID
	}
	bumpAccessCounts(ctx, rdb, tenant, ids)
	diag.ReturnedDocuments = len(results)
	return results, diag, nil
}
