package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestVaultingRemovesBM25Postings(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	metadata, _ := json.Marshal(map[string]string{"text": "rehydration preserves lexical recall"})
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc-1", Embedding: []float32{1, 0}, Metadata: metadata, Text: "rehydration preserves lexical recall"}); err != nil {
		t.Fatal(err)
	}
	if err := updateBM25Index(ctx, rdb, "t1", "doc-1", "rehydration preserves lexical recall"); err != nil {
		t.Fatal(err)
	}
	if err := vaultDoc(ctx, rdb, "t1", "doc-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := rdb.Get(ctx, bm25DocLenKey("t1", "doc-1")).Result(); got != "" {
		t.Fatalf("vaulted document retained BM25 length %q", got)
	}
	if got, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); got != "0" {
		t.Fatalf("BM25 document count = %q, want 0 after vaulting", got)
	}
}

func TestRehydrateRestoresEveryActiveProjection(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qdrant.Close)
	originalQdrant := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(qdrant.URL, 2)
	t.Cleanup(func() { defaultQdrantClient = originalQdrant })
	metadata, _ := json.Marshal(map[string]string{"text": "important recalled memory"})
	vaultKey := "tenant:t1:vault:doc-1"
	if err := rdb.HSet(ctx, vaultKey, map[string]interface{}{
		"embedding":    `[1,0]`,
		"metadata":     string(metadata),
		"created_at":   "2026-01-01T00:00:00Z",
		"access_count": "4",
		"agent_id":     "codex",
		"namespace":    "notes",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	if err := rehydrateVaultDoc(ctx, rdb, "t1", "doc-1"); err != nil {
		t.Fatalf("rehydration failed: %v", err)
	}
	if exists, _ := rdb.Exists(ctx, vecKey("t1", "doc-1")).Result(); exists != 1 {
		t.Fatal("active Redis record was not restored")
	}
	if member, _ := rdb.SIsMember(ctx, vecSetKey("t1"), "doc-1").Result(); !member {
		t.Fatal("rehydrated record is absent from active set")
	}
	if exists, _ := rdb.Exists(ctx, vaultKey).Result(); exists != 0 {
		t.Fatal("vault source remained after every active index projection succeeded")
	}
	if got, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); got != "1" {
		t.Fatalf("BM25 document count = %q, want 1 after rehydrate", got)
	}
}

func TestRehydrateRejectsInvalidEmbeddingWithoutMutatingVault(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	vaultKey := "tenant:t1:vault:bad-doc"
	if err := rdb.HSet(ctx, vaultKey, "embedding", "not-json").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rehydrateVaultDoc(ctx, rdb, "t1", "bad-doc"); err == nil {
		t.Fatal("invalid embedding was accepted")
	}
	if exists, _ := rdb.Exists(ctx, vaultKey).Result(); exists != 1 {
		t.Fatal("invalid vault record was mutated")
	}
}

func TestRehydrateRejectsNewerActiveVersion(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := rdb.HSet(ctx, "tenant:t1:vault:doc-1", map[string]interface{}{
		"embedding":  `[1,0]`,
		"created_at": "2026-01-01T00:00:00Z",
	}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HSet(ctx, vecKey("t1", "doc-1"), "created_at", "2026-02-01T00:00:00Z").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rehydrateVaultDoc(ctx, rdb, "t1", "doc-1"); err != errVaultRehydrateConflict {
		t.Fatalf("rehydration error = %v, want conflict", err)
	}
}

func TestStoreSupersedesVaultCopy(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	vaultKey := "tenant:t1:vault:doc-1"
	if err := rdb.HSet(ctx, vaultKey, "text", "stale").Err(); err != nil {
		t.Fatal(err)
	}
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "doc-1", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if exists, _ := rdb.Exists(ctx, vaultKey).Result(); exists != 0 {
		t.Fatal("fresh store left a stale vault copy")
	}
}

// A rehydration can pass the Lua guard and then lose a race to a concurrent
// /store. Before the repair path existed, that left BM25 and Qdrant holding
// the OLD text and vector under a doc id whose active record was already the
// NEW one — stale search results, produced by the recovery tool itself.
//
// The conflict must still be reported, but the indexes must describe whatever
// is actually active afterwards.
func TestRehydrateConflictRepairsIndexesToSupersedingRecord(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)

	var upserted int
	var upsertVectors [][]float32
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "/points") {
			upserted++
			// Keep the vectors, not just the count. A count alone is satisfied
			// by a repair that re-upserts the vaulted vector, which is the
			// precise regression this test exists to catch — the count says
			// "something was written", only the body says "the right thing".
			var body struct {
				Points []QdrantPoint `json:"points"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upsert body: %v", err)
			}
			for _, p := range body.Points {
				upsertVectors = append(upsertVectors, p.Vector)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qdrant.Close)
	originalQdrant := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(qdrant.URL, 2)
	t.Cleanup(func() { defaultQdrantClient = originalQdrant })

	// Vault copy carries the old text; nothing is active yet, so the Lua
	// guard admits the restore.
	oldMeta, _ := json.Marshal(map[string]string{"text": "obsolete vaulted wording"})
	if err := rdb.HSet(ctx, "tenant:t1:vault:doc-1", map[string]interface{}{
		"embedding":  `[1,0]`,
		"metadata":   string(oldMeta),
		"created_at": "2026-01-01T00:00:00Z",
		"agent_id":   "codex",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	// Simulate the concurrent /store landing after the restore has projected
	// the vaulted record but before the post-write verification reads Redis.
	newMeta, _ := json.Marshal(map[string]string{"text": "current superseding wording"})
	err := rehydrateVaultDocWithAfterProjection(ctx, rdb, "t1", "doc-1", func() error {
		return rdb.HSet(ctx, vecKey("t1", "doc-1"), map[string]interface{}{
			"embedding":  `[0,1]`,
			"metadata":   string(newMeta),
			"created_at": "2026-06-01T00:00:00Z",
			"agent_id":   "codex",
		}).Err()
	})
	if !errors.Is(err, errVaultRehydrateConflict) {
		t.Fatalf("rehydration error = %v, want conflict", err)
	}

	// The newer record must survive untouched.
	got, _ := rdb.HGet(ctx, vecKey("t1", "doc-1"), "created_at").Result()
	if got != "2026-06-01T00:00:00Z" {
		t.Fatalf("active created_at = %q, want the superseding record preserved", got)
	}
	// And the vault copy must remain, so the operator can retry deliberately.
	if exists, _ := rdb.Exists(ctx, "tenant:t1:vault:doc-1").Result(); exists != 1 {
		t.Fatal("vault copy was removed despite the conflict")
	}
	terms, err := rdb.SMembers(ctx, bm25DocTermsKey("t1", "doc-1")).Result()
	if err != nil {
		t.Fatal(err)
	}
	foundCurrent := false
	for _, term := range terms {
		if term == "current" {
			foundCurrent = true
		}
		if term == "obsolete" || term == "vaulted" {
			t.Fatalf("BM25 terms still contain stale term %q after repair: %v", term, terms)
		}
	}
	if !foundCurrent {
		t.Fatalf("BM25 terms do not contain the superseding record after repair: %v", terms)
	}
	if upserted != 2 {
		t.Fatalf("Qdrant upserts = %d, want vaulted projection plus active-record repair", upserted)
	}
	// One upsert carries the vaulted vector and one the superseding vector. If
	// the repair re-projected the vaulted embedding instead of the active
	// record's, the superseding vector would be missing here and vector search
	// would keep returning the superseded content.
	var vaulted, superseding int
	for _, v := range upsertVectors {
		switch {
		case reflect.DeepEqual(v, []float32{1, 0}):
			vaulted++
		case reflect.DeepEqual(v, []float32{0, 1}):
			superseding++
		}
	}
	if vaulted != 1 || superseding != 1 {
		t.Fatalf("upsert vectors = %v, want one vaulted [1 0] and one superseding [0 1]: "+
			"the repair must project the superseding record's vector", upsertVectors)
	}
}

// The repair itself, exercised directly: given an active record, BM25 and
// Qdrant are rebuilt from it rather than from whatever was written last.
func TestReindexActiveDocProjectsCurrentRecord(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	if err := rdb.SAdd(ctx, vecSetKey("t1"), "doc-1").Err(); err != nil {
		t.Fatal(err)
	}

	var upserts int
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/points") {
			upserts++
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qdrant.Close)
	originalQdrant := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(qdrant.URL, 2)
	t.Cleanup(func() { defaultQdrantClient = originalQdrant })

	meta, _ := json.Marshal(map[string]string{"text": "current superseding wording"})
	if err := rdb.HSet(ctx, vecKey("t1", "doc-1"), map[string]interface{}{
		"embedding":  `[0,1]`,
		"metadata":   string(meta),
		"created_at": "2026-06-01T00:00:00Z",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	if err := reindexActiveDoc(ctx, rdb, "t1", "doc-1"); err != nil {
		t.Fatalf("reindex failed: %v", err)
	}
	if count, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); count != "1" {
		t.Fatalf("BM25 doc count = %q, want 1 after reindex", count)
	}
	if upserts != 1 {
		t.Fatalf("qdrant upserts = %d, want 1", upserts)
	}

	// A document that has since been deleted must not error or resurrect.
	if err := rdb.Del(ctx, vecKey("t1", "doc-1")).Err(); err != nil {
		t.Fatal(err)
	}
	// Reindex the id whose hash was actually just deleted. Naming a different
	// id ("doc-2") proved nothing: it was never present either way, so the call
	// returned nil for an empty hash regardless of whether the deleted id
	// behaved correctly — a test that passes for the wrong reason.
	if err := reindexActiveDoc(ctx, rdb, "t1", "doc-1"); err != nil {
		t.Fatalf("reindex of absent doc should be a no-op, got %v", err)
	}
	// No-op means no projections were touched: the deleted doc must not be
	// re-upserted, and the BM25 index must not have been rewritten.
	if upserts != 1 {
		t.Errorf("qdrant upserts = %d after reindexing a deleted doc, want 1 (unchanged)", upserts)
	}
	if count, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); count != "1" {
		t.Errorf("BM25 doc count = %q after reindexing a deleted doc, want 1 (unchanged)", count)
	}
	if exists, _ := rdb.Exists(ctx, vecKey("t1", "doc-1")).Result(); exists != 0 {
		t.Error("reindexing an absent doc resurrected its Redis hash")
	}
}

// A repair that exists to fix a race must not itself race. The window is
// between the read and the projections, so the test writes *inside* that window
// via the injected projection step: the first projection is followed by a
// concurrent /store, and the guard must notice and project the newer contents.
func TestReindexRetriesWhenAWriteLandsMidProjection(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	key := vecKey("t1", "doc-1")

	if err := rdb.HSet(ctx, key, map[string]interface{}{
		"text":       "first wording",
		"embedding":  `[1,0]`,
		"metadata":   `{}`,
		"created_at": "2026-06-01T00:00:00Z",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	var projected []string
	project := func(data map[string]string) error {
		projected = append(projected, data["text"])
		if len(projected) == 1 {
			// The concurrent write that the guard is meant to survive.
			return rdb.HSet(ctx, key, "text", "second wording").Err()
		}
		return nil
	}

	if err := reindexActiveDocWith(ctx, rdb, "t1", "doc-1", project); err != nil {
		t.Fatalf("reindex failed: %v", err)
	}
	if len(projected) != 2 {
		t.Fatalf("projection ran %d times, want 2 (retry after the concurrent write)", len(projected))
	}
	if projected[0] != "first wording" || projected[1] != "second wording" {
		t.Fatalf("projected %q, want [first wording, second wording]", projected)
	}
}

// A document under continuous writes cannot be repaired by read-then-compare,
// so the loop is bounded and reports the failure instead of spinning on the
// recovery path.
func TestReindexGivesUpAfterBoundedAttempts(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	key := vecKey("t1", "doc-1")

	if err := rdb.HSet(ctx, key, map[string]interface{}{
		"text":       "wording 0",
		"embedding":  `[1,0]`,
		"metadata":   `{}`,
		"created_at": "2026-06-01T00:00:00Z",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	calls := 0
	project := func(map[string]string) error {
		calls++
		// Every attempt is invalidated, so the fingerprint never settles.
		return rdb.HSet(ctx, key, "text", fmt.Sprintf("wording %d", calls)).Err()
	}

	err := reindexActiveDocWith(ctx, rdb, "t1", "doc-1", project)
	if err == nil {
		t.Fatal("expected an error when the active record never stops changing")
	}
	if calls != 3 {
		t.Fatalf("projection ran %d times, want 3 (bounded)", calls)
	}
}
