package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestSemanticIndexRetriesAndPreservesSourceID(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	var fail atomic.Bool
	fail.Store(true)
	points := make(chan QdrantPoint, 2)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		if fail.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		if r.URL.Query().Get("wait") != "true" {
			t.Error("index acknowledgment must await applied write")
		}
		var body struct {
			Points []QdrantPoint `json:"points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Points) != 1 {
			t.Errorf("invalid upsert: %v", err)
			w.WriteHeader(400)
			return
		}
		points <- body.Points[0]
		w.WriteHeader(200)
	}))
	defer remote.Close()
	q := NewQdrantClient(remote.URL, 2)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "my.note", AgentID: "a", Text: "hello", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := indexSemanticDocument(ctx, rdb, q, "t1", "my.note"); err == nil {
		t.Fatal("index outage incorrectly acknowledged")
	}
	if n := rdb.ZCard(ctx, semanticPendingKey).Val(); n != 1 {
		t.Fatalf("lost pending work: %d", n)
	}
	fail.Store(false)
	if err := indexSemanticDocument(ctx, rdb, q, "t1", "my.note"); err != nil {
		t.Fatal(err)
	}
	p := <-points
	if _, err := uuid.Parse(p.ID); err != nil {
		t.Fatal("Qdrant wire ID is not UUID")
	}
	if p.Payload["source_id"] != "my.note" {
		t.Fatalf("lost source ID: %v", p.Payload)
	}
	data := rdb.HGetAll(ctx, vecKey("t1", "my.note")).Val()
	if p.Payload["index_revision"] != semanticRevision(data) || data["indexed_revision"] != semanticRevision(data) {
		t.Fatal("wrong indexed revision")
	}
	if n := rdb.ZCard(ctx, semanticPendingKey).Val(); n != 0 {
		t.Fatalf("ack left pending work: %d", n)
	}
}

func TestSemanticIndexConcurrentStoreRemainsPending(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Text: "old", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	old := rdb.HGetAll(ctx, vecKey("t1", "doc")).Val()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Text: "new", Embedding: []float32{0, 1}}); err != nil {
				t.Error(err)
			}
		}
		w.WriteHeader(200)
	}))
	defer remote.Close()
	if err := indexSemanticDocument(ctx, rdb, NewQdrantClient(remote.URL, 2), "t1", "doc"); err == nil {
		t.Fatal("concurrent store was acknowledged as indexed")
	}
	if n := rdb.ZCard(ctx, semanticPendingKey).Val(); n != 1 {
		t.Fatal("new version lost its indexing work")
	}
	hits, err := validateSemanticHits(ctx, rdb, "t1", "", nil, "", []QdrantSearchResult{{ID: "doc", Payload: map[string]interface{}{"index_revision": semanticRevision(old)}}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("stale vector returned: %v %v", hits, err)
	}
}

func TestSemanticIndexVaultRaceDeletesLatePoint(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	var deletes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			if _, err := moveActiveToVault(ctx, rdb, "t1", "doc"); err != nil {
				t.Error(err)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/delete") {
			deletes.Add(1)
		}
		w.WriteHeader(200)
	}))
	defer remote.Close()
	if err := indexSemanticDocument(ctx, rdb, NewQdrantClient(remote.URL, 2), "t1", "doc"); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 1 {
		t.Fatal("late index write was not deleted")
	}
	if n := rdb.ZCard(ctx, semanticPendingKey).Val(); n != 0 {
		t.Fatal("inactive work not cleared")
	}
	hits, err := validateSemanticHits(ctx, rdb, "t1", "", nil, "", []QdrantSearchResult{{ID: "doc"}})
	if err != nil || len(hits) > 0 {
		t.Fatal("vaulted hit escaped validation")
	}
}

func TestSemanticReconciliationBackfillsLegacyAndSkipsSilos(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := rdb.HSet(ctx, vecKey("t1", "legacy"), "embedding", "[1,0]", "text", "old note").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, vecSetKey("t1"), "legacy").Err(); err != nil {
		t.Fatal(err)
	}
	rdb.SAdd(ctx, "tenant:t1:silo:secret:vecs", "hidden")
	reconciler := new(semanticReconciler)
	for i := 0; i < 5; i++ {
		if err := reconciler.step(ctx, rdb); err != nil {
			t.Fatal(err)
		}
	}
	items := rdb.ZRange(ctx, semanticPendingKey, 0, -1).Val()
	if len(items) != 1 || items[0] != semanticMember("t1", "legacy") {
		t.Fatalf("unexpected backfill: %v", items)
	}
	data := rdb.HGetAll(ctx, vecKey("t1", "legacy")).Val()
	rdb.HSet(ctx, vecKey("t1", "legacy"), "indexed_revision", semanticRevision(data))
	rdb.ZRem(ctx, semanticPendingKey, semanticMember("t1", "legacy"))
	for i := 0; i < 5; i++ {
		if err := reconciler.step(ctx, rdb); err != nil {
			t.Fatal(err)
		}
	}
	if rdb.ZCard(ctx, semanticPendingKey).Val() != 0 {
		t.Fatal("already indexed legacy record requeued")
	}
}

func TestQdrantFiltersBeforeLimit(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Limit  int `json:"limit"`
			Filter struct {
				Must []map[string]interface{} `json:"must"`
			} `json:"filter"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Limit != 1 || len(body.Filter.Must) != 3 {
			t.Errorf("expected agent/tag/model filtering before limit: %+v", body)
		}
		if body.Filter.Must[0]["key"] != "agent_id" || body.Filter.Must[1]["key"] != "tags" {
			t.Error("missing source filters")
		}
		modelCondition := body.Filter.Must[2]
		if _, wrapped := modelCondition["filter"]; wrapped {
			t.Fatalf("model filter has unsupported REST filter wrapper: %+v", modelCondition)
		}
		if should, ok := modelCondition["should"].([]interface{}); !ok || len(should) != 2 {
			t.Fatalf("model compatibility filter has no two-way OR: %+v", modelCondition)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": []QdrantSearchResult{}})
	}))
	defer remote.Close()
	// Pin the filter shape through the production call sequence: recallFilterWithModel
	// builds the conditions and Search serializes them ahead of the limit. ID
	// restoration is resolveRecallHits' job (see TestRecallLegacyIDsAndTenantBoundary).
	hits, err := NewQdrantClient(remote.URL, 2).Search(context.Background(), "t1", []float32{1, 0}, 1, recallFilterWithModel("agent", []string{"tag"}, "ollama:model", RecallOptions{}))
	if err != nil || len(hits) != 0 {
		t.Fatalf("bad filtered search: %v %v", hits, err)
	}
}

func TestSemanticWorkerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := RunSemanticIndexWorker(ctx, newTestRedis(t), NewQdrantClient("http://127.0.0.1:1", 2))
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("index worker leaked on shutdown")
	}
}

func TestStoreClearsOldSearchFilters(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	for _, v := range []storedVector{{ID: "doc", AgentID: "old", Tags: []string{"old"}, Embedding: []float32{1, 0}}, {ID: "doc", Embedding: []float32{1, 0}}} {
		if err := storeVec(ctx, rdb, "t1", v); err != nil {
			t.Fatal(err)
		}
	}
	data := rdb.HGetAll(ctx, vecKey("t1", "doc")).Val()
	if data["agent_id"] != "" || len(parseTags(data["tags"])) != 0 {
		t.Fatal("replacement retained obsolete filters")
	}
}

func TestSemanticRetryBackoff(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", 503) }))
	defer remote.Close()
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := drainSemanticIndex(ctx, rdb, NewQdrantClient(remote.URL, 2)); err != nil {
		t.Fatal(err)
	}
	if due := rdb.ZScore(ctx, semanticPendingKey, semanticMember("t1", "doc")).Val(); due < float64(time.Now().Add(50*time.Second).UnixMilli()) {
		t.Fatal("failed indexing did not back off")
	}
	// A fresh store must replace the delayed job so new work is eligible.
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Embedding: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if due := rdb.ZScore(ctx, semanticPendingKey, semanticMember("t1", "doc")).Val(); due > float64(time.Now().Add(time.Second).UnixMilli()) {
		t.Fatal("new store inherited retry delay")
	}
}

func TestSemanticDrainDoesNotBackOffLockContention(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	member := semanticMember("t1", "doc")
	dueBefore := rdb.ZScore(ctx, semanticPendingKey, member).Val()
	lockKey := "tenant:t1:semantic-lock:doc"
	if err := rdb.Set(ctx, lockKey, "other-writer", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()
	failedBefore := semanticIndexMetrics.failed.Load()
	q := NewQdrantClient(remote.URL, 2)
	if err := drainSemanticIndex(ctx, rdb, q); err != nil {
		t.Fatal(err)
	}
	if semanticIndexMetrics.failed.Load() != failedBefore {
		t.Fatal("lock contention was counted as an indexing failure")
	}
	if dueAfter := rdb.ZScore(ctx, semanticPendingKey, member).Val(); dueAfter != dueBefore {
		t.Fatalf("lock contention changed due time: before=%v after=%v", dueBefore, dueAfter)
	}
	if requests.Load() != 0 {
		t.Fatal("contended worker unexpectedly reached Qdrant")
	}
	if err := rdb.Del(ctx, lockKey).Err(); err != nil {
		t.Fatal(err)
	}
	if err := drainSemanticIndex(ctx, rdb, q); err != nil {
		t.Fatal(err)
	}
	if n := rdb.ZCard(ctx, semanticPendingKey).Val(); n != 0 {
		t.Fatalf("eligible work was not indexed after lease release: %d", n)
	}
}

func TestSemanticDrainDiscardsMalformedWorkAndContinues(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, semanticPendingKey, redis.Z{Score: 0, Member: "not-json"}).Err(); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()
	discardedBefore := semanticIndexMetrics.discarded.Load()
	if err := drainSemanticIndex(ctx, rdb, NewQdrantClient(remote.URL, 2)); err != nil {
		t.Fatal(err)
	}
	if n := rdb.ZCard(ctx, semanticPendingKey).Val(); n != 0 {
		t.Fatalf("outbox still contains %d entries", n)
	}
	if semanticIndexMetrics.discarded.Load() != discardedBefore+1 {
		t.Fatal("discarded work was not counted")
	}
	data := rdb.HGetAll(ctx, vecKey("t1", "doc")).Val()
	if data["indexed_revision"] == "" {
		t.Fatal("valid work after malformed entry was not indexed")
	}
}

func TestSemanticPointWrapsStoredEmbeddingParseError(t *testing.T) {
	_, err := semanticPoint("doc", map[string]string{"embedding": "not-json"})
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("expected wrapped JSON syntax error, got %v", err)
	}
}

func TestHybridSearchKeepsFilteredMatchBeyondUnfilteredTop(t *testing.T) {
	ctx := context.Background()
	s := newExportTestServer(t)
	s.config = &Config{}
	s.config.Auth.APIKeyHeader = "X-API-Key"
	s.config.Auth.TenantHeader = "X-Tenant"
	s.config.Embed.OllamaModel = "test-model"
	embedder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"embedding":[1,0]}`)) }))
	defer embedder.Close()
	s.config.Embed.OllamaURL = embedder.URL
	if _, err := s.db.Exec(`INSERT INTO tenants (id,name,email) VALUES ('t1','Test','test@example.test')`); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(cachedAuth{KeyID: 1, TenantID: "t1"})
	if err := s.rdb.Set(ctx, authCacheKey("test-key"), auth, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []storedVector{
		{ID: "a-other", AgentID: "other", Text: "memory", Embedding: []float32{1, 0}, Model: "ollama:test-model"},
		{ID: "z-wanted", AgentID: "wanted", Text: "memory useful explanation", Embedding: []float32{0.9, 0.1}, Model: "ollama:test-model"},
	} {
		if err := storeVec(ctx, s.rdb, "t1", v); err != nil {
			t.Fatal(err)
		}
		if err := updateBM25Index(ctx, s.rdb, "t1", v.ID, v.Text); err != nil {
			t.Fatal(err)
		}
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&request)
		id := "a-other"
		if filter, ok := request["filter"].(map[string]interface{}); ok {
			if must, ok := filter["must"].([]interface{}); ok {
				for _, raw := range must {
					if m, ok := raw.(map[string]interface{}); ok && m["key"] == "agent_id" {
						id = "z-wanted"
					}
				}
			}
		}
		point, err := semanticPoint(id, s.rdb.HGetAll(ctx, vecKey("t1", id)).Val())
		if err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": []QdrantSearchResult{{ID: point.ID, Score: 1, Payload: point.Payload}}})
	}))
	defer remote.Close()
	original := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(remote.URL, 2)
	defer func() { defaultQdrantClient = original }()
	req := httptest.NewRequest(http.MethodPost, "/search", strings.NewReader(`{"text":"memory","agent_id":"wanted","limit":1}`))
	req.Header.Set("X-API-Key", "test-key")
	req.Header.Set("X-Tenant", "t1")
	response := httptest.NewRecorder()
	s.handleSearch(response, req)
	var body struct {
		Results  []SearchResult `json:"results"`
		Degraded bool           `json:"degraded"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(body.Results) != 1 || body.Results[0].ID != "z-wanted" || body.Degraded {
		t.Fatalf("filtered match lost: %d %s", response.Code, response.Body.String())
	}
}

func TestSemanticWriterLeasePreservesPendingWork(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	rdb.Set(ctx, "tenant:t1:semantic-lock:doc", "other-writer", time.Minute)
	if err := indexSemanticDocument(ctx, rdb, NewQdrantClient("http://127.0.0.1:1", 2), "t1", "doc"); err == nil {
		t.Fatal("concurrent writer entered lease")
	}
	if _, err := rdb.ZScore(ctx, semanticPendingKey, semanticMember("t1", "doc")).Result(); err == redis.Nil {
		t.Fatal("busy writer dropped pending work")
	}
}

func TestSemanticReconciliationDetectsLegacyOverwrite(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc", Text: "old", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	data := rdb.HGetAll(ctx, vecKey("t1", "doc")).Val()
	indexed := semanticRevision(data)
	rdb.HSet(ctx, vecKey("t1", "doc"), "indexed_revision", indexed)
	rdb.ZRem(ctx, semanticPendingKey, semanticMember("t1", "doc"))
	// A rolled-back broker HSETs content but knows nothing of index_revision.
	rdb.HSet(ctx, vecKey("t1", "doc"), "text", "new", "embedding", "[0,1]")
	hits, err := validateSemanticHits(ctx, rdb, "t1", "", nil, "", []QdrantSearchResult{{ID: "doc", Payload: map[string]interface{}{"index_revision": indexed}}})
	if err != nil || len(hits) != 0 {
		t.Fatal("legacy overwrite retained stale semantic hit")
	}
	reconciler := new(semanticReconciler)
	for i := 0; i < 3; i++ {
		if err := reconciler.step(ctx, rdb); err != nil {
			t.Fatal(err)
		}
	}
	if rdb.ZCard(ctx, semanticPendingKey).Val() != 1 {
		t.Fatal("legacy overwrite not scheduled for repair")
	}
}
