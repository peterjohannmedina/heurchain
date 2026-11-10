package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGraphRequiresExplicitTenantAllowlist(t *testing.T) {
	old := heurgraphCfg
	t.Cleanup(func() { heurgraphCfg = old })
	heurgraphCfg.Enabled = true
	heurgraphCfg.TenantAllowlist = nil
	if graphTenantEnabled("t1") {
		t.Fatal("empty allowlist enabled tenant")
	}
	heurgraphCfg.TenantAllowlist = []string{"t1"}
	if !graphTenantEnabled("t1") || graphTenantEnabled("t2") {
		t.Fatal("tenant allowlist ignored")
	}
	heurgraphCfg.Enabled = false
	if graphTenantEnabled("t1") {
		t.Fatal("server gate ignored")
	}
}

func TestRecallRejectsNullMetadataBeforeEmbedding(t *testing.T) {
	f := newRecallFixture(t)
	f.s.config.Embed.OllamaURL = "http://127.0.0.1:1"
	w := f.call(t, "/store", map[string]interface{}{"id": "null-metadata", "text": "evidence", "metadata": nil})
	if w.Code != 400 {
		t.Fatalf("metadata:null: %d %s", w.Code, w.Body.String())
	}
	if f.s.rdb.Exists(context.Background(), vecKey("t1", "null-metadata")).Val() != 0 {
		t.Fatal("invalid metadata was stored")
	}
}

func TestScopedRecallPreservesLifecycleFilters(t *testing.T) {
	f := newRecallFixture(t)
	for _, project := range []string{"alpha", "beta"} {
		w := f.call(t, "/store", map[string]interface{}{"id": project, "text": "RX-12 calibration", "embedding": []float32{1, 0}, "metadata": map[string]interface{}{"project": project, "memory_kind": "factual"}})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	for _, mode := range []string{"lexical", "hybrid", "dense"} {
		results, _ := f.search(t, map[string]interface{}{"text": "RX-12 calibration", "retrieval_mode": mode, "memory_kind": "factual", "rerank_mode": "exact_identifiers", "filters": map[string]interface{}{"metadata": map[string]interface{}{"project": "alpha"}}})
		if len(results) != 1 || results[0].ID != "alpha" {
			t.Fatalf("%s bypassed lifecycle filter: %+v", mode, results)
		}
	}
}

func TestGraphCannotBridgeThroughFilteredMemory(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "bridge", "alpha bridge", "a", nil, .1)
	f.store(t, "remote", "remote evidence", "a", nil, .1)
	ctx := context.Background()
	if err := f.s.rdb.HSet(ctx, vecKey("t1", "bridge"), "metadata", `{"project":"other"}`).Err(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.rdb.HSet(ctx, vecKey("t1", "remote"), "metadata", `{"project":"target"}`).Err(); err != nil {
		t.Fatal(err)
	}
	graphTestEdge(t, f, "r1", "alpha", "beta", "bridge")
	graphTestEdge(t, f, "r2", "beta", "gamma", "remote")
	ctx = context.WithValue(ctx, memoryFilterContextKey{}, &memoryFilters{Metadata: map[string]interface{}{"project": "target"}})
	result, err := traverseGraph(ctx, f.s.rdb, f.s.db, "t1", "alpha", "", nil, RecallOptions{GraphHops: 2}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ranks) != 0 {
		t.Fatalf("filtered source bridged graph: %+v", result)
	}
}

func TestStoreInvalidatesGraphCompletionWithEdges(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "source", "alpha evidence", "a", nil, 1)
	graphTestEdge(t, f, "r1", "alpha", "beta", "source")
	// Same-content retries must not leave a complete marker with no edges.
	f.store(t, "source", "alpha evidence", "a", nil, 1)
	if graphCount(t, f.s.db, "graph_extractions") != 0 || graphCount(t, f.s.db, "graph_relations") != 0 {
		t.Fatal("store retained stale graph projection or ledger")
	}
}

func TestSearchOpenAPIIncludesScopedControls(t *testing.T) {
	spec := openAPISpec()
	schemas := spec["components"].(map[string]interface{})["schemas"].(map[string]interface{})
	props := schemas["SearchRequest"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, name := range []string{"filters", "retrieval_mode", "rrf_k", "lexical_weight", "dense_weight", "as_of", "memory_kind", "max_per_source", "qdrant_candidates", "rerank_mode", "graph_mode"} {
		if props[name] == nil {
			t.Errorf("missing /search control %s", name)
		}
	}
	if _, err := json.Marshal(spec); err != nil {
		t.Fatal(err)
	}
}
