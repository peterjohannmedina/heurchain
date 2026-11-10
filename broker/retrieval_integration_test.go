package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestSemanticOutboxRetriesWithoutBypassingMemoryLifecycle(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	f.fail.Store(true)
	request := map[string]interface{}{"id": "retry-life", "text": "original text", "embedding": []float32{1, 0}, "expected_revision": 0}
	w := f.call(t, "/store", request)
	receipt := lifecycleJSON(t, w)
	if w.Code != 202 || receipt["revision"] != float64(1) || receipt["indexed"] != false {
		t.Fatalf("lost partial-write receipt: %d %v", w.Code, receipt)
	}
	if f.s.rdb.ZCard(ctx, semanticPendingKey).Val() != 1 {
		t.Fatal("index outage did not retain durable work")
	}
	request["text"] = "stale replacement"
	if w := f.call(t, "/store", request); w.Code != 409 {
		t.Fatalf("outbox store bypassed CAS: %d %s", w.Code, w.Body)
	}
	f.fail.Store(false)
	if err := drainSemanticIndex(ctx, f.s.rdb, defaultQdrantClient); err != nil {
		t.Fatal(err)
	}
	if f.s.rdb.ZCard(ctx, semanticPendingKey).Val() != 0 {
		t.Fatal("successful retry did not acknowledge work")
	}
	hits, diag := f.search(t, map[string]interface{}{"text": "paraphrase"})
	if len(hits) != 1 || hits[0].Text != "original text" || diag.DenseStatus != "ok" {
		t.Fatalf("retry failed recall: %+v %+v", hits, diag)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryUpdate, map[string]interface{}{"id": "retry-life", "expected_revision": 1, "text": "corrected phrase", "reason": "reviewed correction"})
	if w.Code != 200 || lifecycleJSON(t, w)["revision"] != float64(2) {
		t.Fatalf("correction receipt lost: %d %s", w.Code, w.Body)
	}
	if f.s.rdb.LLen(ctx, memoryVersionsKey("t1", "retry-life")).Val() != 1 {
		t.Fatal("indexing lost revision history")
	}
	f.fail.Store(true)
	w = lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "retry-life", "expected_revision": 2, "confirm": true})
	if w.Code != 202 || lifecycleJSON(t, w)["cleanup_complete"] != false {
		t.Fatalf("forget outage hidden: %d %s", w.Code, w.Body)
	}
	if f.s.rdb.ZCard(ctx, bm25PostingKey("t1", "corrected")).Val() != 0 || f.s.rdb.LLen(ctx, memoryVersionsKey("t1", "retry-life")).Val() != 0 {
		t.Fatal("forget retained postings or revision content")
	}
	// An older projection that arrives after forget must not recreate postings.
	if err := updateBM25Index(ctx, f.s.rdb, "t1", "retry-life", "corrected phrase"); err != nil {
		t.Fatal(err)
	}
	if f.s.rdb.ZCard(ctx, bm25PostingKey("t1", "corrected")).Val() != 0 {
		t.Fatal("late projection recreated forgotten postings")
	}
	if err := storeVec(ctx, f.s.rdb, "t1", storedVector{ID: "retry-life", Text: "resurrect", Embedding: []float32{1, 0}}); !errors.Is(err, errMemoryForgotten) {
		t.Fatalf("outbox store bypassed tombstone: %v", err)
	}
	f.fail.Store(false)
	if err := drainSemanticIndex(ctx, f.s.rdb, defaultQdrantClient); err != nil {
		t.Fatal(err)
	}
	if f.s.rdb.ZCard(ctx, semanticPendingKey).Val() != 0 {
		t.Fatal("forget cleanup was not retried")
	}
	hits, _ = f.search(t, map[string]interface{}{"text": "phrase"})
	if len(hits) != 0 {
		t.Fatalf("forgotten source returned: %+v", hits)
	}
}

func TestPostingCandidatesApplyStructuredFiltersBeforeRetention(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	for i := 0; i < 205; i++ {
		metadata := `{"project":"other"}`
		if i == 204 {
			metadata = `{"project":"wanted"}`
		}
		id := fmt.Sprintf("doc-%03d", i)
		if err := storeVec(ctx, rdb, "t1", storedVector{ID: id, Text: "needle", Metadata: []byte(metadata), Embedding: []float32{1, 0}}); err != nil {
			t.Fatal(err)
		}
		if err := updateBM25Index(ctx, rdb, "t1", id, "needle"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := backfillBM25Postings(ctx, rdb, "t1"); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, memoryFilterContextKey{}, &memoryFilters{Metadata: map[string]interface{}{"project": "wanted"}})
	scores, err := searchBM25Candidates(ctx, rdb, "t1", "", nil, []string{"needle"}, 1)
	if err != nil || len(scores) != 1 || scores["doc-204"] <= 0 {
		t.Fatalf("structured filter applied after retention: %v %v", scores, err)
	}
}

func TestSemanticRetryKeepsCheckpointImmutable(t *testing.T) {
	f := newRecallFixture(t)
	f.fail.Store(true)
	w := lifecycleCall(t, f, f.s.handleMemoryCheckpoint, map[string]interface{}{"id": "retry-checkpoint", "agent_id": "codex", "summary": "reviewed handoff"})
	if w.Code != 202 {
		t.Fatalf("checkpoint outage receipt: %d %s", w.Code, w.Body)
	}
	f.fail.Store(false)
	if err := drainSemanticIndex(context.Background(), f.s.rdb, defaultQdrantClient); err != nil {
		t.Fatal(err)
	}
	w = f.call(t, "/store", map[string]interface{}{"id": "retry-checkpoint", "text": "replace snapshot", "embedding": []float32{1, 0}})
	if w.Code != 409 {
		t.Fatalf("retry discarded checkpoint guard: %d %s", w.Code, w.Body)
	}
}

func TestReindexRepairsLexicalStateBeforeRejectingCorruptVector(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	seedLexicalMemory(t, rdb, "doc", "obsolete phrase", "codex")
	if err := rdb.HSet(ctx, vecKey("t1", "doc"), "text", "", "embedding", "broken").Err(); err != nil {
		t.Fatal(err)
	}
	if err := reindexActiveDoc(ctx, rdb, "t1", "doc"); err == nil {
		t.Fatal("corrupt dense projection acknowledged")
	}
	if rdb.ZCard(ctx, bm25PostingKey("t1", "obsolete")).Val() != 0 || rdb.Exists(ctx, bm25DocTermsKey("t1", "doc")).Val() != 0 {
		t.Fatal("invalid vector prevented removal of obsolete lexical projection")
	}
	if rdb.ZCard(ctx, semanticPendingKey).Val() != 1 {
		t.Fatal("corrupt dense projection lost retry work")
	}
}

func TestSemanticWorkerSerializesProjectionWithForget(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "worker-forget", "obsolete phrase", "codex", nil, 1)
	ctx := context.Background()
	if err := f.s.rdb.ZAdd(ctx, semanticPendingKey, redis.Z{Member: semanticMember("t1", "worker-forget"), Score: 0}).Err(); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	q := *defaultQdrantClient
	q.httpClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	drained := make(chan error, 1)
	go func() { drained <- drainSemanticIndex(ctx, f.s.rdb, &q) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach blocked projection")
	}
	forgetStarted := make(chan struct{})
	forgotten := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		close(forgetStarted)
		forgotten <- lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "worker-forget", "expected_revision": 1, "confirm": true})
	}()
	<-forgetStarted
	select {
	case w := <-forgotten:
		close(release)
		<-drained
		t.Fatalf("forget completed while worker projection was in flight: %d %s", w.Code, w.Body)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	w := <-forgotten
	if w.Code != 200 || lifecycleJSON(t, w)["cleanup_complete"] != true {
		t.Fatalf("forget cleanup failed: %d %s", w.Code, w.Body)
	}
	if f.s.rdb.Exists(ctx, bm25TFKey("t1", "worker-forget", "obsolete")).Val() != 0 || f.s.rdb.ZCard(ctx, bm25PostingKey("t1", "obsolete")).Val() != 0 {
		t.Fatal("worker/forget race retained lexical content")
	}
	f.mu.Lock()
	pointID := "worker-forget"
	ensureUUID(&pointID)
	_, exists := f.points[pointID]
	f.mu.Unlock()
	if exists {
		t.Fatal("worker/forget race retained dense content")
	}
}

type afterBM25PageReadHook struct{ after func() error }

func (h *afterBM25PageReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *afterBM25PageReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}
func (h *afterBM25PageReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if err != nil && err != redis.Nil {
			return err
		}
		if h.after != nil {
			after := h.after
			h.after = nil
			if hookErr := after(); hookErr != nil {
				return hookErr
			}
		}
		return err
	}
}

func TestBackfillCannotRecreatePostingAfterConcurrentForget(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	if err := storeVec(ctx, rdb, "t1", storedVector{ID: "backfill-forget", Text: "obsolete phrase", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	// The backfill reads an active record with no existing lexical projection.
	// Commit forget after that pipeline, before the projection's Lua script.
	other := redis.NewClient(rdb.Options())
	t.Cleanup(func() { _ = other.Close() })
	hook := &afterBM25PageReadHook{after: func() error {
		_, err := forgetMemory(ctx, other, "t1", "backfill-forget", 1)
		return err
	}}
	rdb.AddHook(hook)
	if _, _, err := backfillBM25Page(ctx, rdb, "t1", []string{"backfill-forget"}); err != nil {
		t.Fatal(err)
	}
	if hook.after != nil {
		t.Fatal("backfill race hook did not run")
	}
	if rdb.ZCard(ctx, bm25PostingKey("t1", "obsolete")).Val() != 0 || rdb.Exists(ctx, bm25TFKey("t1", "backfill-forget", "obsolete")).Val() != 0 {
		t.Fatal("backfill recreated forgotten lexical content from its stale page")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
