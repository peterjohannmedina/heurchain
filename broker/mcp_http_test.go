package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPHTTPTransportAndAPIKey(t *testing.T) {
	f := newRecallFixture(t)
	send := func(body, version string, bearer bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp/sse", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", version)
		if bearer {
			// Self-hosted clients may present the broker API key as a Bearer token.
			r.Header.Set("Authorization", "Bearer test-key")
		} else {
			r.Header.Set("X-HeurChain-API-Key", "test-key")
		}
		r.Header.Set("X-HeurChain-Tenant", "t1")
		w := httptest.NewRecorder()
		f.s.handleMCPSSE(w, r)
		return w
	}
	for _, bearer := range []bool{false, true} {
		for _, v := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
			w := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+v+`"}}`, "", bearer)
			if w.Code != 200 || !strings.Contains(w.Body.String(), v) {
				t.Fatalf("initialize: %d %s", w.Code, w.Body)
			}
		}
		w := send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "2025-11-25", bearer)
		if w.Code != 202 || w.Body.Len() != 0 {
			t.Fatalf("notification: %d %s", w.Code, w.Body)
		}
		w = send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "2025-11-25", bearer)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "heurchain_search") || !strings.Contains(w.Body.String(), "heurchain_store") {
			t.Fatalf("tools: %s", w.Body)
		}
	}
	w := send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"heurchain_store","arguments":{"text":"HTTP synthetic memory","agent_id":"gemini"}}}`, "", true)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("write: %s", w.Body)
	}
	w = send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"heurchain_search","arguments":{"text":"HTTP synthetic memory"}}}`, "", true)
	if !strings.Contains(w.Body.String(), "HTTP synthetic memory") {
		t.Fatalf("search: %s", w.Body)
	}
	for _, body := range []string{`{`, `[]`, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, `{"jsonrpc":"2.0","id":1}`} {
		if w = send(body, "", true); w.Code != 400 {
			t.Fatalf("malformed accepted: %s %d", body, w.Code)
		}
	}
	if w = send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "2099-01-01", true); w.Code != 400 {
		t.Fatal("unsupported protocol header accepted")
	}
	w = send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":42}`, "", true)
	var result rpcResp
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Error == nil || result.Error.Code != -32602 {
		t.Fatal("malformed initialization accepted")
	}
	w = send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"unknown"}}`, "", true)
	if !strings.Contains(w.Body.String(), "2025-11-25") {
		t.Fatal("unsupported initialize version not negotiated")
	}
	if w = send(`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`, "", true); w.Code != 200 {
		t.Fatalf("bearer key rejected: %d %s", w.Code, w.Body)
	}
	if _, err := f.s.db.Exec(`UPDATE tenants SET status='suspended' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	if w = send(`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`, "", true); w.Code != 403 {
		t.Fatal("suspended tenant accepted")
	}
}

func TestMCPBrowserTransportHeaders(t *testing.T) {
	f := newRecallFixture(t)
	for _, path := range []string{"/mcp/sse", "/mcp/message"} {
		r := httptest.NewRequest("OPTIONS", path, nil)
		r.Header.Set("Origin", "http://example.com")
		w := httptest.NewRecorder()
		if path == "/mcp/sse" {
			f.s.handleMCPSSE(w, r)
		} else {
			f.s.handleMCPMessage(w, r)
		}
		if w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "MCP-Protocol-Version") {
			t.Fatal("protocol preflight missing")
		}
		r = httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "http://attacker.example")
		w = httptest.NewRecorder()
		if path == "/mcp/sse" {
			f.s.handleMCPSSE(w, r)
		} else {
			f.s.handleMCPMessage(w, r)
		}
		if w.Code != 403 {
			t.Fatal("untrusted Origin accepted")
		}
	}
	w := httptest.NewRecorder()
	f.s.handleMCPSSE(w, httptest.NewRequest("GET", "/mcp/sse", nil))
	if w.Code != 401 || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "WWW-Authenticate") {
		t.Fatal("challenge not browser-visible")
	}
}

func TestMCPLegacyAPIKeySSESmoke(t *testing.T) {
	f := newRecallFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/sse", f.s.handleMCPSSE)
	mux.HandleFunc("/mcp/message", f.s.handleMCPMessage)
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/mcp/sse", nil)
	req.Header.Set("X-HeurChain-API-Key", "test-key")
	req.Header.Set("X-HeurChain-Tenant", "t1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("SSE connect: %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	reader.ReadString('\n')
	endpoint, _ := reader.ReadString('\n')
	reader.ReadString('\n')
	target := server.URL + strings.TrimSpace(strings.TrimPrefix(endpoint, "data: "))
	for _, method := range []string{"initialize", "tools/list", "tools/call"} {
		params := "{}"
		if method == "tools/call" {
			params = `{"name":"heurchain_manifest","arguments":{}}`
		}
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		post, _ := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(body))
		post.Header.Set("X-HeurChain-API-Key", "test-key")
		post.Header.Set("X-HeurChain-Tenant", "t1")
		ack, err := http.DefaultClient.Do(post)
		if err != nil {
			t.Fatal(err)
		}
		ack.Body.Close()
		if ack.StatusCode != 202 {
			t.Fatalf("%s: %d", method, ack.StatusCode)
		}
		reader.ReadString('\n')
		event, err := reader.ReadString('\n')
		reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(event, `"result"`) || strings.Contains(event, `"isError":true`) {
			t.Fatalf("%s: %s", method, event)
		}
	}
}
