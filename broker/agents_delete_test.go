package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeleteAgentPreservesAuditHistory(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "corrected-memory", "old text", "writer", nil, 1)
	w := lifecycleCall(t, f, f.s.handleMemoryUpdate, map[string]interface{}{
		"id": "corrected-memory", "expected_revision": 1, "text": "new text", "reason": "correction",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	w = lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{
		"id": "corrected-memory", "expected_revision": 2, "confirm": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", w.Code, w.Body)
	}
	if _, err := f.s.db.Exec(`INSERT INTO audit_log (tenant_id, actor, action)
		VALUES ('t1', 'admin', 'vault.export'), ('t1', 'legacy', 'memory.update')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO usage_events (tenant_id, event_type) VALUES ('t1', 'flush')`); err != nil {
		t.Fatal(err)
	}

	checkCount := func(want int, query string, args ...interface{}) {
		t.Helper()
		var got int
		if err := f.s.db.QueryRow(query, args...).Scan(&got); err != nil || got != want {
			t.Fatalf("count for %q = %d, %v; want %d", query, got, err, want)
		}
	}
	remove := func(agent string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodDelete, "/agents?tenant_id=t1&agent_id="+agent, nil)
		r.Header.Set("X-HeurChain-API-Key", "test-key")
		w := httptest.NewRecorder()
		f.s.handleAgents(w, r)
		return w
	}

	for _, agent := range []string{"unspecified", "writer"} {
		w := remove(agent)
		var receipt struct {
			Removed     bool `json:"removed"`
			UsageEvents int  `json:"usage_events_deleted"`
			AuditEvents *int `json:"audit_log_deleted"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || w.Code != http.StatusOK ||
			!receipt.Removed || receipt.UsageEvents != 1 || receipt.AuditEvents == nil || *receipt.AuditEvents != 0 {
			t.Fatalf("delete agent %s: %d %s", agent, w.Code, w.Body)
		}
		checkCount(1, `SELECT COUNT(*) FROM audit_log
			WHERE tenant_id = 't1' AND actor = 'apikey:t1' AND action = 'agent.usage_delete' AND resource = ?`, "agent:"+agent)
	}
	checkCount(0, `SELECT COUNT(*) FROM usage_events WHERE tenant_id = 't1'`)
	checkCount(3, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = 't1' AND agent_id = 'writer'
		AND action IN ('store', 'memory.update', 'memory.forget')`)
	checkCount(2, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = 't1' AND agent_id IS NULL
		AND action IN ('vault.export', 'memory.update')`)

	if _, err := f.s.db.Exec(`INSERT INTO usage_events (tenant_id, event_type, agent_id) VALUES ('t1', 'store', 'writer');
		CREATE TRIGGER reject_delete_audit BEFORE INSERT ON audit_log WHEN NEW.action = 'agent.usage_delete'
		BEGIN SELECT RAISE(ABORT, 'fixture refused audit'); END`); err != nil {
		t.Fatal(err)
	}
	if w := remove("writer"); w.Code != http.StatusInternalServerError {
		t.Fatalf("delete with unavailable audit: %d %s", w.Code, w.Body)
	}
	checkCount(1, `SELECT COUNT(*) FROM usage_events WHERE tenant_id = 't1' AND agent_id = 'writer'`)
}
