package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const graphTestSource = "Mira operates the Cedar observatory. Cedar observatory uses the Lumen receiver."
const graphTestOutput = `{"triples":[{"subject":"Mira","predicate":"operates","object":"Cedar observatory","evidence":"Mira operates the Cedar observatory."}]}`

func graphTestConfig(t *testing.T, h http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	old := heurgraphCfg
	t.Cleanup(func() { heurgraphCfg = old })
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); h(w, r) }))
	t.Cleanup(server.Close)
	heurgraphCfg = HeurgraphConfig{Enabled: true, TenantAllowlist: []string{"t1"}, OllamaURL: server.URL, ExtractionModel: "test-model", RequestTimeoutSeconds: 1, MaxSourceBytes: 24000, MaxTriples: 32, ReconcileBatchSize: 2, MaxAttempts: 2}
	return calls
}

func graphReply(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"done": true, "message": map[string]string{"content": content}})
}

func putGraphSource(t *testing.T, rdb *redis.Client, doc, text string) {
	t.Helper()
	if err := storeVec(context.Background(), rdb, "t1", storedVector{ID: doc, Text: text, AgentID: "current-agent", Metadata: json.RawMessage(`{"memory_kind":"fact"}`), Namespace: "notes"}); err != nil {
		t.Fatal(err)
	}
}

func graphCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGraphExtractionSchemaAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		want         int
		bad          bool
	}{
		{"valid", graphTestOutput, 1, false},
		{"empty", `{"triples":[]}`, 0, false},
		{"malformed", `{"triples":`, 0, true},
		{"absent", `{}`, 0, true},
		{"null", `{"triples":null}`, 0, true},
		{"unknown", `{"triples":[],"answer":"x"}`, 0, true},
		{"trailing", `{"triples":[]} {}`, 0, true},
		{"inventedEvidence", strings.Replace(graphTestOutput, "Mira operates the Cedar observatory.", "Mira founded Cedar observatory.", 1), 0, true},
		{"inventedEntity", strings.Replace(graphTestOutput, `"subject":"Mira"`, `"subject":"Juno"`, 1), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/chat" || r.Method != "POST" {
					t.Errorf("unexpected extraction route %s %s", r.Method, r.URL.Path)
				}
				var payload map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["stream"] != false || payload["format"] == nil {
					t.Error("missing bounded structured generation contract")
				}
				graphReply(w, tc.output)
			})
			triples, err := extractGraphTriples(context.Background(), graphTestSource)
			if (err != nil) != tc.bad || len(triples) != tc.want {
				t.Fatalf("triples=%v err=%v", triples, err)
			}
		})
	}
}

func TestGraphExtractionCancellationTimeoutAndBounds(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := extractGraphTriples(ctx, graphTestSource); err == nil {
		t.Fatal("timeout accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation not bounded")
	}
	before := calls.Load()
	if _, err := extractGraphTriples(context.Background(), strings.Repeat("x", 24001)); err == nil {
		t.Fatal("oversize source accepted")
	}
	if calls.Load() != before {
		t.Fatal("oversized source reached model")
	}
}

func TestGraphIndexReplayOverwriteEmptyAndMetadata(t *testing.T) {
	output := graphTestOutput
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, output) })
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	putGraphSource(t, rdb, "doc", graphTestSource)
	msg := redis.XMessage{Values: map[string]interface{}{"data": `{"doc_id":"doc","agent_id":"obsolete-agent","namespace":"ops","session_id":"obsolete-session"}`}}
	if err := processHeurgraphItem(ctx, rdb, db, "t1", msg); err != nil {
		t.Fatal(err)
	}
	if err := processHeurgraphItem(ctx, rdb, db, "t1", msg); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || graphCount(t, db, "graph_relations") != 1 {
		t.Fatal("replay regenerated extraction")
	}
	var agent, namespace, session, evidence string
	if err := db.QueryRow(`SELECT agent_id,namespace,source_session_id,evidence FROM graph_relations`).Scan(&agent, &namespace, &session, &evidence); err != nil {
		t.Fatal(err)
	}
	if agent != "current-agent" || namespace != "notes" || session != "" || evidence != "Mira operates the Cedar observatory." {
		t.Fatalf("stale event metadata contaminated source: %s %s %s %s", agent, namespace, session, evidence)
	}
	_ = rdb.HIncrBy(ctx, vecKey("t1", "doc"), "access_count", 1).Err()
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("access reinforcement caused extraction")
	}
	putGraphSource(t, rdb, "doc", "Mira retired from the observatory.")
	output = `{"triples":[]}`
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err != nil {
		t.Fatal(err)
	}
	if graphCount(t, db, "graph_relations") != 0 || graphCount(t, db, "graph_extractions") != 1 {
		t.Fatal("empty overwrite did not atomically replace edges")
	}
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("successful empty result not recorded")
	}
	heurgraphCfg.ExtractionModel = "new-model"
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("model revision did not invalidate ledger")
	}
}

func TestGraphIndexStaleSourceRollsBack(t *testing.T) {
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		_ = rdb.HSet(ctx, vecKey("t1", "doc"), "text", "Changed during extraction.").Err()
		graphReply(w, graphTestOutput)
	})
	putGraphSource(t, rdb, "doc", graphTestSource)
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err == nil {
		t.Fatal("stale source committed")
	}
	if graphCount(t, db, "graph_relations") != 0 || graphCount(t, db, "graph_extractions") != 0 || graphCount(t, db, "graph_entities") != 0 {
		t.Fatal("stale transaction leaked writes")
	}
}

func TestGraphIndexSiloVaultDeleteAndAllowlist(t *testing.T) {
	for _, mode := range []string{"silo", "metadata_silo", "vault", "delete", "inactive"} {
		t.Run(mode, func(t *testing.T) {
			calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, graphTestOutput) })
			db, rdb := newTestDB(t), newTestRedis(t)
			ctx := context.Background()
			putGraphSource(t, rdb, "doc", graphTestSource)
			if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "silo":
				_ = rdb.HSet(ctx, vecKey("t1", "doc"), "silo_id", "private").Err()
			case "metadata_silo":
				_ = rdb.HSet(ctx, vecKey("t1", "doc"), "metadata", `{"silo_id":"private"}`).Err()
			case "vault":
				if err := vaultDoc(ctx, rdb, "t1", "doc"); err != nil {
					t.Fatal(err)
				}
			case "delete":
				_ = rdb.Del(ctx, vecKey("t1", "doc")).Err()
			case "inactive":
				_ = rdb.SRem(ctx, vecSetKey("t1"), "doc").Err()
			}
			if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || graphCount(t, db, "graph_relations") != 0 || graphCount(t, db, "graph_extractions") != 0 {
				t.Fatal("ineligible source retained graph or reached extraction")
			}
			if err := indexGraphSource(ctx, rdb, db, "other", "doc"); err == nil {
				t.Fatal("tenant allowlist not enforced")
			}
		})
	}
}

func TestGraphIndexTransactionFailureLeavesPending(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, graphTestOutput) })
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	putGraphSource(t, rdb, "doc", graphTestSource)
	if _, err := db.Exec(`CREATE TRIGGER fail_graph_ledger BEFORE INSERT ON graph_extractions BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	queueHeurgraphExtraction(ctx, rdb, "t1", "doc", "", "", "")
	if _, err := drainTenantStream(ctx, rdb, db, "t1", 1); err != nil {
		t.Fatal(err)
	}
	pending, err := rdb.XPending(ctx, heurgraphPendingKey("t1"), heurgraphGroup).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 1 || graphCount(t, db, "graph_relations") != 0 || graphCount(t, db, "graph_extractions") != 0 {
		t.Fatal("failed transaction acknowledged or leaked edges")
	}
	if calls.Load() != 1 {
		t.Fatal("missing extraction")
	}
}

func TestGraphIndexBoundedRetriesAndDeadletter(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	putGraphSource(t, rdb, "doc", graphTestSource)
	for i := 0; i < 5; i++ {
		if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err == nil {
			t.Fatal("failed extraction accepted")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unbounded retries: %d", calls.Load())
	}
	n, err := rdb.XLen(ctx, "tenant:t1:heurgraph:deadletter").Result()
	if err != nil || n != 1 {
		t.Fatalf("deadletter count=%d err=%v", n, err)
	}
	if graphCount(t, db, "graph_extractions") != 0 {
		t.Fatal("failure marked complete")
	}
}

func TestGraphReconciliationResumesWithoutQueueAndCleansMissing(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, graphTestOutput) })
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		putGraphSource(t, rdb, fmt.Sprint(i), graphTestSource)
	}
	for i := 0; i < 10; i++ {
		if err := reconcileGraphTenant(ctx, rdb, db, "t1"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 5 || graphCount(t, db, "graph_extractions") != 5 {
		t.Fatalf("backfill repeated or incomplete: calls=%d", calls.Load())
	}
	_ = rdb.Del(ctx, vecKey("t1", "0")).Err()
	_ = rdb.SRem(ctx, vecSetKey("t1"), "0").Err()
	for i := 0; i < 4; i++ {
		if err := reconcileGraphTenant(ctx, rdb, db, "t1"); err != nil {
			t.Fatal(err)
		}
	}
	if graphCount(t, db, "graph_extractions") != 4 || graphCount(t, db, "graph_relations") != 4 {
		t.Fatal("deleted ledger source survived cleanup")
	}
	putGraphSource(t, rdb, "0", graphTestSource)
	for i := 0; i < 4; i++ {
		if err := reconcileGraphTenant(ctx, rdb, db, "t1"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 6 || graphCount(t, db, "graph_extractions") != 5 {
		t.Fatal("restored source not recovered")
	}
}

func TestGraphFingerprintCanonicalAndAccessIndependent(t *testing.T) {
	a := map[string]string{"text": "one", "metadata": `{"b":2,"a":1}`, "access_count": "1"}
	b := map[string]string{"text": "one", "metadata": `{"a":1,"b":2}`, "access_count": "20"}
	if graphSourceFingerprint(a) != graphSourceFingerprint(b) {
		t.Fatal("format/access changed fingerprint")
	}
	b["agent_id"] = "other"
	if graphSourceFingerprint(a) == graphSourceFingerprint(b) {
		t.Fatal("agent metadata not fingerprinted")
	}
}

func TestGraphExtractionSweepCallBudget(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, graphTestOutput) })
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.WithValue(context.Background(), graphCallBudgetKey{}, &graphCallBudget{remaining: 1})
	putGraphSource(t, rdb, "a", graphTestSource)
	putGraphSource(t, rdb, "b", graphTestSource)
	if err := indexGraphSource(ctx, rdb, db, "t1", "a"); err != nil {
		t.Fatal(err)
	}
	if err := indexGraphSource(ctx, rdb, db, "t1", "b"); err == nil {
		t.Fatal("budget exceeded")
	}
	if calls.Load() != 1 {
		t.Fatal("budget did not bound model calls")
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := indexGraphSource(cancelCtx, rdb, db, "t1", "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

type graphAckFailureHook struct{ fail atomic.Bool }

func (h *graphAckFailureHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *graphAckFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *graphAckFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "xack" && h.fail.Load() {
			return errors.New("injected ack failure")
		}
		return next(ctx, cmd)
	}
}

func TestGraphIndexAckFailureReplaysWithoutGeneration(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, graphTestOutput) })
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	hook := &graphAckFailureHook{}
	hook.fail.Store(true)
	rdb.AddHook(hook)
	putGraphSource(t, rdb, "doc", graphTestSource)
	queueHeurgraphExtraction(ctx, rdb, "t1", "doc", "", "", "")
	if _, err := drainTenantStream(ctx, rdb, db, "t1", 1); err != nil {
		t.Fatal(err)
	}
	pending, err := rdb.XPending(ctx, heurgraphPendingKey("t1"), heurgraphGroup).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 1 || graphCount(t, db, "graph_extractions") != 1 {
		t.Fatal("ack failure lost pending entry or committed ledger")
	}
	msgs, err := rdb.XRange(ctx, heurgraphPendingKey("t1"), "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	hook.fail.Store(false)
	handleMessages(ctx, rdb, db, "t1", heurgraphPendingKey("t1"), msgs)
	pending, err = rdb.XPending(ctx, heurgraphPendingKey("t1"), heurgraphGroup).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 0 || calls.Load() != 1 || graphCount(t, db, "graph_relations") != 1 {
		t.Fatal("ack replay repeated generation or failed acknowledgement")
	}
}

func TestGraphMigrationV10UpgradesV9AndPreservesLegacyRows(t *testing.T) {
	db := newTestDB(t)
	// Reconstruct the exact v8 graph shape, retaining a legacy edge, then apply graph v10 after master v9.
	for _, stmt := range []string{
		`DROP TABLE graph_extractions`,
		`DROP INDEX idx_gent_lower_name`, `DROP INDEX idx_galias_lower_alias`,
		`DROP INDEX idx_grel_source_order`, `DROP INDEX idx_grel_target_order`,
		`ALTER TABLE graph_relations DROP COLUMN source_hash`,
		`ALTER TABLE graph_relations DROP COLUMN evidence`,
		`DELETE FROM schema_version WHERE version >= 10`,
		`INSERT INTO graph_entities(tenant_id,id,name,type) VALUES('t1','a','alpha','entity'),('t1','b','beta','entity')`,
		`INSERT INTO graph_relations(tenant_id,id,source_id,target_id,predicate,source_memory_id,src_created_at) VALUES('t1','old','a','b','uses','legacy','2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := runMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatal(err)
	}
	var hash, evidence string
	if err := db.QueryRow(`SELECT source_hash,evidence FROM graph_relations WHERE id='old'`).Scan(&hash, &evidence); err != nil {
		t.Fatal(err)
	}
	if hash != "" || evidence != "" {
		t.Fatal("migration invented provenance for legacy edge")
	}
	if graphCount(t, db, "graph_relations") != 1 || graphCount(t, db, "graph_extractions") != 0 {
		t.Fatal("migration changed legacy graph data")
	}
}

func TestGraphTenantDeadlineCountsTimeoutAndResumesCheckpoint(t *testing.T) {
	ctx := context.Background()
	db, rdb := newTestDB(t), newTestRedis(t)
	var slow atomic.Bool
	slow.Store(true)
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if slow.Load() && strings.Contains(payload.Messages[1].Content, "slow source") {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		graphReply(w, `{"triples":[]}`)
	})
	putGraphSource(t, rdb, "slow", "slow source")
	putGraphSource(t, rdb, "fast", "fast source")
	key := "tenant:t1:heurgraph:reconcile"
	if err := rdb.HSet(ctx, key, "cursor", 0, "pending", `["slow","fast"]`).Err(); err != nil {
		t.Fatal(err)
	}
	tenantCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	if err := reconcileGraphTenant(tenantCtx, rdb, db, "t1"); err != nil {
		t.Fatal(err)
	}
	cancel()
	data, err := rdb.HGetAll(ctx, vecKey("t1", "slow")).Result()
	if err != nil {
		t.Fatal(err)
	}
	failureKey := "tenant:t1:heurgraph:failure:" + graphDigest("slow"+graphSourceFingerprint(data)+graphModelIdentity())
	count, err := rdb.Get(ctx, failureKey).Int()
	if err != nil || count != 1 {
		t.Fatalf("tenant deadline did not count timeout: %d %v", count, err)
	}
	pending, err := rdb.HGet(ctx, key, "pending").Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending != `["fast"]` || calls.Load() != 1 {
		t.Fatalf("checkpoint=%s calls=%d; want next source with no late generation", pending, calls.Load())
	}
	slow.Store(false)
	if err := reconcileGraphTenant(ctx, rdb, db, "t1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || graphCount(t, db, "graph_extractions") != 1 {
		t.Fatal("resume did not continue at fast source")
	}
	pending, err = rdb.HGet(ctx, key, "pending").Result()
	if err != nil || pending != `[]` {
		t.Fatalf("resumed checkpoint=%s err=%v", pending, err)
	}
}

func TestGraphTenantDeadlineReachesRetryLimit(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		var payload interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	})
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx := context.Background()
	putGraphSource(t, rdb, "doc", graphTestSource)
	for attempt := 0; attempt < 2; attempt++ {
		tenantCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		err := indexGraphSource(tenantCtx, rdb, db, "t1", "doc")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt timeout=%v", err)
		}
	}
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); err == nil {
		t.Fatal("retry cooldown missing")
	}
	if calls.Load() != 2 {
		t.Fatalf("deadline retry cap failed: %d calls", calls.Load())
	}
	n, err := rdb.XLen(ctx, "tenant:t1:heurgraph:deadletter").Result()
	if err != nil || n != 1 {
		t.Fatalf("deadletter=%d err=%v", n, err)
	}
}

func TestGraphShutdownKeepsLastDurableCheckpoint(t *testing.T) {
	db, rdb := newTestDB(t), newTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var shutdown atomic.Bool
	shutdown.Store(true)
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if shutdown.Load() && payload.Messages[1].Content == "second source" {
			cancel()
		}
		graphReply(w, `{"triples":[]}`)
	})
	putGraphSource(t, rdb, "first", "first source")
	putGraphSource(t, rdb, "second", "second source")
	key := "tenant:t1:heurgraph:reconcile"
	if err := rdb.HSet(ctx, key, "cursor", 0, "pending", `["first","second"]`).Err(); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGraphTenant(ctx, rdb, db, "t1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown result=%v", err)
	}
	pending, err := rdb.HGet(context.Background(), key, "pending").Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending != `["second"]` || graphCount(t, db, "graph_extractions") != 1 || calls.Load() != 2 {
		t.Fatalf("shutdown lost prior progress: pending=%s calls=%d", pending, calls.Load())
	}
	if err := reconcileGraphTenant(ctx, rdb, db, "t1"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled reconciliation continued")
	}
	if calls.Load() != 2 {
		t.Fatal("generation continued after shutdown")
	}
	shutdown.Store(false)
	if err := reconcileGraphTenant(context.Background(), rdb, db, "t1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || graphCount(t, db, "graph_extractions") != 2 {
		t.Fatal("restart regenerated prior source or lost pending source")
	}
}

func TestGraphTooLittleDeadlineStartsNoGeneration(t *testing.T) {
	calls := graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) { graphReply(w, `{"triples":[]}`) })
	db, rdb := newTestDB(t), newTestRedis(t)
	putGraphSource(t, rdb, "doc", graphTestSource)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := indexGraphSource(ctx, rdb, db, "t1", "doc"); !errors.Is(err, errGraphBudgetDeferred) {
		t.Fatalf("expected deferred generation: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("generation started without metadata reserve")
	}
}
