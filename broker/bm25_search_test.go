package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
)

func seedLexicalMemory(t *testing.T, rdb *redis.Client, id, text, agent string) {
	t.Helper()
	ctx := context.Background()
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: id, Text: text, AgentID: agent, Tags: []string{agent}, Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := updateBM25Index(ctx, rdb, "t1", id, text); err != nil {
		t.Fatal(err)
	}
}

func TestBM25LegacyBackfillPreservesExistingIndex(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	seedLexicalMemory(t, rdb, "legacy", "durable memory", "a")
	// Older imports may lack top-level text. Preserve their existing TF data.
	rdb.HDel(ctx, vecKey("t1", "legacy"), "text")
	rdb.HSet(ctx, vecKey("t1", "legacy"), "metadata", `{"text":"different legacy metadata"}`)
	rdb.Del(ctx, bm25PostingKey("t1", "durable"), bm25PostingKey("t1", "memory"))
	before, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"memory"}, 10)
	if err != nil || before["legacy"] <= 0 {
		t.Fatalf("legacy fallback lost recall: %v %v", before, err)
	}
	stats := rdb.MGet(ctx, bm25MetaDocCountKey("t1"), bm25MetaAvgDLKey("t1"), bm25TFKey("t1", "legacy", "memory")).Val()
	if _, _, err := backfillBM25Postings(ctx, rdb, "t1"); err != nil {
		t.Fatal(err)
	}
	ready, err := bm25PostingsReady(ctx, rdb, "t1")
	if err != nil || !ready {
		t.Fatal("completed backfill did not enable postings")
	}
	after, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"memory"}, 10)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("backfill changed lexical recall: %v -> %v (%v)", before, after, err)
	}
	if got := rdb.MGet(ctx, bm25MetaDocCountKey("t1"), bm25MetaAvgDLKey("t1"), bm25TFKey("t1", "legacy", "memory")).Val(); !reflect.DeepEqual(stats, got) {
		t.Fatalf("backfill rewrote statistics: %v -> %v", stats, got)
	}
}

func TestBM25ReplacementAndVaultRemoveObsoletePostings(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	seedLexicalMemory(t, rdb, "doc", "obsolete oldword", "a")
	seedLexicalMemory(t, rdb, "doc", "current newword", "a")
	// A slow request containing obsolete text cannot overwrite the new index.
	if err := updateBM25Index(ctx, rdb, "t1", "doc", "obsolete oldword"); err != nil {
		t.Fatal(err)
	}
	if rdb.ZCard(ctx, bm25PostingKey("t1", "obsolete")).Val() != 0 || rdb.ZCard(ctx, bm25PostingKey("t1", "current")).Val() != 1 {
		t.Fatal("replacement retained stale postings")
	}
	seedLexicalMemory(t, rdb, "doc", "!!!", "a")
	if rdb.ZCard(ctx, bm25PostingKey("t1", "current")).Val() != 0 || rdb.Get(ctx, bm25MetaDocCountKey("t1")).Val() != "0" {
		t.Fatal("token-free replacement retained old lexical index")
	}
	seedLexicalMemory(t, rdb, "doc", "vaultable memory", "a")
	if _, err := moveActiveToVault(ctx, rdb, "t1", "doc"); err != nil {
		t.Fatal(err)
	}
	if rdb.ZCard(ctx, bm25PostingKey("t1", "vaultable")).Val() != 0 {
		t.Fatal("vault retained lexical posting")
	}
	if rdb.ZCard(ctx, semanticPendingKey).Val() != 1 {
		t.Fatal("vault lost semantic removal outbox")
	}
	if err := markBM25PostingsComplete(ctx, rdb, "t1"); err != nil {
		t.Fatal(err)
	}
	hits, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"memory"}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("vaulted memory returned: %v %v", hits, err)
	}
}

type bm25CommandCounter struct {
	tf          atomic.Int64
	maxPipeline atomic.Int64
}

func (h *bm25CommandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *bm25CommandCounter) count(cmd redis.Cmder) {
	if cmd.Name() == "get" && len(cmd.Args()) > 1 && strings.Contains(fmt.Sprint(cmd.Args()[1]), ":bm25:tf:") {
		h.tf.Add(1)
	}
}
func (h *bm25CommandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.count(cmd); return next(ctx, cmd) }
}
func (h *bm25CommandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			h.count(cmd)
		}
		for old := h.maxPipeline.Load(); int64(len(cmds)) > old; old = h.maxPipeline.Load() {
			if h.maxPipeline.CompareAndSwap(old, int64(len(cmds))) {
				break
			}
		}
		return next(ctx, cmds)
	}
}

func TestBM25ReadyIndexSkipsUnrelatedCorpus(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	for i := 0; i < 300; i++ {
		seedLexicalMemory(t, rdb, fmt.Sprintf("irrelevant-%03d", i), "background material", "other")
	}
	seedLexicalMemory(t, rdb, "match", "uniqueneedle", "wanted")
	if _, _, err := backfillBM25Postings(ctx, rdb, "t1"); err != nil {
		t.Fatal(err)
	}
	commands := new(bm25CommandCounter)
	rdb.AddHook(commands)
	hits, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"uniqueneedle"}, 10)
	if err != nil || len(hits) != 1 || hits["match"] <= 0 {
		t.Fatalf("missing match: %v %v", hits, err)
	}
	if n := commands.tf.Load(); n != 1 {
		t.Fatalf("TF GETs = %d, want only the matching document (not all 301)", n)
	}
	t.Logf("301-document corpus: indexed search fetched %d term-frequency value", commands.tf.Load())
}

func TestBM25MultiTermMergeScoresEachDocumentOnce(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	const docs = 270 // more than two posting pages
	for i := 0; i < docs; i++ {
		seedLexicalMemory(t, rdb, fmt.Sprintf("doc-%03d", i), "shared alpha beta", "a")
	}
	if _, _, err := backfillBM25Postings(ctx, rdb, "t1"); err != nil {
		t.Fatal(err)
	}
	commands := new(bm25CommandCounter)
	rdb.AddHook(commands)
	hits, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"shared", "alpha", "beta", "alpha"}, 5)
	if err != nil || len(hits) != 5 {
		t.Fatalf("bounded results: %v %v", hits, err)
	}
	if n := commands.tf.Load(); n != docs*3 {
		t.Fatalf("TF GETs = %d, want %d after stream deduplication", n, docs*3)
	}
	if n := commands.maxPipeline.Load(); n > maxBM25ScoreDocs*4+5 {
		t.Fatalf("unbounded pipeline: %d commands", n)
	}
	for i := 0; i < 5; i++ {
		if hits[fmt.Sprintf("doc-%03d", i)] <= 0 {
			t.Fatal("non-deterministic top-k tie handling")
		}
	}
}

func TestBM25FiltersBeforeRetentionAndRejectsZeroScores(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	for i := 0; i < 140; i++ {
		seedLexicalMemory(t, rdb, fmt.Sprintf("a-%03d", i), "memory", "other")
	}
	seedLexicalMemory(t, rdb, "z-wanted", "memory extra explanation", "wanted")
	seedLexicalMemory(t, rdb, "zero", "unrelated words", "wanted")
	for _, ready := range []bool{false, true} {
		if ready {
			if _, _, err := backfillBM25Postings(ctx, rdb, "t1"); err != nil {
				t.Fatal(err)
			}
		}
		hits, err := searchBM25Candidates(ctx, rdb, "t1", "wanted", []string{"wanted"}, []string{"memory"}, 1)
		if err != nil || len(hits) != 1 || hits["z-wanted"] <= 0 {
			t.Fatalf("ready=%v filtered match lost: %v %v", ready, hits, err)
		}
		none, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"missingterm"}, 10)
		if err != nil || len(none) != 0 {
			t.Fatalf("zero-score results retained: %v %v", none, err)
		}
	}
}

func TestBM25IncompleteBackfillNeverMarksReady(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	seedLexicalMemory(t, rdb, "good", "memory", "a")
	rdb.SAdd(ctx, vecSetKey("t1"), "missing-hash")
	if _, _, err := backfillBM25Postings(ctx, rdb, "t1"); err == nil {
		t.Fatal("missing hash marked complete")
	}
	if ready, _ := bm25PostingsReady(ctx, rdb, "t1"); ready {
		t.Fatal("incomplete postings used for search")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := backfillBM25Postings(cancelled, rdb, "t1"); err == nil {
		t.Fatal("cancelled backfill succeeded")
	}
	if ready, _ := bm25PostingsReady(ctx, rdb, "t1"); ready {
		t.Fatal("cancelled backfill marked ready")
	}
}

func TestBM25ConcurrentUpdatesKeepStatistics(t *testing.T) {
	ctx := context.Background()
	rdb := newTestRedis(t)
	const docs = 20
	var wg sync.WaitGroup
	errs := make(chan error, docs)
	for i := 0; i < docs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- updateBM25Index(ctx, rdb, "t1", fmt.Sprintf("doc-%d", i), "shared memory")
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := rdb.Get(ctx, bm25MetaDocCountKey("t1")).Val(); n != "20" {
		t.Fatalf("lost concurrent document counts: %s", n)
	}
	if n := rdb.Get(ctx, bm25DFKey("t1", "shared")).Val(); n != "20" {
		t.Fatalf("lost concurrent document frequencies: %s", n)
	}
}
