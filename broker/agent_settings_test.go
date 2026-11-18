package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestVisibleToRequester(t *testing.T) {
	iso := map[string]bool{"alpha": true} // alpha's corpus is isolated

	cases := []struct {
		name              string
		requester, author string
		visibility, agent string
		want              bool
	}{
		{"legacy doc visible to all", "beta", "", "", "", true},
		{"legacy doc visible even to empty requester's peer", "x", "", "private", "", true},
		{"own private doc", "alpha", "alpha", "private", "", true},
		{"isolated author hidden from other", "beta", "alpha", "private", "", false},
		{"isolated author's own read allowed", "alpha", "alpha", "private", "", true},
		{"non-isolated author shared (cooperative default)", "alpha", "beta", "private", "", true},
		{"explicit tenant share overrides isolation", "beta", "alpha", "tenant", "", true},
		{"explicit agents share includes requester", "beta", "alpha", "agents", `["beta"]`, true},
		{"explicit agents share excludes other", "gamma", "alpha", "agents", `["beta"]`, false},
	}
	for _, c := range cases {
		if got := visibleToRequester(c.requester, c.author, c.visibility, c.agent, iso); got != c.want {
			t.Errorf("%s: visibleToRequester(%q,%q,%q,%q) = %v, want %v",
				c.name, c.requester, c.author, c.visibility, c.agent, got, c.want)
		}
	}
}

func TestAuthorFieldLegacyFallback(t *testing.T) {
	if got := authorField(map[string]string{"created_by": "alpha", "agent_id": "beta"}); got != "alpha" {
		t.Errorf("created_by should win, got %q", got)
	}
	if got := authorField(map[string]string{"agent_id": "alpha"}); got != "alpha" {
		t.Errorf("agent_id should supply the author, got %q", got)
	}
	if got := authorField(nil); got != "" {
		t.Errorf("authorless record should have an empty author, got %q", got)
	}
}

// The flag must default to OFF. crossAgentEnabled reads a map key on master
// (the branch this was ported from used a typed struct field), and a missing
// key must read as false — otherwise a deployment with no `features:` block
// would silently start filtering reads.
func TestCrossAgentEnabledDefaultsOff(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if crossAgentEnabled(cfg) {
		t.Error("cross_agent_visibility must default to false on an empty config")
	}

	cfg, err = loadConfig(writeCfg(t, "features:\n  cross_agent_visibility: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !crossAgentEnabled(cfg) {
		t.Error("explicit cross_agent_visibility: true was not honoured")
	}

	// An unrelated flag must not switch it on.
	cfg, err = loadConfig(writeCfg(t, "features:\n  something_else: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if crossAgentEnabled(cfg) {
		t.Error("an unrelated feature flag enabled cross-agent visibility")
	}
}

func TestQueryUsesStoredAgentForVisibility(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "alpha-memory", "from alpha", "alpha", []string{"fact"}, 1)
	f.store(t, "beta-memory", "from beta", "beta", []string{"note"}, 1)
	f.store(t, "anonymous", "no author", "", []string{"fact"}, 1)
	f.store(t, "excluded", "unmatched tag", "beta", []string{"other"}, 1)

	call := func(method, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-HeurChain-API-Key", "test-key")
		req.Header.Set("X-HeurChain-Tenant", "t1")
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	checkQuery := func(agent string, want ...string) {
		t.Helper()
		body := fmt.Sprintf(`{"embedding":[1,1],"agent_id":%q,"tags":["fact","note"]}`, agent)
		w := call(http.MethodPost, "/query", body, f.s.handleQuery)
		var reply struct{ Results []searchResult }
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(reply.Results))
		for _, result := range reply.Results {
			ids = append(ids, result.ID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, want) {
			t.Fatalf("query as %q returned %v, want %v", agent, ids, want)
		}
	}

	checkQuery("beta", "beta-memory")
	f.s.config.Features = map[string]bool{"cross_agent_visibility": true}
	checkQuery("beta", "alpha-memory", "anonymous", "beta-memory")
	call(http.MethodPut, "/agent-settings", `{"agent_id":"alpha","isolated":true}`, f.s.handleAgentSettings)
	checkQuery("beta", "anonymous", "beta-memory")
	checkQuery("alpha", "alpha-memory", "anonymous", "beta-memory")
	checkQuery("", "alpha-memory", "anonymous", "beta-memory")
}
