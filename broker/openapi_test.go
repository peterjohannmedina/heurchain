package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestOpenAPI(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	(&Server{}).handleOpenAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// The spec contains UTF-8 punctuation (em dashes in descriptions). Without a
	// charset Safari falls back to Latin-1 and renders them as mojibake.
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	paths := doc["paths"].(map[string]interface{})
	for _, path := range []string{"/store", "/search", "/agent/{agent_id}", "/session/start", "/graph/ask", "/admin/vault/rehydrate"} {
		if _, ok := paths[path]; !ok {
			t.Errorf("missing path %s", path)
		}
	}

	// Every $ref must resolve — a dangling ref makes the whole document
	// unusable to a client generator, and nothing else here would catch it.
	components := doc["components"].(map[string]interface{})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(
		`"\$ref":\s*"#/components/([a-zA-Z]+)/([a-zA-Z]+)"`).FindAllStringSubmatch(string(raw), -1) {
		group, ok := components[m[1]].(map[string]interface{})
		if !ok {
			t.Errorf("$ref group %q does not exist", m[1])
			continue
		}
		if _, ok := group[m[2]]; !ok {
			t.Errorf("dangling $ref: #/components/%s/%s", m[1], m[2])
		}
	}

	// namespace is the ACT-R decay tier and changes how long a memory stays
	// retrievable, so callers need it documented with its legal values.
	store := components["schemas"].(map[string]interface{})["StoreRequest"].(map[string]interface{})
	ns, ok := store["properties"].(map[string]interface{})["namespace"].(map[string]interface{})
	if !ok {
		t.Fatal("StoreRequest is missing the namespace property")
	}
	if got := ns["enum"]; got == nil {
		t.Error("namespace should enumerate its legal decay tiers")
	}
	schemes := components["securitySchemes"].(map[string]interface{})
	for _, name := range []string{"HeurChainAPIKey", "HeurChainTenant", "AdminBasicAuth"} {
		if _, ok := schemes[name]; !ok {
			t.Errorf("missing security scheme %s", name)
		}
	}
}

func TestParseAgentPath(t *testing.T) {
	tests := []struct {
		path   string
		agent  string
		recall bool
		ok     bool
	}{
		{"/agent/codex", "codex", false, true},
		{"/agent/codex/recall", "codex", true, true},
		{"/agent/codex/", "", false, false},
		{"/agent/codex/unknown", "", false, false},
		{"/agent/bad id", "", false, false},
	}
	for _, tt := range tests {
		agent, recall, ok := parseAgentPath(tt.path)
		if agent != tt.agent || recall != tt.recall || ok != tt.ok {
			t.Errorf("parseAgentPath(%q) = (%q, %v, %v), want (%q, %v, %v)", tt.path, agent, recall, ok, tt.agent, tt.recall, tt.ok)
		}
	}
}
