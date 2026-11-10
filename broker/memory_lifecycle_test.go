package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func lifecycleCall(t *testing.T, f *recallFixture, handler http.HandlerFunc, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/memory/test", strings.NewReader(string(data)))
	r.Header.Set("X-HeurChain-API-Key", "test-key")
	r.Header.Set("X-HeurChain-Tenant", "t1")
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}
func lifecycleJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var data map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
	return data
}

func TestMemoryCorrectionRetainsVersionsAndRejectsStaleWriter(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "preference", "prefers tea", "codex", []string{"fact"}, 1)
	w := lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "preference"})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	old := lifecycleJSON(t, w)["memory"].(map[string]interface{})
	if old["revision"] != float64(1) {
		t.Fatal(old)
	}
	update := map[string]interface{}{"id": "preference", "expected_revision": 1, "text": "prefers coffee", "reason": "customer correction"}
	w = lifecycleCall(t, f, f.s.handleMemoryUpdate, update)
	if w.Code != 200 || lifecycleJSON(t, w)["revision"] != float64(2) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryUpdate, update)
	if w.Code != 409 {
		t.Fatalf("stale writer: %d %s", w.Code, w.Body)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "preference", "include_history": true})
	data := lifecycleJSON(t, w)
	current := data["memory"].(map[string]interface{})
	history := data["history"].([]interface{})
	if current["text"] != "prefers coffee" || current["created_at"] != old["created_at"] || len(history) != 1 || history[0].(map[string]interface{})["text"] != "prefers tea" {
		t.Fatal(data)
	}
	results, _ := f.search(t, map[string]interface{}{"text": "prefers"})
	if len(results) != 1 || results[0].Text != "prefers coffee" {
		t.Fatal(results)
	}
}

func TestMemoryConcurrentCorrectionsHaveOneWinner(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "shared", "before", "codex", nil, 1)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- lifecycleCall(t, f, f.s.handleMemoryUpdate, map[string]interface{}{"id": "shared", "expected_revision": 1, "text": fmt.Sprintf("writer %d", i), "reason": "concurrent correction"}).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
}

func TestMemoryForgetPurgesArchiveHistoryIndexesAndBlocksResurrection(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	f.store(t, "remove-me", "secret old", "codex", nil, 1)
	f.store(t, "remove-me", "secret new", "codex", nil, 1)
	if _, err := moveActiveToVault(ctx, f.s.rdb, "t1", "remove-me"); err != nil {
		t.Fatal(err)
	}
	archived, _ := f.s.rdb.HGetAll(ctx, memoryVaultKey("t1", "remove-me")).Result()
	w := lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "remove-me", "expected_revision": 2, "confirm": true})
	if w.Code != 200 || lifecycleJSON(t, w)["cleanup_complete"] != true {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, key := range []string{vecKey("t1", "remove-me"), memoryVaultKey("t1", "remove-me"), memoryVersionsKey("t1", "remove-me"), bm25DocTermsKey("t1", "remove-me")} {
		if n, _ := f.s.rdb.Exists(ctx, key).Result(); n != 0 {
			t.Fatalf("retained %s", key)
		}
	}
	f.mu.Lock()
	n := len(f.points)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("dense record survived")
	}
	if err := restoreActiveFromVault(ctx, f.s.rdb, "t1", "remove-me", archived); !errors.Is(err, errMemoryForgotten) {
		t.Fatal(err)
	}
	if w = f.call(t, "/store", map[string]interface{}{"id": "remove-me", "text": "stale import", "embedding": []float32{1, 0}}); w.Code != 410 {
		t.Fatalf("resurrection %d %s", w.Code, w.Body)
	}
	if w = lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "remove-me", "include_history": true}); w.Code != 410 {
		t.Fatalf("forgotten history %d %s", w.Code, w.Body)
	}
	results, _ := f.search(t, map[string]interface{}{"text": "secret"})
	if len(results) != 0 {
		t.Fatal(results)
	}
	manifest, err := buildManifest(ctx, f.s.rdb, "t1")
	if err != nil || manifest["total"] != 0 {
		t.Fatal(manifest, err)
	}
	if memories, err := recentAgentMemories(ctx, f.s.rdb, "t1", "codex", 10); err != nil || len(memories) != 0 {
		t.Fatal(memories, err)
	}
	bumpAccessCounts(ctx, f.s.rdb, "t1", []string{"remove-me"})
	if n, _ := f.s.rdb.Exists(ctx, vecKey("t1", "remove-me")).Result(); n != 0 {
		t.Fatal("inflight recall recreated deleted record")
	}
	for _, confirm := range []bool{false, true} {
		w = lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "remove-me", "expected_revision": 2, "confirm": confirm})
		if confirm && w.Code != 200 || !confirm && w.Code != 400 {
			t.Fatalf("forget retry %d %s", w.Code, w.Body)
		}
	}
}

func TestMemoryForgetIndexOutageIsExplicitAndRetryable(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "outage", "forget this", "codex", nil, 1)
	f.fail.Store(true)
	req := map[string]interface{}{"id": "outage", "expected_revision": 1, "confirm": true}
	w := lifecycleCall(t, f, f.s.handleMemoryForget, req)
	reply := lifecycleJSON(t, w)
	if w.Code != 202 || reply["forgotten"] != true || reply["cleanup_complete"] != false || reply["retry_required"] != true {
		t.Fatal(w.Code, reply)
	}
	results, _ := f.search(t, map[string]interface{}{"text": "forget"})
	if len(results) != 0 {
		t.Fatal(results)
	}
	f.fail.Store(false)
	w = lifecycleCall(t, f, f.s.handleMemoryForget, req)
	if w.Code != 200 || lifecycleJSON(t, w)["cleanup_complete"] != true {
		t.Fatal(w.Code, w.Body)
	}
}

func TestMemoryListArchivePaginationFiltersAndTenantIsolation(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		w := f.call(t, "/store", map[string]interface{}{"id": fmt.Sprintf("item-%d", i), "text": "needle", "agent_id": "codex", "namespace": "self", "embedding": []float32{1, 0}, "metadata": map[string]interface{}{"project": "alpha", "source": "fixture"}})
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	if _, err := moveActiveToVault(ctx, f.s.rdb, "t1", "item-0"); err != nil {
		t.Fatal(err)
	}
	if err := storeVec(ctx, f.s.rdb, "other", storedVector{ID: "foreign", Text: "never disclose", Embedding: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		w := lifecycleCall(t, f, f.s.handleMemoryList, map[string]interface{}{"limit": 2, "cursor": cursor, "filters": map[string]interface{}{"metadata": map[string]interface{}{"project": "alpha"}}})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		data := lifecycleJSON(t, w)
		for _, item := range data["memories"].([]interface{}) {
			seen[item.(map[string]interface{})["id"].(string)] = true
		}
		cursor = data["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(seen) != 6 || seen["foreign"] || cursor != "" {
		t.Fatal(seen, cursor)
	}
	if w := lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "foreign"}); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	results, _ := f.search(t, map[string]interface{}{"text": "needle", "filters": map[string]interface{}{"namespace": "self", "metadata": map[string]interface{}{"project": "alpha"}}})
	if len(results) != 5 {
		t.Fatal(results)
	}
	results, _ = f.search(t, map[string]interface{}{"text": "needle", "filters": map[string]interface{}{"metadata": map[string]interface{}{"project": "beta"}}})
	if len(results) != 0 {
		t.Fatal(results)
	}
	if w := f.call(t, "/search", map[string]interface{}{"text": "needle", "filters": map[string]interface{}{"unknown": true}}); w.Code != 400 {
		t.Fatalf("unknown filter silently ignored: %s", w.Body)
	}
}

func TestCheckpointResumesFromArchiveWithoutIndexAndCannotOverwriteMemory(t *testing.T) {
	f := newRecallFixture(t)
	req := map[string]interface{}{"id": "cp-one", "agent_id": "codex", "summary": "Reviewed repair, deployment pending", "project": "alpha", "next_steps": []string{"verify CI", "review PR"}}
	f.fail.Store(true)
	w := lifecycleCall(t, f, f.s.handleMemoryCheckpoint, req)
	reply := lifecycleJSON(t, w)
	if w.Code != 202 || reply["stored"] != true || reply["indexed"] != false || reply["checkpoint_id"] != "cp-one" || reply["automatic_compaction_hook"] != false {
		t.Fatal(w.Code, reply)
	}
	if _, err := moveActiveToVault(context.Background(), f.s.rdb, "t1", "cp-one"); err != nil {
		t.Fatal(err)
	}
	// A different server instance shares persisted state; no session/cache token
	// or embedding service is needed to resume the archived handoff.
	other := &Server{config: f.s.config, rdb: f.s.rdb, db: f.s.db}
	w = lifecycleCall(t, f, other.handleMemoryResume, map[string]interface{}{"id": "cp-one"})
	reply = lifecycleJSON(t, w)
	if w.Code != 200 || reply["status"] != "archived" || reply["checkpoint"].(map[string]interface{})["summary"] != req["summary"] {
		t.Fatal(w.Code, reply)
	}
	req["summary"] = "different snapshot"
	if w = lifecycleCall(t, f, f.s.handleMemoryCheckpoint, req); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestMemoryHistoryLimitsAndLegacyUpgrade(t *testing.T) {
	for _, size := range []int{20, 32000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newRecallFixture(t)
			ctx := context.Background()
			// Simulate an ordinary pre-release hash with no revision metadata.
			if err := f.s.rdb.HSet(ctx, vecKey("t1", "legacy"), "text", "original", "created_at", "2026-01-01T00:00:00Z").Err(); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 105; i++ {
				if err := storeVec(ctx, f.s.rdb, "t1", storedVector{ID: "legacy", Text: fmt.Sprint(i) + strings.Repeat("x", size), Metadata: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			history, err := f.s.rdb.LRange(ctx, memoryVersionsKey("t1", "legacy"), 0, -1).Result()
			if err != nil {
				t.Fatal(err)
			}
			bytes := 0
			for _, item := range history {
				bytes += len(item)
			}
			if len(history) > memoryRevisionRetention || bytes > memoryRevisionByteLimit || len(history) == 0 {
				t.Fatal(len(history), bytes)
			}
			if size == 20 && len(history) != 100 {
				t.Fatal("count bound did not retain expected history")
			}
			w := lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "legacy"})
			current := lifecycleJSON(t, w)["memory"].(map[string]interface{})
			if current["revision"] != float64(106) || current["created_at"] != "2026-01-01T00:00:00Z" {
				t.Fatal(current)
			}
			if w = lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "legacy", "revision": 1}); w.Code != 404 {
				t.Fatal("pruned revision returned", w.Code)
			}
		})
	}
}

func TestMemoryCorrectionIndexRetryDoesNotCreateRevision(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "retry-update", "old", "codex", nil, 1)
	f.fail.Store(true)
	req := map[string]interface{}{"id": "retry-update", "expected_revision": 1, "text": "new", "reason": "correct"}
	w := lifecycleCall(t, f, f.s.handleMemoryUpdate, req)
	reply := lifecycleJSON(t, w)
	if w.Code != 202 || reply["stored"] != true || reply["indexed"] != false || reply["retry_required"] != true {
		t.Fatal(w.Code, reply)
	}
	f.fail.Store(false)
	req["expected_revision"] = 2
	w = lifecycleCall(t, f, f.s.handleMemoryUpdate, req)
	reply = lifecycleJSON(t, w)
	if w.Code != 200 || reply["revision"] != float64(2) || reply["changed"] != false || reply["indexed"] != true {
		t.Fatal(w.Code, reply)
	}
}

func TestForgetRequiresCurrentRevisionAndValidTenantCredential(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "protected", "keep me", "codex", nil, 1)
	for _, tc := range []struct {
		key, tenant      string
		revision, status int
	}{{"test-key", "t1", 2, 409}, {"wrong", "t1", 1, 401}, {"test-key", "other", 1, 401}} {
		r := httptest.NewRequest("POST", "/memory/forget", strings.NewReader(fmt.Sprintf(`{"id":"protected","expected_revision":%d,"confirm":true}`, tc.revision)))
		r.Header.Set("X-HeurChain-API-Key", tc.key)
		r.Header.Set("X-HeurChain-Tenant", tc.tenant)
		w := httptest.NewRecorder()
		f.s.handleMemoryForget(w, r)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body)
		}
	}
	w := lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "protected"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestQdrantDeleteMissingCollectionAndRoute(t *testing.T) {
	for _, exists := range []bool{false, true} {
		q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && exists {
				w.WriteHeader(200)
				return
			}
			w.WriteHeader(404)
		}))
		client := &QdrantClient{baseURL: q.URL, httpClient: q.Client()}
		err := client.DeletePoint(context.Background(), "t1", "missing")
		q.Close()
		if (err == nil) == exists {
			t.Fatal("delete 404 not checked against collection existence", exists, err)
		}
	}
}

func TestMemoryQueryStructuredFiltersAndInvalidDates(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "dated", "needle", "codex", []string{"fact"}, 1)
	f.s.rdb.HSet(context.Background(), vecKey("t1", "dated"), "created_at", "2026-10-01T00:00:00Z", "updated_at", "2026-10-13T20:00:00Z", "metadata", `{"project":"alpha","verified":true}`)
	for _, tc := range []struct {
		filter        string
		status, count int
	}{
		{`{"agent_id":"codex","metadata":{"project":"alpha","verified":true},"created_after":"2026-10-01T00:00:00Z","updated_before":"2026-10-13T20:00:00Z"}`, 200, 1},
		{`{"updated_before":"2026-10-13T19:59:59Z"}`, 200, 0},
		{`{"metadata":{"project":"beta"}}`, 200, 0},
		{`{"status":"archived"}`, 200, 0},
		{`{"created_before":"bad"}`, 400, 0},
		{`{"created_after":"2026-10-14T00:00:00Z","created_before":"2026-10-01T00:00:00Z"}`, 400, 0},
		{`{"metadata":{"project":["alpha"]}}`, 400, 0},
	} {
		var body interface{}
		if err := json.Unmarshal([]byte(`{"text":"needle","filters":`+tc.filter+`}`), &body); err != nil {
			t.Fatal(err)
		}
		w := lifecycleCall(t, f, f.s.handleQuery, body)
		if w.Code != tc.status {
			t.Fatal(tc.filter, w.Code, w.Body)
		}
		if tc.status == 200 && len(lifecycleJSON(t, w)["results"].([]interface{})) != tc.count {
			t.Fatal(tc.filter, w.Body)
		}
	}
}

func TestLifecycleDiscoveryAndOpenAPIRoutes(t *testing.T) {
	paths := openAPISpec()["paths"].(map[string]interface{})
	for tool, path := range memoryToolPaths {
		if paths[path] == nil {
			t.Fatal("missing OpenAPI route", tool, path)
		}
		found := false
		for _, endpoint := range discoveryJSON.Endpoints {
			if endpoint.Path == path && endpoint.AuthRequired {
				found = true
			}
		}
		if !found {
			t.Fatal("missing authenticated discovery route", path)
		}
	}
}

func TestCheckpointCannotAdoptOrdinaryMemoryByMetadata(t *testing.T) {
	f := newRecallFixture(t)
	checkpoint := memoryCheckpoint{ID: "claimed-checkpoint", AgentID: "codex", Summary: "ordinary data"}
	text, _ := json.Marshal(checkpoint)
	w := f.call(t, "/store", map[string]interface{}{"id": checkpoint.ID, "agent_id": checkpoint.AgentID, "text": string(text), "metadata": map[string]string{"kind": "checkpoint"}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryCheckpoint, checkpoint)
	if w.Code != 409 {
		t.Fatal("untrusted metadata adopted as protected snapshot", w.Code, w.Body)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryResume, map[string]string{"id": checkpoint.ID})
	if w.Code != 400 {
		t.Fatal("ordinary record presented as checkpoint", w.Code, w.Body)
	}
}
