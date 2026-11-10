package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestIdentifierExtractionAndStableRanking(t *testing.T) {
	ids, status := recallQueryIdentifiers(`Find "RX-12.r2", rx-12.R2 and C7M482. Ignore 2026-09-28 and ordinary words.`)
	if status != "applied" || !reflect.DeepEqual(ids, map[string]bool{"rx-12.r2": true, "c7m482": true}) {
		t.Fatalf("identifiers: %v %s", ids, status)
	}
	hits := []SearchResult{{ID: "prefix", Text: "RX-12.r22 C7M4820"}, {ID: "fragments", Text: "RX.12-r2"}, {ID: "exact", Text: "RX-12.r2 RX-12.r2"}, {ID: "two", Text: "C7M482 RX-12.r2"}}
	rerankRecallIdentifiers(hits, ids)
	if hits[0].ID != "two" || hits[1].ID != "exact" || *hits[1].IdentifierMatches != 1 || hits[2].ID != "prefix" || hits[3].ID != "fragments" {
		t.Fatalf("ranking/boundaries/frequency: %+v", hits)
	}
	for _, text := range []string{"ordinary words 2026-09-28 12345", strings.Repeat("a", 129) + "7"} {
		if ids, status := recallQueryIdentifiers(text); len(ids) != 0 || status != "no_identifiers" {
			t.Fatalf("false identifier: %v %s", ids, status)
		}
	}
	if ids, status := recallQueryIdentifiers("a01 a02 a03 a04 a05 a06 a07 a08 a09"); len(ids) != 0 || status != "too_many_identifiers" {
		t.Fatalf("unbounded query: %v %s", ids, status)
	}
	bad := RecallOptions{RerankMode: "unrecognized"}
	if bad.normalize() == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestIdentifierRerankPoolIsolationAndDefault(t *testing.T) {
	f := newRecallFixture(t)
	for i := 0; i < 7; i++ {
		f.store(t, fmt.Sprintf("a-%d", i), "RX.12-r2 phrase", "reader", []string{"visible"}, 1)
	}
	f.store(t, "z-exact", "RX-12.r2 phrase. Detailed archived source with additional explanatory prose.", "reader", []string{"visible"}, .1)
	f.store(t, "forbidden-agent", "RX-12.r2 phrase", "other", []string{"visible"}, 1)
	f.store(t, "forbidden-tag", "RX-12.r2 phrase", "reader", []string{"hidden"}, 1)
	request := map[string]interface{}{"text": "RX-12.r2 phrase", "retrieval_mode": "lexical", "agent_id": "reader", "tags": []string{"visible"}, "limit": 1, "qdrant_candidates": 10}
	baseline, _ := f.search(t, request)
	if baseline[0].ID == "z-exact" || baseline[0].IdentifierMatches != nil {
		t.Fatal("fixture or default changed")
	}
	request["rerank_mode"] = "exact_identifiers"
	beforeCount, err := f.s.rdb.HGet(context.Background(), vecKey("t1", "a-6"), "access_count").Int()
	if err != nil {
		t.Fatal(err)
	}
	hits, diag := f.search(t, request)
	if hits[0].ID != "z-exact" || diag.RerankCandidates != 8 || diag.RerankStatus != "applied" {
		t.Fatalf("promotion/isolation failed: %+v %+v", hits, diag)
	}
	// Hydrated but unreturned records must not be reinforced.
	count, err := f.s.rdb.HGet(context.Background(), vecKey("t1", "a-6"), "access_count").Int()
	if err != nil || count != beforeCount {
		t.Fatalf("extra reinforcement: %d %v", count, err)
	}
	request["qdrant_candidates"] = 2
	hits, diag = f.search(t, request)
	if hits[0].ID == "z-exact" || diag.RerankCandidates != 2 {
		t.Fatal("reranker escaped bounded pool")
	}
	request["rerank_mode"] = "none"
	hits, _ = f.search(t, request)
	if !reflect.DeepEqual(hits, baseline) {
		t.Fatal("disabled changed baseline results")
	}
	request["text"] = "ordinary phrase"
	before, _ := f.search(t, request)
	request["rerank_mode"] = "exact_identifiers"
	after, diag := f.search(t, request)
	if !reflect.DeepEqual(before, after) || diag.RerankStatus != "no_identifiers" {
		t.Fatal("ordinary queries changed")
	}
}

func TestIdentifierRerankRetainsScopeAndSourceCap(t *testing.T) {
	f := newRecallFixture(t)
	for i, kind := range []string{"factual", "factual", "creative", "factual"} {
		parent := "shared"
		if i == 3 {
			parent = "other"
		}
		metadata := map[string]interface{}{"memory_kind": kind, "source": map[string]string{"parent_id": parent}}
		w := f.call(t, "/store", map[string]interface{}{"id": fmt.Sprintf("doc-%d", i), "text": "RX-12.r2", "embedding": []float32{1, 0}, "metadata": metadata})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	hits, _ := f.search(t, map[string]interface{}{"text": "RX-12.r2", "retrieval_mode": "lexical", "rerank_mode": "exact_identifiers", "memory_kind": "factual", "max_per_source": 1, "limit": 5})
	if len(hits) != 2 || hits[0].ID != "doc-0" || hits[1].ID != "doc-3" {
		t.Fatalf("source cap/scope: %+v", hits)
	}
}
