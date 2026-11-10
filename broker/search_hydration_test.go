package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A document whose id is not a UUID lives under two different names: Redis keys
// the record by the id the client chose, and Qdrant holds the point under the
// UUIDv5 derived from it (UpsertPoint rewrites the id because Qdrant requires a
// UUID). /search has to translate back before it fuses the two retrieval legs
// or hydrates the result.
//
// Before the translation existed, this document was returned twice — once per
// leg, since RRF keyed the Qdrant hit by its rewritten id and the BM25 hit by
// the real one — and neither copy was hydrated, because the vecKey(...) read
// used the rewritten id and found nothing. The client-visible damage was a
// search result with no text and no agent, next to a duplicate of itself.
func TestSearchHydratesNonUUIDDocumentFoundByBothLegs(t *testing.T) {
	ctx := context.Background()
	s := newExportTestServer(t)

	// The query is embedded by the handler before either leg runs.
	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"embedding": []float32{0.5, 0.5, 0.5, 0.5},
		})
	}))
	t.Cleanup(embed.Close)
	s.config.Embed.OllamaURL = embed.URL

	// Qdrant answers under the id it actually holds, with the caller's id in the
	// payload — exactly what UpsertPoint writes for a non-UUID id.
	normalized := uuid.NewSHA1(uuid.NameSpaceURL, []byte("doc-1")).String()
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/points/search") {
			w.WriteHeader(http.StatusOK)
			return
		}
		data, err := s.rdb.HGetAll(ctx, vecKey("t1", "doc-1")).Result()
		if err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"result": []map[string]interface{}{{
				"id":      normalized,
				"score":   0.9,
				"payload": map[string]interface{}{"source_id": "doc-1", "agent_id": "codex", "index_revision": semanticRevision(data)},
			}},
		})
	}))
	t.Cleanup(qdrant.Close)
	originalQdrant := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(qdrant.URL, 4)
	t.Cleanup(func() { defaultQdrantClient = originalQdrant })

	// One active record, keyed by the client's id, indexed for both legs.
	embedding := []float32{0.5, 0.5, 0.5, 0.5}
	if err := storeVec(ctx, s.rdb, "t1", storedVector{
		ID:        "doc-1",
		AgentID:   "codex",
		Embedding: embedding,
		Metadata:  json.RawMessage(`{}`),
		Text:      "lexical recall",
	}); err != nil {
		t.Fatal(err)
	}
	if err := updateBM25Index(ctx, s.rdb, "t1", "doc-1", "lexical recall"); err != nil {
		t.Fatal(err)
	}

	// Authenticate through the auth-cache fast path, so the test does not have
	// to mint a key and carry bcrypt into it.
	epoch := authEpoch(ctx, s.rdb, "t1")
	cached, err := json.Marshal(cachedAuth{KeyID: 1, TenantID: "t1", Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.Set(ctx, authCacheKey("test-key"), string(cached), 0).Err(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/search",
		strings.NewReader(`{"text":"lexical recall","limit":5}`))
	req.Header.Set(s.config.Auth.APIKeyHeader, "test-key")
	req.Header.Set(s.config.Auth.TenantHeader, "t1")
	rec := httptest.NewRecorder()

	s.handleSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}

	// Both legs found the same document, so there is one result — not one per
	// name the document goes by.
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1: the BM25 and Qdrant legs must fuse a "+
			"non-UUID document instead of returning it once per id (got %+v)",
			len(resp.Results), resp.Results)
	}
	got := resp.Results[0]
	if got.ID != "doc-1" {
		t.Errorf("result id = %q, want the client's id %q", got.ID, "doc-1")
	}
	if got.BM25Score == nil || got.QdrantScore == nil {
		t.Errorf("result was not fused from both legs: bm25=%v qdrant=%v",
			got.BM25Score, got.QdrantScore)
	}
	// The hydration read is the half that fails silently: with the rewritten id
	// the vecKey(...) lookup misses, and the handler keeps an unhydrated result
	// whenever no agent/tag filter is set.
	if got.Text != "lexical recall" {
		t.Errorf("result text = %q, want it hydrated from the active record", got.Text)
	}
	if got.AgentID != "codex" {
		t.Errorf("result agent_id = %q, want it hydrated from the active record", got.AgentID)
	}
}

// The other half of the same fix: the payload field is what makes the
// translation possible, so pin that UpsertPoint writes it — and that a UUID id,
// which needs no translation, is left byte-identical to what it was before.
func TestUpsertPointRecordsTheCallersIDWhenQdrantNeedsAUUID(t *testing.T) {
	ctx := context.Background()

	var bodies []map[string]interface{}
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upsert body: %v", err)
		}
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qdrant.Close)

	client := NewQdrantClient(qdrant.URL, 4)
	for _, id := range []string{"doc-1", "550e8400-e29b-41d4-a716-446655440000"} {
		if err := client.UpsertPoint(ctx, "t1", QdrantPoint{
			ID:      id,
			Vector:  []float32{1, 0, 0, 0},
			Payload: map[string]interface{}{"agent_id": "codex"},
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("captured %d upsert bodies, want 2", len(bodies))
	}

	point := func(i int) map[string]interface{} {
		points, _ := bodies[i]["points"].([]interface{})
		if len(points) != 1 {
			t.Fatalf("upsert %d carried %d points, want 1", i, len(points))
		}
		p, _ := points[0].(map[string]interface{})
		return p
	}

	nonUUID := point(0)
	payload, _ := nonUUID["payload"].(map[string]interface{})
	if payload["source_id"] != "doc-1" {
		t.Errorf("payload source_id = %v, want %q — without it the hit cannot be "+
			"translated back to the document /search can hydrate", payload["source_id"], "doc-1")
	}
	if payload["agent_id"] != "codex" {
		t.Errorf("payload agent_id = %v, want the caller's payload preserved", payload["agent_id"])
	}
	if id, _ := nonUUID["id"].(string); id == "doc-1" {
		t.Error("point id was not normalized; Qdrant requires a UUID")
	} else if _, err := uuid.Parse(id); err != nil {
		t.Errorf("point id %q is not a UUID: %v", id, err)
	}

	// A UUID point remains unchanged and also retains its source identity.
	uuidPoint := point(1)
	uuidPayload, _ := uuidPoint["payload"].(map[string]interface{})
	if uuidPayload["source_id"] != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("a UUID source identity was not preserved: %v", uuidPayload)
	}
	if id, _ := uuidPoint["id"].(string); id != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("UUID point id = %q, want it unchanged", id)
	}
}
