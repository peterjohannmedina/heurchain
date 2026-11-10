package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCombinedScopeFiltersBeforeLexicalRetention(t *testing.T) {
	for _, postings := range []bool{false, true} {
		t.Run(fmt.Sprintf("postings=%v", postings), func(t *testing.T) {
			f := newRecallFixture(t)
			for i := 0; i < maxQdrantCandidates+5; i++ {
				w := f.call(t, "/store", map[string]interface{}{"id": fmt.Sprintf("a-%03d", i), "text": "needle", "embedding": []float32{1, 0}, "metadata": map[string]interface{}{"memory_kind": "creative"}})
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			}
			w := f.call(t, "/store", map[string]interface{}{"id": "z-eligible", "text": "needle extra context", "embedding": []float32{1, 0}, "metadata": map[string]interface{}{"memory_kind": "factual", "valid_from": "2026-01-01T00:00:00Z"}})
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if postings {
				if err := markBM25PostingsComplete(context.Background(), f.s.rdb, "t1"); err != nil {
					t.Fatal(err)
				}
			}
			hits, _ := f.search(t, map[string]interface{}{"text": "needle", "retrieval_mode": "lexical", "memory_kind": "factual", "as_of": "2026-06-01T00:00:00Z", "limit": 1})
			if len(hits) != 1 || hits[0].ID != "z-eligible" {
				t.Fatal("scope applied after bounded retention", hits)
			}
		})
	}
}

func TestCombinedStrictCacheSeparatesExplicitProviders(t *testing.T) {
	var ollama, openai atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			ollama.Add(1)
			respondJSON(w, map[string]interface{}{"model": "fixture", "embeddings": [][]float32{{1, 0}}})
		case "/embeddings":
			openai.Add(1)
			respondJSON(w, map[string]interface{}{"data": []map[string]interface{}{{"embedding": []float32{0, 1}}}})
		default:
			t.Error("unexpected provider path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	s := &Server{config: &Config{}}
	s.config.Embed.StrictInput = true
	s.config.Embed.QueryCacheEntries = 10
	s.config.Embed.QueryCacheTTLSeconds = 60
	s.config.Embed.ModelRevision = "fixed-fixture"
	s.config.Embed.OllamaURL, s.config.Embed.OllamaModel = provider.URL, "fixture"
	s.config.Embed.OpenAIURL, s.config.Embed.OpenAIModel, s.config.Embed.OpenAIKey = provider.URL+"/embeddings", "fixture", "local-fixture"
	s.config.Embed.Provider = "ollama"
	first, hit, err := s.embedSearchText(context.Background(), "t1", "same input")
	if err != nil || hit || !reflect.DeepEqual(first, []float32{1, 0}) {
		t.Fatal(first, hit, err)
	}
	s.config.Embed.Provider = "openai"
	second, hit, err := s.embedSearchText(context.Background(), "t1", "same input")
	if err != nil || hit || !reflect.DeepEqual(second, []float32{0, 1}) || openai.Load() != 1 || ollama.Load() != 1 {
		t.Fatal("provider cache substitution", second, hit, err)
	}
	_, hit, err = s.embedSearchText(context.Background(), "t1", "same input")
	if err != nil || !hit || openai.Load() != 1 {
		t.Fatal("selected provider not cached", hit, err)
	}
	if _, _, err := s.embedSearchText(context.Background(), "t1", strings.Repeat("x", maxEmbedTextBytes+1)); err == nil {
		t.Fatal("strict provider silently truncated")
	}
}

func TestCombinedTenantBoundariesOnMemorySessionAndVault(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "private", "tenant one private source", "codex", nil, 1)
	for name, handler := range map[string]http.HandlerFunc{"memory": f.s.handleMemoryGet, "session": f.s.handleSessionCtx, "vault": f.s.handleVaultExport} {
		path, method, body := "/memory/get", http.MethodPost, `{"id":"private","query":"private"}`
		if name == "session" {
			path, method = "/session/test/context", http.MethodGet
		}
		if name == "vault" {
			path, method = "/vault/export?tenant_id=t2", http.MethodGet
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-HeurChain-API-Key", "test-key")
		r.Header.Set("X-HeurChain-Tenant", "t2")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 401 || strings.Contains(w.Body.String(), "tenant one private source") {
			t.Fatal(name, w.Code, w.Body)
		}
	}
}

func TestCombinedProductionReindexRetriesChangedSource(t *testing.T) {
	for _, continuous := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuous=%v", continuous), func(t *testing.T) {
			f := newRecallFixture(t)
			ctx := context.Background()
			f.store(t, "source", "old text", "codex", nil, 1)
			var calls atomic.Int32
			q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points") {
					n := calls.Add(1)
					if continuous || n == 1 {
						if err := f.s.rdb.HSet(ctx, vecKey("t1", "source"), "text", fmt.Sprintf("replacement %d", n), "index_revision", fmt.Sprintf("revision-%d", n)).Err(); err != nil {
							t.Error(err)
						}
					}
				}
				respondJSON(w, map[string]interface{}{})
			}))
			defer q.Close()
			defaultQdrantClient = NewQdrantClient(q.URL, 2)
			err := reindexActiveDoc(ctx, f.s.rdb, "t1", "source")
			if continuous {
				if err == nil || calls.Load() != 3 {
					t.Fatal("unbounded/false successful repair", calls.Load(), err)
				}
			} else {
				data, readErr := f.s.rdb.HGetAll(ctx, vecKey("t1", "source")).Result()
				if err != nil || readErr != nil || calls.Load() != 2 || data["indexed_revision"] != semanticRevision(data) {
					t.Fatal("revision retry not acknowledged", calls.Load(), data, err, readErr)
				}
			}
		})
	}
}

func TestCombinedReindexCannotAcknowledgeRejectedProjection(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	if err := rdb.HSet(ctx, vecKey("t1", "source"), "text", "unchanged outer snapshot").Err(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := reindexActiveDocWith(ctx, rdb, "t1", "source", func(map[string]string) error {
		calls++
		// The semantic writer observed another snapshot while the outer reader
		// saw unchanged contents. A failed acknowledgement must force retry.
		return errSemanticSourceChanged
	})
	if err == nil || calls != 3 {
		t.Fatal("rejected projection acknowledged", calls, err)
	}
}
