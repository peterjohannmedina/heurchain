package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sendVaultMCP(t *testing.T, s *Server, tenant, method string, params any, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp/sse", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-HeurChain-API-Key", "test-key")
	req.Header.Set("X-HeurChain-Tenant", tenant)
	w := httptest.NewRecorder()
	s.handleMCPSSE(w, req)
	if w.Code != wantStatus {
		t.Fatalf("%s: HTTP %d, want %d: %s", method, w.Code, wantStatus, w.Body.String())
	}
	return w
}

func callVaultMCP(t *testing.T, s *Server, tool string, args map[string]any, wantError bool) string {
	t.Helper()
	w := sendVaultMCP(t, s, "t1", "tools/call", map[string]any{"name": tool, "arguments": args}, http.StatusOK)
	var response struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Result == nil || len(response.Result.Content) != 1 || response.Result.IsError != wantError {
		t.Fatalf("tool %s: want isError=%v: %s", tool, wantError, w.Body.String())
	}
	return response.Result.Content[0].Text
}

func requireMemoryKey(t *testing.T, s *Server, key string, want int64) {
	t.Helper()
	got, err := s.rdb.Exists(context.Background(), key).Result()
	if err != nil || got != want {
		t.Fatalf("key %q: exists=%d, want %d (error %v)", key, got, want, err)
	}
}

func TestMCPVaultRehydrateOptInAndTenantScope(t *testing.T) {
	f := newRecallFixture(t)
	const tool = "heurchain_vault_rehydrate"
	const id = "archive-1"
	const archivedText = "amber telescope archived record"
	ctx := context.Background()
	f.store(t, id, archivedText, "reader", []string{"fixture"}, 1)
	if err := vaultDoc(ctx, f.s.rdb, "t1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ('t2', 'Other fixture', 'other@example.test')`); err != nil {
		t.Fatal(err)
	}
	mine, foreign := memoryVaultKey("t1", id), memoryVaultKey("t2", id)
	if err := f.s.rdb.HSet(ctx, foreign, "text", "unrelated archived record").Err(); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"doc_id": id}
	list := func() string { return sendVaultMCP(t, f.s, "t1", "tools/list", nil, http.StatusOK).Body.String() }
	if strings.Contains(list(), tool) {
		t.Fatal("restore listed without broker opt-in")
	}
	callVaultMCP(t, f.s, tool, args, true)
	requireMemoryKey(t, f.s, mine, 1)
	requireMemoryKey(t, f.s, vecKey("t1", id), 0)

	f.s.config.Features = map[string]bool{"mcp_vault_rehydrate": true}
	if !strings.Contains(list(), tool) {
		t.Fatal("opted-in restore not listed")
	}
	sendVaultMCP(t, f.s, "t2", "tools/call", map[string]any{"name": tool, "arguments": args}, http.StatusUnauthorized)
	callVaultMCP(t, f.s, tool, map[string]any{"doc_id": id, "tenant_id": "t2"}, true)
	requireMemoryKey(t, f.s, mine, 1)
	requireMemoryKey(t, f.s, foreign, 1)

	before, _ := f.search(t, map[string]any{"text": archivedText})
	if len(before) != 0 {
		t.Fatalf("archived record still searchable: %+v", before)
	}
	receipt := callVaultMCP(t, f.s, tool, args, false)
	var restored struct {
		Rehydrated bool   `json:"rehydrated"`
		DocID      string `json:"doc_id"`
	}
	if err := json.Unmarshal([]byte(receipt), &restored); err != nil || !restored.Rehydrated || restored.DocID != id {
		t.Fatalf("restore receipt: %s", receipt)
	}
	requireMemoryKey(t, f.s, mine, 0)
	requireMemoryKey(t, f.s, foreign, 1)
	requireMemoryKey(t, f.s, vecKey("t2", id), 0)
	after, _ := f.search(t, map[string]any{"text": archivedText})
	if len(after) != 1 || after[0].ID != id || after[0].Text != archivedText || after[0].QdrantRank != 1 || after[0].BM25Rank != 1 {
		t.Fatalf("record not restored to both search indexes: %+v", after)
	}
}

func TestMCPVaultRehydrateRetainsSourceDuringIndexOutage(t *testing.T) {
	f := newRecallFixture(t)
	const id = "retry-archive"
	f.s.config.Features = map[string]bool{"mcp_vault_rehydrate": true}
	f.store(t, id, "memory with an unavailable projection", "reader", nil, 1)
	if err := vaultDoc(context.Background(), f.s.rdb, "t1", id); err != nil {
		t.Fatal(err)
	}
	f.fail.Store(true)
	args := map[string]any{"doc_id": id}
	callVaultMCP(t, f.s, "heurchain_vault_rehydrate", args, true)
	requireMemoryKey(t, f.s, memoryVaultKey("t1", id), 1)
	f.fail.Store(false)
	callVaultMCP(t, f.s, "heurchain_vault_rehydrate", args, false)
	requireMemoryKey(t, f.s, memoryVaultKey("t1", id), 0)
}

func TestMCPPromoteArchivesWithoutEnablingRestore(t *testing.T) {
	f := newRecallFixture(t)
	const id = "low-activation-record"
	ctx := context.Background()
	oldConfig := consolidationCfg
	consolidationCfg.PromoteLive = false
	consolidationCfg.PromotionThreshold = 100
	t.Cleanup(func() { consolidationCfg = oldConfig })
	f.store(t, id, "old memory to archive", "reader", nil, 1)
	text := callVaultMCP(t, f.s, "heurchain_promote", map[string]any{"id": id}, false)
	var receipt struct {
		Promoted bool `json:"promoted"`
	}
	if err := json.Unmarshal([]byte(text), &receipt); err != nil || !receipt.Promoted {
		t.Fatalf("promote receipt: %s", text)
	}
	requireMemoryKey(t, f.s, vecKey("t1", id), 0)
	requireMemoryKey(t, f.s, memoryVaultKey("t1", id), 1)
	if got, err := f.s.rdb.HGet(ctx, memoryVaultKey("t1", id), "text").Result(); err != nil || got != "old memory to archive" {
		t.Fatalf("archive text not preserved: %q (%v)", got, err)
	}
}
