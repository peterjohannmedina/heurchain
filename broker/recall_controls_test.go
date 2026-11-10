package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRecallScopeValidityAndKind(t *testing.T) {
	o := RecallOptions{AsOf: "2026-03-15T00:00:00Z", MemoryKind: "factual"}
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"memory_kind":"factual","valid_from":"2026-03-15T00:00:00Z"}`, true},
		{`{"memory_kind":"factual","valid_until":"2026-03-15T00:00:00Z"}`, false},
		{`{"memory_kind":"creative"}`, false},
		{`{"memory_kind":"factual","valid_from":"bad"}`, false},
		{`{}`, false},
	} {
		if recallMetadataEligible(tc.raw, o) != tc.want {
			t.Errorf("eligibility: %s", tc.raw)
		}
	}
	if recallMetadataValid(json.RawMessage(`{"valid_from":"bad"}`)) == nil {
		t.Fatal("invalid date accepted")
	}
	if len(recallFilter("reader", nil, o)["must_not"].([]interface{})) != 2 {
		t.Fatal("missing filter pushdown")
	}
}

func TestLexicalSearchWorksWithoutDenseBackends(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "source", "unique calibration phrase", "reader", nil, 1)
	f.s.config.Embed.OllamaURL = "http://127.0.0.1:1"
	f.fail.Store(true)
	w := f.call(t, "/search", map[string]interface{}{"text": "unique calibration", "retrieval_mode": "lexical", "limit": 1})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var body struct {
		Results   []SearchResult    `json:"results"`
		Retrieval recallDiagnostics `json:"retrieval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.Results[0].ID != "source" || body.Retrieval.DenseStatus != "skipped" {
		t.Fatal(w.Body.String())
	}
}

func TestFusionWeightsAndDefaultCompatibility(t *testing.T) {
	lexical := map[string]float64{"lexical": 10, "dense": 1}
	dense := []QdrantSearchResult{{ID: "dense", Score: 1}}
	o := RecallOptions{}
	_ = o.normalize()
	if fuseResultsWithOptions(lexical, dense, 0, o)[0].ID != "dense" {
		t.Fatal("default changed")
	}
	o.RRFK = 1
	o.LexicalWeight = 10
	o.DenseWeight = .1
	if fuseResultsWithOptions(lexical, dense, 0, o)[0].ID != "lexical" {
		t.Fatal("weights not applied")
	}
	o.RRFK = -1
	if o.normalize() == nil {
		t.Fatal("negative setting accepted")
	}
}

func TestStrictEmbeddingCacheIsolationAndBounds(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/embed" {
			t.Error(r.URL.Path)
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["truncate"] != false {
			t.Error("truncation allowed")
		}
		respondJSON(w, map[string]interface{}{"model": "fixture", "embeddings": [][]float32{{1, 0}}})
	}))
	defer provider.Close()
	s := &Server{config: &Config{}}
	s.config.Embed.OllamaURL = provider.URL
	s.config.Embed.OllamaModel = "fixture"
	s.config.Embed.StrictInput = true
	s.config.Embed.ModelRevision = "digest"
	s.config.Embed.QueryCacheEntries = 1
	s.config.Embed.QueryCacheTTLSeconds = 60
	ctx := context.Background()
	v, hit, err := s.embedSearchText(ctx, "a", "Case")
	if err != nil || hit {
		t.Fatalf("first: %v", err)
	}
	v[0] = 99
	v, hit, err = s.embedSearchText(ctx, "a", "Case")
	if err != nil || !hit || v[0] != 1 || calls.Load() != 1 {
		t.Fatal("cache alias/miss")
	}
	if _, hit, err = s.embedSearchText(ctx, "b", "Case"); err != nil || hit {
		t.Fatal("cross-tenant cache reuse")
	}
	if len(s.queryEmbeddings.values) != 1 {
		t.Fatal("unbounded cache")
	}
	if _, _, err = s.embedSearchText(ctx, "a", strings.Repeat("x", maxEmbedTextBytes+1)); err == nil {
		t.Fatal("silent truncation")
	}
}

func TestStrictEmbeddingDoesNotFallbackToAnotherModel(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "overlength", 400) }))
	defer primary.Close()
	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer fallback.Close()
	s := &Server{config: &Config{}}
	s.config.Embed.StrictInput = true
	s.config.Embed.OllamaURL = primary.URL
	s.config.Embed.OpenAIURL = fallback.URL
	s.config.Embed.OpenAIKey = "fixture"
	if _, err := s.embedText(context.Background(), "source"); err == nil {
		t.Fatal("provider failure hidden")
	}
	if calls.Load() != 0 {
		t.Fatal("embedding space silently changed")
	}
}
