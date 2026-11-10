package main

import (
	"context"
	"encoding/json"
	"testing"
)

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

func TestSearchOpenAPIIncludesScopedControls(t *testing.T) {
	spec := openAPISpec()
	schemas := spec["components"].(map[string]interface{})["schemas"].(map[string]interface{})
	props := schemas["SearchRequest"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, name := range []string{"filters", "retrieval_mode", "rrf_k", "lexical_weight", "dense_weight", "as_of", "memory_kind", "max_per_source", "qdrant_candidates", "rerank_mode"} {
		if props[name] == nil {
			t.Errorf("missing /search control %s", name)
		}
	}
	if _, err := json.Marshal(spec); err != nil {
		t.Fatal(err)
	}
}
