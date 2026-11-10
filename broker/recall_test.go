package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recallFixture struct {
	s          *Server
	mu         sync.Mutex
	points     map[string]QdrantPoint
	fail       atomic.Bool
	lastFilter map[string]interface{}
}

func newRecallFixture(t *testing.T) *recallFixture {
	t.Helper()
	f := &recallFixture{s: newExportTestServer(t), points: map[string]QdrantPoint{}}
	if _, err := f.s.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ('t1', 'Recall fixture', 'recall@example.test')`); err != nil {
		t.Fatal(err)
	}
	f.s.config = &Config{}
	f.s.config.Auth.APIKeyHeader = "X-HeurChain-API-Key"
	f.s.config.Auth.TenantHeader = "X-HeurChain-Tenant"
	auth, _ := json.Marshal(cachedAuth{KeyID: 1, TenantID: "t1", Epoch: 0})
	if err := f.s.rdb.Set(context.Background(), authCacheKey("test-key"), auth, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Errorf("unexpected embedding route %s", r.URL.Path)
		}
		respondJSON(w, map[string]interface{}{"embedding": []float32{1, 0}})
	}))
	t.Cleanup(embed.Close)
	f.s.config.Embed.OllamaURL = embed.URL
	f.s.config.Embed.OllamaModel = "fixture"
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() {
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/delete"):
			if r.URL.Query().Get("wait") != "true" {
				t.Error("delete acknowledged without wait=true")
			}
			var body struct {
				Points []string `json:"points"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			for _, id := range body.Points {
				delete(f.points, id)
			}
			respondJSON(w, map[string]interface{}{})
		case r.Method == http.MethodGet:
			respondJSON(w, map[string]interface{}{})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			if r.URL.Query().Get("wait") != "true" {
				t.Error("projection acknowledged without wait=true")
			}
			var body struct {
				Points []QdrantPoint `json:"points"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			for _, point := range body.Points {
				f.points[point.ID] = point
			}
			respondJSON(w, map[string]interface{}{})
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			var body struct {
				Limit  int                    `json:"limit"`
				Filter map[string]interface{} `json:"filter"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			f.lastFilter = body.Filter
			hits := []QdrantSearchResult{}
			for _, point := range f.points {
				match := fixtureRecallFilterMatches(point.Payload, body.Filter)
				if match {
					hits = append(hits, QdrantSearchResult{ID: point.ID, Score: point.Vector[0], Payload: point.Payload})
				}
			}
			sort.Slice(hits, func(i, j int) bool {
				if hits[i].Score != hits[j].Score {
					return hits[i].Score > hits[j].Score
				}
				return hits[i].ID < hits[j].ID
			})
			if len(hits) > body.Limit {
				hits = hits[:body.Limit]
			}
			respondJSON(w, map[string]interface{}{"result": hits})
		default:
			t.Errorf("unexpected Qdrant request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	t.Cleanup(q.Close)
	old := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(q.URL, 2)
	t.Cleanup(func() { defaultQdrantClient = old })
	return f
}

func (f *recallFixture) call(t *testing.T, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	req.Header.Set("X-HeurChain-API-Key", "test-key")
	req.Header.Set("X-HeurChain-Tenant", "t1")
	w := httptest.NewRecorder()
	if path == "/store" {
		f.s.handleStore(w, req)
	} else {
		f.s.handleSearch(w, req)
	}
	return w
}

func (f *recallFixture) store(t *testing.T, id, text, agent string, tags []string, score float32) {
	t.Helper()
	w := f.call(t, "/store", map[string]interface{}{"id": id, "text": text, "agent_id": agent, "tags": tags, "embedding": []float32{score, 1}, "metadata": map[string]string{"source": "fixture"}})
	if w.Code != http.StatusOK {
		t.Fatalf("store: %d %s", w.Code, w.Body.String())
	}
	var body struct{ Stored, Indexed bool }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Stored || !body.Indexed {
		t.Fatalf("store not indexed: %s", w.Body.String())
	}
}

func (f *recallFixture) search(t *testing.T, body map[string]interface{}) ([]SearchResult, recallDiagnostics) {
	t.Helper()
	w := f.call(t, "/search", body)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	var reply struct {
		Results     []SearchResult    `json:"results"`
		Diagnostics recallDiagnostics `json:"retrieval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply.Results, reply.Diagnostics
}

func TestRecallStoreDenseRoundTripAndUpdate(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "book-page-42", "first exact passage", "reader", []string{"book"}, 1)
	hits, _ := f.search(t, map[string]interface{}{"text": "paraphrase without lexical overlap"})
	if len(hits) != 1 || hits[0].ID != "book-page-42" || hits[0].Text != "first exact passage" || hits[0].QdrantRank != 1 {
		t.Fatalf("dense roundtrip: %+v", hits)
	}
	if string(hits[0].Metadata) != "{\"source\":\"fixture\"}" {
		t.Fatalf("metadata lost: %s", hits[0].Metadata)
	}
	f.store(t, "book-page-42", "updated exact passage", "reader", []string{"book"}, 0.5)
	hits, _ = f.search(t, map[string]interface{}{"text": "another paraphrase"})
	if len(hits) != 1 || hits[0].Text != "updated exact passage" || *hits[0].QdrantScore != 0.5 {
		t.Fatalf("update left stale projection: %+v", hits)
	}
	count, err := f.s.rdb.HGet(context.Background(), vecKey("t1", "book-page-42"), "access_count").Int()
	if err != nil || count != 2 {
		t.Fatalf("search reinforcement = %d, %v", count, err)
	}
}

func TestRecallFilterBeforeCandidateAndFinalLimits(t *testing.T) {
	f := newRecallFixture(t)
	for i := 0; i < 4; i++ {
		f.store(t, fmt.Sprintf("a-other-%d", i), "needle needle", "other", []string{"other"}, 1)
	}
	f.store(t, "z-eligible", "needle", "reader", []string{"book"}, 0.1)
	hits, _ := f.search(t, map[string]interface{}{"text": "needle", "limit": 1, "qdrant_candidates": 1, "agent_id": "reader", "tags": []string{"book"}})
	if len(hits) != 1 || hits[0].ID != "z-eligible" || hits[0].QdrantRank != 1 {
		t.Fatalf("eligible hit lost: %+v", hits)
	}
	f.mu.Lock()
	filter := f.lastFilter
	f.mu.Unlock()
	if len(filter) == 0 {
		t.Fatal("dense filtering was not pushed down")
	}
	count, _ := f.s.rdb.HGet(context.Background(), vecKey("t1", "a-other-0"), "access_count").Int()
	if count != 1 {
		t.Fatalf("excluded record reinforced: %d", count)
	}
}

func TestRecallLegacyIDsAndTenantBoundary(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "legacy-document", "source passage", "", nil, 1)
	f.mu.Lock()
	for id, p := range f.points {
		delete(p.Payload, "source_id")
		f.points[id] = p
	}
	f.mu.Unlock()
	hits, _ := f.search(t, map[string]interface{}{"text": "paraphrase"})
	if len(hits) != 1 || hits[0].ID != "legacy-document" {
		t.Fatalf("legacy mapping: %+v", hits)
	}
	foreign := "foreign-document"
	point := foreign
	ensureUUID(&point)
	resolved, n := resolveRecallHits([]QdrantSearchResult{{ID: point, Payload: map[string]interface{}{"source_id": foreign}}}, []string{"legacy-document"}, []string{"legacy-document"})
	if len(resolved) != 0 || n != 1 {
		t.Fatal("inactive/foreign ID accepted")
	}
	// UUID-shaped logical IDs can alias a hashed logical ID. Old ambiguous
	// points must not be assigned arbitrarily; new source_id disambiguates.
	alias := "legacy-document"
	ensureUUID(&alias)
	resolved, n = resolveRecallHits([]QdrantSearchResult{{ID: alias}}, []string{"legacy-document", alias}, []string{"legacy-document", alias})
	if len(resolved) != 0 || n != 1 {
		t.Fatal("ambiguous legacy alias accepted")
	}
	resolved, n = resolveRecallHits([]QdrantSearchResult{{ID: alias, Payload: map[string]interface{}{"source_id": "legacy-document"}}}, []string{"legacy-document", alias}, []string{"legacy-document", alias})
	if len(resolved) != 1 || resolved[0].ID != "legacy-document" || n != 0 {
		t.Fatal("source_id did not disambiguate")
	}
	upper := "550E8400-E29B-41D4-A716-446655440000"
	resolved, n = resolveRecallHits([]QdrantSearchResult{{ID: strings.ToLower(upper)}}, []string{upper}, []string{upper})
	if len(resolved) != 1 || resolved[0].ID != upper || n != 0 {
		t.Fatal("UUID normalization lost logical ID")
	}
}

func TestRecallConcurrentStoresKeepCorpusStatistics(t *testing.T) {
	f := newRecallFixture(t)
	errors := make(chan error, 8)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			a, b := f.s.storeWithIndexes(context.Background(), "t1", storedVector{ID: fmt.Sprintf("concurrent-%d", i), Text: "concurrent memory", Embedding: []float32{1, 0}, Metadata: json.RawMessage(`{}`)})
			if a != nil {
				errors <- a
			} else if b != nil {
				errors <- b
			}
		}(i)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	count, err := f.s.rdb.Get(context.Background(), bm25MetaDocCountKey("t1")).Int()
	if err != nil || count != 8 {
		t.Fatalf("concurrent BM25 doc_count = %d, %v", count, err)
	}
	f.mu.Lock()
	points := len(f.points)
	f.mu.Unlock()
	if points != 8 {
		t.Fatalf("dense points = %d", points)
	}
}

func TestRecallZeroLexicalScoresCannotReorderDense(t *testing.T) {
	hits := fuseResults(map[string]float64{"a": 0, "z": 0}, []QdrantSearchResult{{ID: "z", Score: 1}, {ID: "a", Score: 0.5}}, 2)
	if len(hits) != 2 || hits[0].ID != "z" || hits[0].BM25Rank != 0 {
		t.Fatalf("zero lexical evidence changed ranking: %+v", hits)
	}
	if got := rrfScore(1, 0); math.Abs(got-1.0/61) > 1e-12 {
		t.Fatalf("absent leg received credit: %g", got)
	}
	if got := rrfScore(0, 0); got != 0 {
		t.Fatalf("absent both = %g", got)
	}
	rdb := newTestRedis(t)
	if err := updateBM25Index(context.Background(), rdb, "t1", "a", "unrelated"); err != nil {
		t.Fatal(err)
	}
	scores, err := scoreBM25(context.Background(), rdb, "t1", []string{"needle"}, []string{"a", "missing"})
	if err != nil || len(scores) != 0 {
		t.Fatalf("nonmatches retained: %v %v", scores, err)
	}
}

func TestRecallIndexFailureIsExplicitAndRetryable(t *testing.T) {
	f := newRecallFixture(t)
	f.fail.Store(true)
	w := f.call(t, "/store", map[string]interface{}{"id": "retry", "text": "needle", "embedding": []float32{1, 0}})
	var body struct{ Stored, Indexed bool }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusAccepted || !body.Stored || body.Indexed {
		t.Fatalf("partial write hidden: %d %s", w.Code, w.Body.String())
	}
	hits, diag := f.search(t, map[string]interface{}{"text": "needle"})
	if len(hits) != 1 || diag.DenseStatus != "failed" {
		t.Fatalf("degraded search not explicit: %+v %+v", hits, diag)
	}
	hits, diag = f.search(t, map[string]interface{}{"text": "no-match"})
	if len(hits) != 0 || diag.DenseStatus != "failed" {
		t.Fatalf("degraded empty result not explicit: %+v %+v", hits, diag)
	}
	f.fail.Store(false)
	f.store(t, "retry", "needle", "", nil, 1)
	hits, diag = f.search(t, map[string]interface{}{"text": "paraphrase"})
	if len(hits) != 1 || diag.DenseStatus != "ok" {
		t.Fatalf("retry failed: %+v %+v", hits, diag)
	}
}

func TestRecallHydrationRefillsAndDoesNotReinforceMissingText(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "a-missing", "needle needle", "", nil, 1)
	f.store(t, "z-good", "needle", "", nil, 0.5)
	// Exercise a legacy point without a source-revision payload. Versioned
	// points whose body changes are rejected before fusion instead.
	if err := f.s.rdb.HDel(context.Background(), vecKey("t1", "a-missing"), "text", "index_revision", "indexed_revision").Err(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	pointID := "a-missing"
	ensureUUID(&pointID)
	delete(f.points[pointID].Payload, "index_revision")
	f.mu.Unlock()
	hits, diag := f.search(t, map[string]interface{}{"text": "needle", "limit": 1})
	if len(hits) != 1 || hits[0].ID != "z-good" || diag.MissingText != 1 {
		t.Fatalf("hydration did not refill: %+v %+v", hits, diag)
	}
	count, _ := f.s.rdb.HGet(context.Background(), vecKey("t1", "a-missing"), "access_count").Int()
	if count != 1 {
		t.Fatalf("missing text reinforced: %d", count)
	}
}

// Match the recursive REST filter shape used by the local Qdrant fixture.
func fixtureRecallFilterMatches(payload map[string]interface{}, condition map[string]interface{}) bool {
	if clauses, ok := condition["must_not"].([]interface{}); ok {
		for _, raw := range clauses {
			if fixtureRecallFilterMatches(payload, raw.(map[string]interface{})) {
				return false
			}
		}
	}
	if clauses, ok := condition["must"].([]interface{}); ok {
		for _, raw := range clauses {
			if !fixtureRecallFilterMatches(payload, raw.(map[string]interface{})) {
				return false
			}
		}
	}
	if clauses, ok := condition["should"].([]interface{}); ok {
		found := false
		for _, raw := range clauses {
			if fixtureRecallFilterMatches(payload, raw.(map[string]interface{})) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if empty, ok := condition["is_empty"].(map[string]interface{}); ok {
		value, exists := payload[empty["key"].(string)]
		return !exists || value == nil
	}
	key, ok := condition["key"].(string)
	if !ok {
		return true
	}
	if bounds, ok := condition["range"].(map[string]interface{}); ok {
		value, ok := payload[key].(string)
		if !ok {
			return false
		}
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return false
		}
		for op, raw := range bounds {
			text, ok := raw.(string)
			if !ok {
				return false
			}
			bound, err := time.Parse(time.RFC3339, text)
			if err != nil {
				return false
			}
			switch op {
			case "gt":
				if !at.After(bound) {
					return false
				}
			case "gte":
				if at.Before(bound) {
					return false
				}
			case "lt":
				if !at.Before(bound) {
					return false
				}
			case "lte":
				if at.After(bound) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	term := condition["match"].(map[string]interface{})
	if value, ok := term["value"]; ok {
		return payload[key] == value
	}
	if any, ok := term["any"].([]interface{}); ok {
		if value, ok := payload[key].(string); ok {
			for _, want := range any {
				if value == want {
					return true
				}
			}
			return false
		}
		stored, _ := payload[key].([]interface{})
		for _, want := range any {
			for _, have := range stored {
				if want == have {
					return true
				}
			}
		}
		return false
	}
	return false
}
