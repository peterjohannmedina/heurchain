package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestGraphModelReextractionThenForgetRemovesOmittedDerivations(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "unique"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecallFixture(t)
			ctx := context.Background()
			var replacement atomic.Bool
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
				output := `{"triples":[{"subject":"Mira","predicate":"operates","object":"Cedar observatory","evidence":"Mira operates the Cedar observatory."},{"subject":"Cedar observatory","predicate":"uses","object":"Lumen receiver","evidence":"Cedar observatory uses the Lumen receiver."}]}`
				if replacement.Load() {
					output = graphTestOutput
				}
				if payload.Messages[1].Content == "Lumen receiver belongs to the Summit lab." {
					output = `{"triples":[{"subject":"Lumen receiver","predicate":"belongs to","object":"Summit lab","evidence":"Lumen receiver belongs to the Summit lab."}]}`
				}
				graphReply(w, output)
			})
			f.store(t, "source", graphTestSource, "a", nil, 1)
			if err := indexGraphSource(ctx, f.s.rdb, f.s.db, "t1", "source"); err != nil {
				t.Fatal(err)
			}
			if shared {
				f.store(t, "retained", "Lumen receiver belongs to the Summit lab.", "a", nil, 1)
				if err := indexGraphSource(ctx, f.s.rdb, f.s.db, "t1", "retained"); err != nil {
					t.Fatal(err)
				}
			}
			for _, entity := range []string{"mira", "cedar observatory", "lumen receiver", "summit lab"} {
				if !shared && entity == "summit lab" {
					continue
				}
				id := graphDigest(entity)
				if _, err := f.s.db.Exec(`UPDATE graph_entities SET summary='old derived text' WHERE tenant_id='t1' AND id=?`, id); err != nil {
					t.Fatal(err)
				}
				if err := f.s.rdb.HSet(ctx, heurgraphEntKey("t1", id), "summary", "old derived text").Err(); err != nil {
					t.Fatal(err)
				}
				if err := f.s.rdb.ZAdd(ctx, heurgraphAdjKey("t1", id), redis.Z{Score: 1, Member: "old edge"}).Err(); err != nil {
					t.Fatal(err)
				}
			}
			replacement.Store(true)
			heurgraphCfg.ExtractionModel = "replacement-model"
			before := calls.Load()
			if err := indexGraphSource(ctx, f.s.rdb, f.s.db, "t1", "source"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != before+1 {
				t.Fatal("model identity change did not re-extract unchanged source")
			}
			var sourceEdges int
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM graph_relations WHERE tenant_id='t1' AND source_memory_id='source'`).Scan(&sourceEdges); err != nil || sourceEdges != 1 {
				t.Fatalf("replacement edges=%d err=%v", sourceEdges, err)
			}
			for _, entity := range []string{"mira", "cedar observatory", "lumen receiver"} {
				id := graphDigest(entity)
				if n, err := f.s.rdb.Exists(ctx, heurgraphEntKey("t1", id), heurgraphAdjKey("t1", id)).Result(); err != nil || n != 0 {
					t.Fatalf("replacement retained old cache: %s %d %v", entity, n, err)
				}
			}
			w := lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "source", "expected_revision": 1, "confirm": true})
			if w.Code != 200 || lifecycleJSON(t, w)["cleanup_complete"] != true {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, table := range []string{"graph_entities", "graph_aliases"} {
				var count int
				if err := f.s.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE tenant_id='t1'").Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if shared {
					want = 2
				}
				if count != want {
					t.Fatalf("%s retained %d names; want %d", table, count, want)
				}
			}
			if shared {
				if graphCount(t, f.s.db, "graph_relations") != 1 || graphCount(t, f.s.db, "graph_extractions") != 1 {
					t.Fatal("replacement or forget removed unrelated source")
				}
				var summary string
				if err := f.s.db.QueryRow(`SELECT summary FROM graph_entities WHERE tenant_id='t1' AND id=?`, graphDigest("lumen receiver")).Scan(&summary); err != nil || summary != "" {
					t.Fatalf("shared summary retained old source: %q %v", summary, err)
				}
				id := graphDigest("summit lab")
				if n, err := f.s.rdb.Exists(ctx, heurgraphEntKey("t1", id), heurgraphAdjKey("t1", id)).Result(); err != nil || n != 2 {
					t.Fatal("unrelated cache removed", n, err)
				}
			}
		})
	}
}

func TestGraphArchiveReconcileThenForgetRemovesDerivedNames(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "unique"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecallFixture(t)
			ctx := context.Background()
			graphTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				output := graphTestOutput
				if payload.Messages[1].Content == "Mira uses the Lumen receiver." {
					output = `{"triples":[{"subject":"Mira","predicate":"uses","object":"Lumen receiver","evidence":"Mira uses the Lumen receiver."}]}`
				}
				graphReply(w, output)
			})
			f.store(t, "source", "Mira operates the Cedar observatory.", "a", nil, 1)
			if err := indexGraphSource(ctx, f.s.rdb, f.s.db, "t1", "source"); err != nil {
				t.Fatal(err)
			}
			if shared {
				f.store(t, "retained", "Mira uses the Lumen receiver.", "a", nil, 1)
				if err := indexGraphSource(ctx, f.s.rdb, f.s.db, "t1", "retained"); err != nil {
					t.Fatal(err)
				}
			}
			for _, entity := range []string{"mira", "cedar observatory", "lumen receiver"} {
				if !shared && entity == "lumen receiver" {
					continue
				}
				id := graphDigest(entity)
				if _, err := f.s.db.Exec(`UPDATE graph_entities SET summary='derived source text' WHERE tenant_id='t1' AND id=?`, id); err != nil {
					t.Fatal(err)
				}
				if err := f.s.rdb.HSet(ctx, heurgraphEntKey("t1", id), "summary", "derived source text").Err(); err != nil {
					t.Fatal(err)
				}
				if err := f.s.rdb.ZAdd(ctx, heurgraphAdjKey("t1", id), redis.Z{Score: 1, Member: "derived edge"}).Err(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := moveActiveToVault(ctx, f.s.rdb, "t1", "source"); err != nil {
				t.Fatal(err)
			}
			if err := reconcileGraphTenant(ctx, f.s.rdb, f.s.db, "t1"); err != nil {
				t.Fatal(err)
			}
			w := lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "source", "expected_revision": 1, "confirm": true})
			if w.Code != 200 || lifecycleJSON(t, w)["cleanup_complete"] != true {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, table := range []string{"graph_entities", "graph_aliases"} {
				var count int
				query := "SELECT COUNT(*) FROM " + table + " WHERE tenant_id='t1'"
				if err := f.s.db.QueryRow(query).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if shared {
					want = 2
				}
				if count != want {
					t.Fatalf("%s retained %d names; want %d", table, count, want)
				}
			}
			for _, entity := range []string{"mira", "cedar observatory"} {
				id := graphDigest(entity)
				if n, err := f.s.rdb.Exists(ctx, heurgraphEntKey("t1", id), heurgraphAdjKey("t1", id)).Result(); err != nil || n != 0 {
					t.Fatalf("removed source cache retained: %s %d %v", entity, n, err)
				}
			}
			if shared {
				var summary string
				if err := f.s.db.QueryRow(`SELECT summary FROM graph_entities WHERE tenant_id='t1' AND id=?`, graphDigest("mira")).Scan(&summary); err != nil || summary != "" {
					t.Fatalf("shared summary retained deleted source: %q %v", summary, err)
				}
				if graphCount(t, f.s.db, "graph_relations") != 1 || graphCount(t, f.s.db, "graph_extractions") != 1 {
					t.Fatal("unrelated source removed")
				}
				id := graphDigest("lumen receiver")
				if n, err := f.s.rdb.Exists(ctx, heurgraphEntKey("t1", id), heurgraphAdjKey("t1", id)).Result(); err != nil || n != 2 {
					t.Fatal("unrelated cache removed", n, err)
				}
			}
		})
	}
}

type graphRemovalHook struct{ onDelete func() error }

func (h *graphRemovalHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *graphRemovalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *graphRemovalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "del" {
			if err := h.onDelete(); err != nil {
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func TestGraphRemovalConcurrentExtractionRetriesWithoutLosingMapping(t *testing.T) {
	f := graphFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	f.s.db.SetMaxOpenConns(2)
	f.store(t, "source", "alpha evidence", "a", nil, 1)
	graphTestEdge(t, f, "old", "alpha", "beta", "source")
	var once sync.Once
	var writeErr error
	f.s.rdb.AddHook(&graphRemovalHook{onDelete: func() error {
		once.Do(func() {
			// An extractor commits after cleanup has read its endpoint mapping.
			// WAL permits the writer; cleanup must then fail its stale snapshot
			// instead of deleting new edges with an incomplete endpoint mapping.
			_, writeErr = f.s.db.Exec(`INSERT INTO graph_entities(tenant_id,id,name,type) VALUES('t1','new','new private name','entity');
			INSERT INTO graph_aliases(tenant_id,entity_id,alias) VALUES('t1','new','new private name');
			INSERT INTO graph_relations(tenant_id,id,source_id,target_id,predicate,source_memory_id,src_created_at) VALUES('t1','new-edge','alpha','new','related','source','2026-01-01')`)
		})
		return writeErr
	}})
	if err := f.s.removeMemoryGraph(ctx, "t1", "source"); err == nil {
		t.Fatal("cleanup accepted a stale source-to-entity snapshot")
	}
	if writeErr != nil {
		t.Fatal("concurrent fixture write failed", writeErr)
	}
	if graphCount(t, f.s.db, "graph_relations") != 2 {
		t.Fatal("failed cleanup lost mapping required by retry")
	}
	if err := f.s.removeMemoryGraph(ctx, "t1", "source"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"graph_relations", "graph_extractions", "graph_entities", "graph_aliases"} {
		if graphCount(t, f.s.db, table) != 0 {
			t.Fatalf("retry retained %s", table)
		}
	}
}
