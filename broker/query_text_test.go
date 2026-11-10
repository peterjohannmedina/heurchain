package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPTextQueryRetrievesStoredMemory(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "wanted", "verified docking procedure", "codex", []string{"procedure"}, 1)
	f.store(t, "other-agent", "verified docking procedure", "claude", []string{"procedure"}, 1)
	f.store(t, "other-tag", "verified docking procedure", "codex", []string{"unrelated"}, 1)
	session := &mcpSession{apiKey: "test-key", tenantID: "t1"}
	data, err := f.s.invokeMCPTool(context.Background(), session, "heurchain_query",
		json.RawMessage(`{"text":"how to dock","agent_id":"codex","tags":["procedure"],"top_k":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Results []searchResult }
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].ID != "wanted" || response.Results[0].Score <= 0 {
		t.Fatalf("text query lost vector retrieval or filters: %s", data)
	}
}

func TestQueryEmbeddingValidationAndCompatibility(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "wanted", "source", "codex", nil, 1)
	var calls atomic.Int32
	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "private upstream failure", http.StatusServiceUnavailable)
	}))
	defer embed.Close()
	f.s.config.Embed.OllamaURL = embed.URL
	for _, tc := range []struct {
		name, key, body        string
		status, embeddingCalls int
	}{
		{"client-vector", "test-key", `{"embedding":[1,0],"text":"optional lexical text","top_k":1}`, 200, 0},
		{"unauthorized", "wrong-key", `{"text":"query"}`, 401, 0},
		{"empty", "test-key", `{"text":"  "}`, 400, 0},
		{"unknown-field", "test-key", `{"text":"query","unexpected":true}`, 400, 0},
		{"bad-agent", "test-key", `{"text":"query","agent_id":"bad agent"}`, 400, 0},
		{"provider-down", "test-key", `{"text":"query"}`, 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(tc.body))
			r.Header.Set("X-HeurChain-API-Key", tc.key)
			r.Header.Set("X-HeurChain-Tenant", "t1")
			w := httptest.NewRecorder()
			f.s.handleQuery(w, r)
			if w.Code != tc.status || int(calls.Load()-before) != tc.embeddingCalls {
				t.Fatalf("status=%d embedding calls=%d: %s", w.Code, calls.Load()-before, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private upstream") {
				t.Fatal("provider error leaked")
			}
			if tc.name == "client-vector" && !strings.Contains(w.Body.String(), `"id":"wanted"`) {
				t.Fatal("explicit embedding no longer retrieves stored memory")
			}
		})
	}
}
