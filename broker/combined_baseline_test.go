package main

import (
	"encoding/json"
	"os"
	"testing"
)

// This unchanged scenario also runs in detached checkouts of master and each
// constituent PR. Only stable public behavior is compared: timing/query IDs,
// storage timestamps and provider diagnostics are intentionally excluded.
func TestCombinedBaselineSnapshot(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "source-a", "alpha calibration", "reader", []string{"fact"}, 1)
	f.store(t, "source-b", "alpha handbook", "reader", []string{"fact"}, .8)
	f.store(t, "source-c", "unrelated passage", "other", []string{"ops"}, .5)
	snapshot := map[string]interface{}{}
	search := func(name string, request map[string]interface{}) {
		hits, _ := f.search(t, request)
		rows := []map[string]interface{}{}
		for _, h := range hits {
			rows = append(rows, map[string]interface{}{"id": h.ID, "text": h.Text, "agent_id": h.AgentID, "tags": h.Tags, "bm25_rank": h.BM25Rank, "dense_rank": h.QdrantRank, "rrf_score": h.RRFScore})
		}
		snapshot[name] = rows
	}
	search("default_search", map[string]interface{}{"text": "alpha", "limit": 3})
	search("filtered_search", map[string]interface{}{"text": "alpha", "agent_id": "reader", "tags": []string{"fact"}, "limit": 3})
	w := lifecycleCall(t, f, f.s.handleMemoryUpdate, map[string]interface{}{"id": "source-a", "expected_revision": 1, "text": "alpha revised", "reason": "comparison correction"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	snapshot["updated_revision"] = lifecycleJSON(t, w)["revision"]
	search("corrected_search", map[string]interface{}{"text": "alpha", "limit": 3})
	w = lifecycleCall(t, f, f.s.handleMemoryGet, map[string]interface{}{"id": "source-a", "revision": 1})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	snapshot["historical_text"] = lifecycleJSON(t, w)["memory"].(map[string]interface{})["text"]
	w = lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{"id": "source-a", "expected_revision": 2, "confirm": true})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	snapshot["forgotten"] = lifecycleJSON(t, w)["forgotten"]
	search("after_forget", map[string]interface{}{"text": "alpha", "limit": 3})
	w = lifecycleCall(t, f, f.s.handleMemoryCheckpoint, map[string]interface{}{"id": "comparison-checkpoint", "agent_id": "codex", "summary": "stable handoff"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryResume, map[string]interface{}{"id": "comparison-checkpoint"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	snapshot["checkpoint_summary"] = lifecycleJSON(t, w)["checkpoint"].(map[string]interface{})["summary"]
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("HEURCHAIN_COMPARISON_OUTPUT"); output != "" {
		if err := os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
