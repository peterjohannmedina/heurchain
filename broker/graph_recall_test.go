package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
)

func graphFixture(t *testing.T) *recallFixture {
	t.Helper()
	f := newRecallFixture(t)
	old := heurgraphCfg
	heurgraphCfg.Enabled = true
	heurgraphCfg.TenantAllowlist = []string{"t1"}
	heurgraphCfg.ActivationCutoff = -100
	t.Cleanup(func() { heurgraphCfg = old })
	return f
}
func graphTestEdge(t *testing.T, f *recallFixture, id, from, to, doc string) {
	t.Helper()
	ctx := context.Background()
	for _, entity := range []string{from, to} {
		if _, err := f.s.db.Exec(`INSERT OR IGNORE INTO graph_entities(id,tenant_id,name,type) VALUES (?,'t1',?,'entity')`, entity, entity); err != nil {
			t.Fatal(err)
		}
	}
	data, err := f.s.rdb.HGetAll(ctx, vecKey("t1", doc)).Result()
	if err != nil {
		t.Fatal(err)
	}
	hash := graphSourceFingerprint(data)
	if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO graph_extractions(tenant_id,source_memory_id,source_hash,model,status) VALUES ('t1',?,?,'fixture','complete')`, doc, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO graph_relations(id,tenant_id,source_id,target_id,predicate,source_memory_id,src_created_at,source_hash,evidence) VALUES (?,'t1',?,?,'related',?,?,?,?)`, id, from, to, doc, data["created_at"], hash, data["text"]); err != nil {
		t.Fatal(err)
	}
}
func graphTraverse(t *testing.T, f *recallFixture, agent string, tags []string, scope RecallOptions) graphReadResult {
	t.Helper()
	r, err := traverseGraph(context.Background(), f.s.rdb, f.s.db, "t1", "alpha", agent, tags, scope, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGraphTwoHopRecallAndDefaultCompatibility(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "base", "alpha baseline", "a", nil, 1)
	f.store(t, "bridge", "bridge evidence", "a", nil, .1)
	f.store(t, "remote", "remote evidence", "a", nil, .1)
	graphTestEdge(t, f, "r1", "alpha", "beta", "bridge")
	graphTestEdge(t, f, "r2", "beta", "gamma", "remote")
	f.mu.Lock()
	for id, point := range f.points {
		if point.Payload["source_id"] != "base" {
			delete(f.points, id)
		}
	}
	f.mu.Unlock()
	off, offDiag := f.search(t, map[string]interface{}{"text": "alpha", "limit": 10})
	if len(off) != 1 || off[0].ID != "base" || offDiag.GraphStatus != "" {
		t.Fatalf("off %+v %+v", off, offDiag)
	}
	on, diag := f.search(t, map[string]interface{}{"text": "alpha", "limit": 10, "graph_mode": "on"})
	if len(on) != 3 || diag.GraphStatus != "ok" || diag.GraphCandidates != 2 {
		t.Fatalf("on %+v %+v", on, diag)
	}
	found := false
	for _, r := range on {
		if r.ID == "remote" {
			found = true
			if len(r.GraphProvenance) != 2 || r.GraphRank == 0 {
				t.Fatalf("missing two-hop provenance: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("graph-only evidence missing")
	}
	explicitOff, _ := f.search(t, map[string]interface{}{"text": "alpha", "limit": 10, "graph_mode": "off"})
	if !reflect.DeepEqual(off, explicitOff) {
		t.Fatal("graph off changed baseline")
	}
}

func TestGraphIntermediateSourceFilters(t *testing.T) {
	for _, kind := range []string{"agent", "tags", "time", "kind", "changed", "vault", "deleted", "silo", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			f := graphFixture(t)
			f.store(t, "bridge", "bridge evidence", "hidden", []string{"private"}, 1)
			f.store(t, "remote", "remote evidence", "public", []string{"public"}, 1)
			ctx := context.Background()
			scope := RecallOptions{GraphHops: 2}
			agent := ""
			var tags []string
			if kind == "time" {
				f.s.rdb.HSet(ctx, vecKey("t1", "bridge"), "metadata", `{"valid_from":"2099-01-01T00:00:00Z"}`)
				scope.AsOf = "2026-01-01T00:00:00Z"
			}
			if kind == "kind" {
				f.s.rdb.HSet(ctx, vecKey("t1", "bridge"), "metadata", `{"memory_kind":"creative"}`)
				scope.MemoryKind = "factual"
			}
			graphTestEdge(t, f, "r1", "alpha", "beta", "bridge")
			graphTestEdge(t, f, "r2", "beta", "gamma", "remote")
			switch kind {
			case "agent":
				agent = "public"
			case "tags":
				tags = []string{"public"}
			case "changed":
				f.s.rdb.HSet(ctx, vecKey("t1", "bridge"), "text", "changed")
			case "vault":
				if _, err := moveActiveToVault(ctx, f.s.rdb, "t1", "bridge"); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				f.s.rdb.Del(ctx, vecKey("t1", "bridge"))
			case "silo":
				f.s.rdb.HSet(ctx, vecKey("t1", "bridge"), "silo", "secret")
			case "incomplete":
				f.s.db.Exec(`DELETE FROM graph_extractions WHERE source_memory_id='bridge'`)
			}
			r := graphTraverse(t, f, agent, tags, scope)
			if len(r.Ranks) != 0 {
				t.Fatalf("hidden edge bridged: %+v", r)
			}
		})
	}
}

func TestGraphTenantSeedsAndTokenBoundaries(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "bridge", "evidence", "a", nil, 1)
	graphTestEdge(t, f, "r1", "alpha", "beta", "bridge")
	if _, err := f.s.db.Exec(`INSERT INTO tenants(id,name,email) VALUES ('t2','Other','other@test'); INSERT INTO graph_entities(id,tenant_id,name,type) VALUES ('other','t2','alpha','entity')`); err != nil {
		t.Fatal(err)
	}
	ids, err := graphSeeds(context.Background(), f.s.db, "t2", "alpha", nil)
	if err != nil || !reflect.DeepEqual(ids, []string{"other"}) {
		t.Fatalf("seeds %v %v", ids, err)
	}
	ids, err = graphSeeds(context.Background(), f.s.db, "t1", "alphabet", nil)
	if err != nil || len(ids) != 0 {
		t.Fatalf("substring matched %v %v", ids, err)
	}
	ids, err = graphSeeds(context.Background(), f.s.db, "t2", "", []string{"alpha"})
	if err != nil || len(ids) != 0 {
		t.Fatalf("foreign explicit seed %v %v", ids, err)
	}
}

func TestGraphBoundsCyclesAndCancellation(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "doc", "evidence", "a", nil, 1)
	for i := 0; i < graphFanout+10; i++ {
		graphTestEdge(t, f, fmt.Sprintf("r%03d", i), "alpha", fmt.Sprintf("node%03d", i), "doc")
	}
	graphTestEdge(t, f, "zcycle", "node000", "alpha", "doc")
	r := graphTraverse(t, f, "", nil, RecallOptions{GraphHops: 2})
	if !r.Truncated || r.Examined > graphMaxEdges || len(r.Ranks) != 1 {
		t.Fatalf("unbounded %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := traverseGraph(ctx, f.s.rdb, f.s.db, "t1", "alpha", "", nil, RecallOptions{}, nil, 10)
	if err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestGraphFailureFallsBackAndOptions(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "base", "alpha baseline", "a", nil, 1)
	baseline, _ := f.search(t, map[string]interface{}{"text": "alpha"})
	if _, err := f.s.db.Exec(`DROP TABLE graph_aliases`); err != nil {
		t.Fatal(err)
	}
	results, diag := f.search(t, map[string]interface{}{"text": "alpha", "graph_mode": "on"})
	if diag.GraphStatus != "failed" || !reflect.DeepEqual(baseline, results) {
		t.Fatalf("fallback %+v %+v", results, diag)
	}
	for _, option := range []RecallOptions{{GraphMode: "bad"}, {GraphMode: "on", Mode: "lexical"}, {GraphMode: "on", Mode: "dense"}, {GraphMode: "on", GraphHops: 3}, {GraphMode: "on", GraphWeight: -1}} {
		if err := option.normalize(); err == nil {
			t.Fatalf("accepted %+v", option)
		}
	}
}

func TestGraphAskAuthProvenanceAndDisabled(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "doc", "verbatim evidence", "a", nil, 1)
	graphTestEdge(t, f, "r1", "alpha", "beta", "doc")
	call := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/graph/ask", strings.NewReader(`{"query":"alpha"}`))
		r.Header.Set("X-HeurChain-API-Key", key)
		r.Header.Set("X-HeurChain-Tenant", "t1")
		w := httptest.NewRecorder()
		f.s.handleGraphAsk(w, r)
		return w
	}
	if w := call(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("auth %d", w.Code)
	}
	w := call("test-key")
	if w.Code != http.StatusOK {
		t.Fatalf("ask %d %s", w.Code, w.Body.String())
	}
	var body struct{ Citations []graphCitation }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Citations) != 1 || body.Citations[0].Evidence != "verbatim evidence" {
		t.Fatalf("citations %s %v", w.Body.String(), err)
	}
	heurgraphCfg.Enabled = false
	if w := call("test-key"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled %d", w.Code)
	}
}

func TestGraphActivationUsesCurrentSource(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "doc", "evidence", "a", nil, 1)
	graphTestEdge(t, f, "r1", "alpha", "beta", "doc")
	// Access counts are deliberately outside the source fingerprint.
	heurgraphCfg.ActivationCutoff = 1
	if r := graphTraverse(t, f, "", nil, RecallOptions{}); len(r.Ranks) != 0 {
		t.Fatal("low activation accepted")
	}
	f.s.rdb.HSet(context.Background(), vecKey("t1", "doc"), "access_count", 100000)
	if r := graphTraverse(t, f, "", nil, RecallOptions{}); len(r.Ranks) != 1 {
		t.Fatal("live activation ignored")
	}
}

func TestGraphWeightedFusionAndSourceCaps(t *testing.T) {
	options := RecallOptions{GraphMode: "on", GraphWeight: 2}
	if err := options.normalize(); err != nil {
		t.Fatal(err)
	}
	fused := fuseResultsWithGraph(map[string]float64{"both": 1}, nil, map[string]int{"both": 1, "only": 2}, nil, 0, options)
	if len(fused) != 2 || fused[0].ID != "both" || fused[0].RRFScore != 3.0/61.0 || fused[1].RRFScore != 2.0/62.0 {
		t.Fatalf("fusion %+v", fused)
	}
	f := graphFixture(t)
	for _, doc := range []string{"one", "two"} {
		f.store(t, doc, "separate evidence", "a", nil, 1)
		if err := f.s.rdb.HSet(context.Background(), vecKey("t1", doc), "metadata", `{"source":{"parent_id":"same"}}`).Err(); err != nil {
			t.Fatal(err)
		}
		graphTestEdge(t, f, "edge-"+doc, "alpha", doc, doc)
	}
	f.mu.Lock()
	f.points = map[string]QdrantPoint{}
	f.mu.Unlock()
	results, diag := f.search(t, map[string]interface{}{"text": "alpha", "graph_mode": "on", "max_per_source": 1})
	if len(results) != 1 || diag.GraphCandidates != 2 {
		t.Fatalf("caps %+v %+v", results, diag)
	}
}

func TestGraphReadRejectsIndexedEncryptedSources(t *testing.T) {
	for _, kind := range []string{"silo_id", "metadata_silo", "encrypted", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			f := graphFixture(t)
			f.store(t, "doc", "evidence", "a", nil, 1)
			field, value := "silo_id", "private"
			switch kind {
			case "metadata_silo":
				field, value = "metadata", `{"silo_id":"private"}`
			case "encrypted":
				field, value = "encrypted", "true"
			case "malformed":
				field, value = "metadata", "{"
			}
			if err := f.s.rdb.HSet(context.Background(), vecKey("t1", "doc"), field, value).Err(); err != nil {
				t.Fatal(err)
			}
			graphTestEdge(t, f, "r1", "alpha", "beta", "doc")
			if r := graphTraverse(t, f, "", nil, RecallOptions{}); len(r.Ranks) != 0 {
				t.Fatalf("ineligible indexed source returned %+v", r)
			}
		})
	}
}

func TestGraphTotalEdgeBudget(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "doc", "evidence", "a", nil, 1)
	for i := 0; i < 10; i++ {
		node := fmt.Sprintf("hub%02d", i)
		graphTestEdge(t, f, fmt.Sprintf("a%02d", i), "alpha", node, "doc")
		for j := 0; j < 30; j++ {
			graphTestEdge(t, f, fmt.Sprintf("b%02d-%02d", i, j), node, fmt.Sprintf("leaf%02d-%02d", i, j), "doc")
		}
	}
	r := graphTraverse(t, f, "", nil, RecallOptions{GraphHops: 2})
	if !r.Truncated || r.Examined != graphMaxEdges {
		t.Fatalf("budget %+v", r)
	}
}

// A real Redis hook changes the source after hydration has fetched its old
// bytes. The graph leg must be discarded, using the original query for fallback.
type graphHydrationRaceHook struct {
	rdb     *redis.Client
	changed atomic.Bool
}

func (h *graphHydrationRaceHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *graphHydrationRaceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *graphHydrationRaceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if err != nil {
			return err
		}
		for _, cmd := range cmds {
			args := cmd.Args()
			if cmd.Name() == "hgetall" && len(args) > 1 && args[1] == vecKey("t1", "graph-doc") && h.changed.CompareAndSwap(false, true) {
				return h.rdb.HSet(ctx, vecKey("t1", "graph-doc"), "text", "replacement source").Err()
			}
		}
		return nil
	}
}
func TestGraphSourceRacePreservesOriginalQueryFallback(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "base", "alpha baseline", "a", nil, 1)
	f.store(t, "graph-doc", "completely unrelated passage", "a", nil, .1)
	graphTestEdge(t, f, "r1", "alpha", "beta", "graph-doc")
	f.mu.Lock()
	for id, point := range f.points {
		if point.Payload["source_id"] != "base" {
			delete(f.points, id)
		}
	}
	f.mu.Unlock()
	baseline, _ := f.search(t, map[string]interface{}{"text": "alpha"})
	hook := &graphHydrationRaceHook{rdb: f.s.rdb}
	f.s.rdb.AddHook(hook)
	results, diag := f.search(t, map[string]interface{}{"text": "alpha", "graph_mode": "on"})
	if !hook.changed.Load() || diag.GraphStatus != "stale" || !reflect.DeepEqual(results, baseline) {
		t.Fatalf("race fallback %+v %+v baseline %+v", results, diag, baseline)
	}
}
