package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Rollback rehearsal phase 3 (plan 6.12) found this gap: forget removed a
// memory, a tombstone-unaware broker (0.4.4) or a pre-forget Redis restore
// then put the content back next to the tombstone, and after re-upgrading
// /search and /query returned it again. Writes already refuse the ID; these
// tests pin that every read path also honours the tombstone on its own.

// seedForgetTombstone writes the value forget's Lua stores, with no TTL.
func seedForgetTombstone(t *testing.T, f *recallFixture, id string) {
	t.Helper()
	value, _ := json.Marshal(map[string]interface{}{"id": id, "revision": 2, "forgotten_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err := f.s.rdb.Set(context.Background(), memoryTombstoneKey("t1", id), value, 0).Err(); err != nil {
		t.Fatal(err)
	}
}

func tombstoneTestPointID(id string) string {
	ensureUUID(&id)
	return id
}

func TestReadsHideResurrectedForgottenMemory(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	const phrase = "orbital docking checklist"
	f.store(t, "keep", phrase+" kept", "codex", []string{"procedure"}, 0.9)
	f.store(t, "gone-residue", phrase+" forgotten then resurrected", "codex", []string{"procedure"}, 1)

	// The rehearsal state: the active hash, active-set membership, BM25
	// postings and the Qdrant point all exist again, next to the tombstone.
	seedForgetTombstone(t, f, "gone-residue")
	if member, _ := f.s.rdb.SIsMember(ctx, vecSetKey("t1"), "gone-residue").Result(); !member {
		t.Fatal("fixture: expected the resurrected record in the active set")
	}
	if n, _ := f.s.rdb.Exists(ctx, bm25DocLenKey("t1", "gone-residue")).Result(); n == 0 {
		t.Fatal("fixture: expected resurrected BM25 postings")
	}
	f.mu.Lock()
	_, point := f.points[tombstoneTestPointID("gone-residue")]
	f.mu.Unlock()
	if !point {
		t.Fatal("fixture: expected a resurrected Qdrant point")
	}

	onlyKeep := func(surface string, ids []string) {
		t.Helper()
		if len(ids) != 1 || ids[0] != "keep" {
			t.Fatalf("%s returned %v, want only [keep]", surface, ids)
		}
	}

	for _, mode := range []string{"hybrid", "lexical", "dense"} {
		hits, _ := f.search(t, map[string]interface{}{"text": phrase, "retrieval_mode": mode})
		ids := []string{}
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		onlyKeep("/search retrieval_mode="+mode, ids)
	}

	session := &mcpSession{apiKey: "test-key", tenantID: "t1"}
	for _, tool := range []string{"heurchain_search", "heurchain_query"} {
		data, err := f.s.invokeMCPTool(ctx, session, tool, json.RawMessage(`{"text":"`+phrase+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		var reply struct{ Results []searchResult }
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("%s reply: %s", tool, data)
		}
		ids := []string{}
		for _, r := range reply.Results {
			ids = append(ids, r.ID)
		}
		onlyKeep("MCP "+tool, ids)
	}

	// /query's brute-force scan, called directly so no recall leg can mask it.
	scanned, err := searchVecs(ctx, f.s.rdb, "t1", "", nil, []float32{1, 1}, 10, -1, true)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, r := range scanned {
		ids = append(ids, r.ID)
	}
	onlyKeep("searchVecs", ids)

	data, err := f.s.invokeMCPTool(ctx, session, "heurchain_manifest", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Total  int
		Recent []manifestRecentItem
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	ids = ids[:0]
	for _, r := range manifest.Recent {
		ids = append(ids, r.ID)
	}
	onlyKeep("manifest recent", ids)
	if manifest.Total != 1 {
		t.Fatalf("manifest total = %d, want 1", manifest.Total)
	}

	items, err := recentAgentMemories(ctx, f.s.rdb, "t1", "codex", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids = ids[:0]
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	onlyKeep("/agent/<id>", ids)

	// A resurrected source is neither extracted nor citable as evidence.
	if _, eligible, err := readGraphSource(ctx, f.s.rdb, "t1", "gone-residue"); err != nil || eligible {
		t.Fatalf("readGraphSource(gone-residue) eligible=%v err=%v, want ineligible", eligible, err)
	}
	if err := f.s.validateOntologyEvidence(ctx, "t1", "", &ontologyEnvelope{SourceMemoryID: "gone-residue"}); err == nil {
		t.Fatal("a forgotten memory was accepted as ontology evidence")
	}
	if err := f.s.validateOntologyEvidence(ctx, "t1", "", &ontologyEnvelope{SourceMemoryID: "keep"}); err != nil {
		t.Fatalf("an active memory was refused as ontology evidence: %v", err)
	}

	// Vault residue is not exported or counted either.
	if err := f.s.rdb.HSet(ctx, memoryVaultKey("t1", "gone-residue"), map[string]interface{}{
		"text": "vault residue", "embedding": "[1,1]", "metadata": "{}",
	}).Err(); err != nil {
		t.Fatal(err)
	}
	files := exportVault(t, f.s, "t1")
	if len(files) != 1 || files["keep.md"] == "" {
		names := []string{}
		for name := range files {
			names = append(names, name)
		}
		t.Fatalf("/vault/export entries = %v, want only keep.md", names)
	}
	for _, md := range files {
		if strings.Contains(md, "resurrected") || strings.Contains(md, "residue") {
			t.Fatalf("export leaked forgotten content:\n%s", md)
		}
	}
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASS", "hunter2")
	req := httptest.NewRequest(http.MethodGet, "/vault/stats?tenant_id=t1", nil)
	req.SetBasicAuth("admin", "hunter2")
	w := httptest.NewRecorder()
	f.s.handleVaultStats(w, req)
	var stats struct {
		VectorsTotal int `json:"vectors_total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil || stats.VectorsTotal != 1 {
		t.Fatalf("/vault/stats = %s, want vectors_total 1", w.Body.String())
	}

	// Reindexing a resurrected record deletes its Qdrant point instead of
	// projecting it again; the kept record's point is untouched.
	if err := reindexActiveDoc(ctx, f.s.rdb, "t1", "gone-residue"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, reprojected := f.points[tombstoneTestPointID("gone-residue")]
	_, kept := f.points[tombstoneTestPointID("keep")]
	f.mu.Unlock()
	if reprojected || !kept {
		t.Fatalf("after reindexActiveDoc: forgotten point present=%v, kept point present=%v", reprojected, kept)
	}

	// The write side still refuses the ID (410), as before this change.
	if w := f.call(t, "/store", map[string]interface{}{"id": "gone-residue", "text": "again", "agent_id": "codex", "embedding": []float32{1, 1}}); w.Code != http.StatusGone {
		t.Fatalf("store over tombstone = %d %s, want 410", w.Code, w.Body.String())
	}
}

// The background semantic worker deletes a resurrected record's Qdrant point
// and clears its work item, instead of upserting the point and acknowledging.
func TestSemanticWorkerDeletesResurrectedForgottenPoint(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	f.store(t, "keep", "orbital docking kept", "codex", []string{"procedure"}, 0.9)
	f.store(t, "gone-residue", "orbital docking resurrected", "codex", []string{"procedure"}, 1)
	seedForgetTombstone(t, f, "gone-residue")
	member := semanticMember("t1", "gone-residue")
	if err := f.s.rdb.ZAdd(ctx, semanticPendingKey, redis.Z{Score: 0, Member: member}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := drainSemanticIndex(ctx, f.s.rdb, defaultQdrantClient); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, gone := f.points[tombstoneTestPointID("gone-residue")]
	_, kept := f.points[tombstoneTestPointID("keep")]
	f.mu.Unlock()
	if gone || !kept {
		t.Fatalf("after drain: forgotten point present=%v, kept point present=%v", gone, kept)
	}
	if _, err := f.s.rdb.ZScore(ctx, semanticPendingKey, member).Result(); err != redis.Nil {
		t.Fatalf("forgotten work item still pending (err=%v)", err)
	}
}

// A Redis error on the tombstone read hides the record rather than leaking it.
func TestTombstoneHiddenFailsClosed(t *testing.T) {
	f := newRecallFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !tombstoneHidden(f.s.rdb.Exists(ctx, memoryTombstoneKey("t1", "x"))) {
		t.Fatal("tombstoneHidden returned false on a failed EXISTS")
	}
	if tombstoneHidden(f.s.rdb.Exists(context.Background(), memoryTombstoneKey("t1", "x"))) {
		t.Fatal("tombstoneHidden hid an ID with no tombstone")
	}
}
