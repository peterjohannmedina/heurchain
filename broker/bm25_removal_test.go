package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /store does not require text, so a document that was indexed with a body can
// be rewritten without one. The index has to follow it down: the old body's
// terms must stop matching, and the corpus totals must stop counting a document
// that can no longer be scored. The empty body used to return early, leaving
// every one of those behind.
func TestUpdateBM25IndexWithdrawsPostingsForAnEmptyBody(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)

	if err := updateBM25Index(ctx, rdb, "t1", "doc-1", "lexical recall"); err != nil {
		t.Fatal(err)
	}
	if count, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); count != "1" {
		t.Fatalf("BM25 doc count = %q after the first store, want 1", count)
	}

	if err := updateBM25Index(ctx, rdb, "t1", "doc-1", ""); err != nil {
		t.Fatal(err)
	}

	if terms, _ := rdb.SMembers(ctx, bm25DocTermsKey("t1", "doc-1")).Result(); len(terms) != 0 {
		t.Fatalf("BM25 term set survived an empty-body store: %v", terms)
	}
	if exists, _ := rdb.Exists(ctx, bm25TFKey("t1", "doc-1", "lexical")).Result(); exists != 0 {
		t.Error("BM25 term-frequency key survived an empty-body store")
	}
	if df, _ := rdb.Get(ctx, bm25DFKey("t1", "lexical")).Result(); df != "" && df != "0" {
		t.Errorf("BM25 document frequency = %q, want 0 once the only document holding the term left", df)
	}
	// The length key must be gone, not stored as "0": the read above treats a
	// zero length as "this document was never indexed", so a stored 0 would
	// count the document a second time on its next store.
	if dl, _ := rdb.Get(ctx, bm25DocLenKey("t1", "doc-1")).Result(); dl != "" {
		t.Fatalf("BM25 length key = %q, want it absent", dl)
	}
	if count, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); count != "0" {
		t.Fatalf("BM25 doc count = %q once the body was withdrawn, want 0", count)
	}
	if avgdl, _ := rdb.Get(ctx, bm25MetaAvgDLKey("t1")).Result(); avgdl != "0" {
		t.Fatalf("BM25 average document length = %q with no documents left, want 0", avgdl)
	}

	// And the document can come back. Storing a body again must count it once,
	// which is what the length key being absent — rather than zero — buys.
	if err := updateBM25Index(ctx, rdb, "t1", "doc-1", "lexical recall"); err != nil {
		t.Fatal(err)
	}
	if count, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); count != "1" {
		t.Fatalf("BM25 doc count = %q after re-storing the body, want 1 (counted twice?)", count)
	}
	if df, _ := rdb.Get(ctx, bm25DFKey("t1", "lexical")).Result(); df != "1" {
		t.Fatalf("BM25 document frequency = %q after re-storing the body, want 1", df)
	}
}

// The race the repair path exists for can also be lost to an embedding-only
// write: /store accepts a record with no text. The repair re-projects from
// whatever is active, and when that record holds no body the postings have to
// come out — otherwise lexical search keeps returning a document whose only
// remaining content is a vector.
func TestReindexWithdrawsPostingsWhenSupersedingRecordHasNoText(t *testing.T) {
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

	// The superseded body is in the index; the active record has already been
	// replaced by an embedding-only write.
	if err := rdb.SAdd(ctx, vecSetKey("t1"), "doc-1").Err(); err != nil {
		t.Fatal(err)
	}
	if err := updateBM25Index(ctx, rdb, "t1", "doc-1", "obsolete vaulted wording"); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HSet(ctx, vecKey("t1", "doc-1"), map[string]interface{}{
		"embedding":  `[0,1]`,
		"metadata":   `{}`,
		"created_at": "2026-06-01T00:00:00Z",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	if err := reindexActiveDoc(ctx, rdb, "t1", "doc-1"); err != nil {
		t.Fatalf("reindex failed: %v", err)
	}

	terms, err := rdb.SMembers(ctx, bm25DocTermsKey("t1", "doc-1")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) != 0 {
		t.Fatalf("postings from the superseded body survived the repair: %v", terms)
	}
	if exists, _ := rdb.Exists(ctx, bm25TFKey("t1", "doc-1", "obsolete")).Result(); exists != 0 {
		t.Error("term-frequency key for the superseded body survived the repair")
	}
	if count, _ := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Result(); count != "0" {
		t.Fatalf("BM25 doc count = %q, want 0: the active record holds no scorable terms", count)
	}
}
