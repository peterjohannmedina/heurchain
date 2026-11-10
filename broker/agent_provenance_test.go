package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// provenanceFixture builds a server with one tenant, an audit/usage trail for
// agent "codex", and a cached API key, so the handler can be exercised on the
// same auth path the MCP surface uses. Timestamps are relative to now because
// the handler only reports the last 90 days.
func provenanceFixture(t *testing.T) *Server {
	t.Helper()
	s := newExportTestServer(t)
	ctx := context.Background()

	if _, err := s.db.Exec(`INSERT INTO tenants (id, name, email) VALUES
		('t1', 'Provenance fixture', 'prov@example.test'),
		('t2', 'Other tenant', 'other@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO api_keys (id, tenant_id, key_hash, name) VALUES (1, 't1', 'unused', 'laptop')`); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	at := func(hoursAgo int) string {
		return now.Add(-time.Duration(hoursAgo) * time.Hour).Format("2006-01-02T15:04:05Z")
	}

	// Two codex actions from one machine, one from another, plus noise from a
	// different agent and a different tenant that must not leak into the reply.
	rows := []struct {
		tenant, actor, agent, ip, ua, at string
	}{
		{"t1", "1", "codex", "203.0.113.7", "claude-code/1.0", at(48)},
		{"t1", "1", "codex", "203.0.113.7", "claude-code/1.0", at(24)},
		{"t1", "system", "codex", "198.51.100.9", "curl/8", at(12)},
		{"t1", "1", "other", "203.0.113.7", "claude-code/1.0", at(6)},
		{"t2", "1", "codex", "192.0.2.1", "elsewhere", at(5)},
	}
	for _, r := range rows {
		if _, err := s.db.Exec(
			`INSERT INTO audit_log (tenant_id, actor, action, ip_address, user_agent, success, agent_id, created_at)
			 VALUES (?, ?, 'store', ?, ?, 1, ?, ?)`,
			r.tenant, r.actor, r.ip, r.ua, r.agent, r.at,
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct{ event, at string }{
		{"store", at(48)},
		{"store", at(24)},
		{"search", at(12)},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO usage_events (tenant_id, event_type, agent_id, created_at) VALUES ('t1', ?, 'codex', ?)`,
			r.event, r.at,
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.rdb.HSet(ctx, agentCfgKey("t1", "codex"), map[string]interface{}{
		"display_name": "Laptop assistant",
		"note":         "runs on the workstation",
		"isolated":     "1",
	}).Err(); err != nil {
		t.Fatal(err)
	}
	s.config.Features = map[string]bool{"cross_agent_visibility": true}

	auth, _ := json.Marshal(cachedAuth{KeyID: 1, TenantID: "t1", Epoch: 0})
	if err := s.rdb.Set(ctx, authCacheKey("test-key"), auth, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	return s
}

// provenanceRequestAuth sends an authenticated request through the same
// tenantStatusGuard wrapper main.go registers, so the tests exercise the
// guard's tenant-header requirement too.
func provenanceRequestAuth(s *Server, tenantID, agentID string, auth func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/agent-provenance?tenant_id="+url.QueryEscape(tenantID)+"&agent_id="+url.QueryEscape(agentID), nil)
	req.Header.Set(s.config.Auth.TenantHeader, tenantID)
	if auth != nil {
		auth(req)
	}
	rec := httptest.NewRecorder()
	s.tenantStatusGuard(s.handleAgentProvenance)(rec, req)
	return rec
}

func provenanceRequest(s *Server, tenantID, agentID, apiKey string) *httptest.ResponseRecorder {
	return provenanceRequestAuth(s, tenantID, agentID, func(req *http.Request) {
		if apiKey != "" {
			req.Header.Set(s.config.Auth.APIKeyHeader, apiKey)
		}
	})
}

type provenanceReply struct {
	AgentID      string `json:"agent_id"`
	WindowDays   int    `json:"window_days"`
	DisplayName  string `json:"display_name"`
	Note         string `json:"note"`
	Isolated     bool   `json:"isolated"`
	Visibility   bool   `json:"visibility_enabled"`
	FirstSeen    string `json:"first_seen"`
	LastSeen     string `json:"last_seen"`
	TotalActions int    `json:"total_actions"`
	SourceIPs    []struct {
		IP       string `json:"ip"`
		Count    int    `json:"count"`
		LastSeen string `json:"last_seen"`
	} `json:"source_ips"`
	UserAgents []provCountRow `json:"user_agents"`
	APIKeys    []provCountRow `json:"api_keys"`
	EventTypes []provCountRow `json:"event_types"`
}

func decodeProvenance(t *testing.T, rec *httptest.ResponseRecorder) provenanceReply {
	t.Helper()
	var got provenanceReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	return got
}

func TestAgentProvenanceRequiresAuth(t *testing.T) {
	s := provenanceFixture(t)
	rec := provenanceRequest(s, "t1", "codex", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestAgentProvenanceRefusesWrongTenant(t *testing.T) {
	s := provenanceFixture(t)
	// The cached key belongs to t1; asking for t2's provenance must not be
	// authorised by it (validateAPIKey scopes the key to the requested tenant).
	rec := provenanceRequest(s, "t2", "codex", "test-key")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-tenant status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestAgentProvenanceRejectsInvalidAgentID(t *testing.T) {
	s := provenanceFixture(t)
	rec := provenanceRequest(s, "t1", "bad id", "test-key")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid agent_id status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// tenantStatusGuard checks the tenant header; resolveVaultCaller checks the
// tenant_id query parameter. A mismatch would let a suspended tenant put an
// active tenant's id in the header and still read its own data.
func TestAgentProvenanceRejectsTenantHeaderMismatch(t *testing.T) {
	s := provenanceFixture(t)
	// Close the database first: the guard needs it, so this test calls the
	// handler directly. Any query in the handler would fail (and the handler
	// answers 200 with empty sections), so a 400 proves the mismatch is
	// rejected before authentication or any database work.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/agent-provenance?tenant_id=t1&agent_id=codex", nil)
	req.Header.Set(s.config.Auth.TenantHeader, "t2")
	req.Header.Set(s.config.Auth.APIKeyHeader, "test-key")
	rec := httptest.NewRecorder()
	s.handleAgentProvenance(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched header status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "tenant header does not match tenant_id") {
		t.Errorf("body = %s, want the mismatch error", body)
	}
}

// The status guard must run on the tenant the query names, not merely on some
// active tenant's header: a suspended tenant naming itself is refused before
// the handler executes.
func TestAgentProvenanceSuspendedTenantRefusedByGuard(t *testing.T) {
	s := provenanceFixture(t)
	if _, err := s.db.Exec(`UPDATE tenants SET status='suspended' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	rec := provenanceRequest(s, "t1", "codex", "test-key")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("suspended tenant status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error        string `json:"error"`
		TenantStatus string `json:"tenant_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if body.TenantStatus != "suspended" {
		t.Errorf("tenant_status = %q, want suspended (body: %s)", body.TenantStatus, rec.Body.String())
	}
}

func TestAgentProvenanceHappyPath(t *testing.T) {
	s := provenanceFixture(t)
	rec := provenanceRequest(s, "t1", "codex", "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got := decodeProvenance(t, rec)

	// The fields renderProvenance() in public/dashboard.html reads.
	if got.AgentID != "codex" {
		t.Errorf("agent_id = %q, want codex", got.AgentID)
	}
	if got.WindowDays != 90 {
		t.Errorf("window_days = %d, want 90", got.WindowDays)
	}
	if got.DisplayName != "Laptop assistant" || got.Note != "runs on the workstation" || !got.Isolated || !got.Visibility {
		t.Errorf("disclosed settings = %+v, want the seeded name/note/isolated with visibility on", got)
	}
	if got.FirstSeen == "" || got.LastSeen == "" || got.FirstSeen >= got.LastSeen {
		t.Errorf("first/last seen = %q/%q, want increasing timestamps", got.FirstSeen, got.LastSeen)
	}
	if got.TotalActions != 3 {
		t.Errorf("total_actions = %d, want 3 (other agent and tenant excluded)", got.TotalActions)
	}
	if len(got.SourceIPs) != 2 {
		t.Fatalf("source_ips = %+v, want two distinct addresses", got.SourceIPs)
	}
	if got.SourceIPs[0].IP != "203.0.113.7" || got.SourceIPs[0].Count != 2 {
		t.Errorf("top source IP = %+v, want 203.0.113.7 x2", got.SourceIPs[0])
	}
	if got.SourceIPs[0].LastSeen == "" || got.SourceIPs[0].LastSeen > got.LastSeen {
		t.Errorf("top source IP last_seen = %q, overall last_seen = %q", got.SourceIPs[0].LastSeen, got.LastSeen)
	}
	if len(got.UserAgents) != 2 {
		t.Errorf("user_agents = %+v, want two entries", got.UserAgents)
	}
	if len(got.APIKeys) != 2 {
		t.Fatalf("api_keys = %+v, want key-name and system entries", got.APIKeys)
	}
	if got.APIKeys[0].Label != "laptop" || got.APIKeys[0].Count != 2 {
		t.Errorf("top api key = %+v, want laptop x2", got.APIKeys[0])
	}
	if len(got.EventTypes) != 2 || got.EventTypes[0].Label != "store" || got.EventTypes[0].Count != 2 {
		t.Errorf("event_types = %+v, want store x2 then search", got.EventTypes)
	}
}

// TestProvTimeNormalise pins the accepted layouts and the fallback. SQLite's
// CURRENT_TIMESTAMP text ("2026-11-07 04:00:00") is UTC with no offset, and
// new Date() in the dashboard would read it as local time, so it must come out
// as RFC3339 with a Z suffix. Anything unparseable is returned unchanged.
func TestProvTimeNormalise(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"sqlite current timestamp", "2026-11-07 04:00:00", "2026-11-07T04:00:00Z"},
		{"rfc3339 zulu", "2026-11-07T04:00:00Z", "2026-11-07T04:00:00Z"},
		{"rfc3339 offset", "2026-11-07T06:00:00+02:00", "2026-11-07T04:00:00Z"},
		{"rfc3339 nanosecond", "2026-11-07T04:00:00.123456789Z", "2026-11-07T04:00:00Z"},
		{"empty stays empty", "", ""},
		{"unparsable returned unchanged", "not-a-time", "not-a-time"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &provTimeNormaliser{}
			if got := n.normalise(c.in); got != c.want {
				t.Fatalf("normalise(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// The warning is emitted once per request, not once per bad row.
	n := &provTimeNormaliser{}
	n.normalise("not-a-time")
	if !n.warned {
		t.Error("first unparsable timestamp did not set the warn flag")
	}
}

// The handler must normalise the timestamps it reads back out of audit_log,
// including the per-IP aggregate. The value is computed rather than hard-coded
// so the row stays inside the 90-day window; the literal from production
// ("2026-11-07 04:00:00") is covered by TestProvTimeNormalise.
func TestAgentProvenanceNormalisesSQLiteTimestamps(t *testing.T) {
	s := provenanceFixture(t)
	seen := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	sqliteText := seen.Format("2006-01-02 15:04:05")
	if _, err := s.db.Exec(
		`INSERT INTO audit_log (tenant_id, actor, action, ip_address, user_agent, success, agent_id, created_at)
		 VALUES ('t1', '1', 'store', '203.0.113.77', 'sqlite/1.0', 1, 'clock', ?)`, sqliteText); err != nil {
		t.Fatal(err)
	}
	rec := provenanceRequest(s, "t1", "clock", "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got := decodeProvenance(t, rec)
	want := seen.Format(time.RFC3339)
	if got.FirstSeen != want || got.LastSeen != want {
		t.Errorf("first/last seen = %q/%q, want %q", got.FirstSeen, got.LastSeen, want)
	}
	if len(got.SourceIPs) != 1 {
		t.Fatalf("source_ips = %+v, want one entry", got.SourceIPs)
	}
	if got.SourceIPs[0].LastSeen != want {
		t.Errorf("source_ips[0].last_seen = %q, want %q", got.SourceIPs[0].LastSeen, want)
	}
	// The offset-less text must not survive into the reply: a browser would read
	// it as local time.
	if body := rec.Body.String(); strings.Contains(body, sqliteText) {
		t.Errorf("response still carries the offset-less SQLite timestamp: %s", body)
	}
}

// An unparseable timestamp must not drop the row or fail the request; it is
// returned as-is so the operator still sees the data.
func TestAgentProvenanceKeepsUnparsableTimestamp(t *testing.T) {
	s := provenanceFixture(t)
	if _, err := s.db.Exec(
		`INSERT INTO audit_log (tenant_id, actor, action, ip_address, user_agent, success, agent_id, created_at)
		 VALUES ('t1', '1', 'store', '203.0.113.78', 'broken/1.0', 1, 'busted', 'not-a-time')`); err != nil {
		t.Fatal(err)
	}
	rec := provenanceRequest(s, "t1", "busted", "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got := decodeProvenance(t, rec)
	if got.TotalActions != 1 {
		t.Errorf("total_actions = %d, want 1 (the row must not be dropped)", got.TotalActions)
	}
	if got.FirstSeen != "not-a-time" || got.LastSeen != "not-a-time" {
		t.Errorf("first/last seen = %q/%q, want the original text unchanged", got.FirstSeen, got.LastSeen)
	}
	if len(got.SourceIPs) != 1 || got.SourceIPs[0].LastSeen != "not-a-time" {
		t.Errorf("source_ips = %+v, want last_seen unchanged", got.SourceIPs)
	}
}

// The UI labels agent_id-less rows "unspecified"; the handler must translate
// that to IS NULL rather than searching for a literal agent_id.
func TestAgentProvenanceUnspecifiedMapsToNull(t *testing.T) {
	s := provenanceFixture(t)
	if _, err := s.db.Exec(
		`INSERT INTO audit_log (tenant_id, actor, action, ip_address, success, agent_id, created_at)
		 VALUES ('t1', '1', 'store', '203.0.113.9', 1, NULL, ?)`,
		time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatal(err)
	}
	rec := provenanceRequest(s, "t1", "unspecified", "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProvenance(t, rec); got.TotalActions != 1 {
		t.Errorf("total_actions = %d, want 1 (the NULL agent_id row)", got.TotalActions)
	}
}

// Activity older than the window must not leak into a "last 90 days" view.
func TestAgentProvenanceIgnoresRowsOutsideWindow(t *testing.T) {
	s := provenanceFixture(t)
	old := time.Now().UTC().AddDate(0, 0, -120).Format("2006-01-02T15:04:05Z")
	if _, err := s.db.Exec(
		`INSERT INTO audit_log (tenant_id, actor, action, ip_address, user_agent, success, agent_id, created_at)
		 VALUES ('t1', '1', 'store', '203.0.113.99', 'ancient/1.0', 1, 'codex', ?)`, old); err != nil {
		t.Fatal(err)
	}
	rec := provenanceRequest(s, "t1", "codex", "test-key")
	got := decodeProvenance(t, rec)
	if got.TotalActions != 3 {
		t.Errorf("total_actions = %d, want 3 (the 120-day-old row is outside the window)", got.TotalActions)
	}
	for _, ip := range got.SourceIPs {
		if ip.IP == "203.0.113.99" {
			t.Errorf("source_ips includes a row outside the 90-day window: %+v", got.SourceIPs)
		}
	}
}

// Non-numeric actors are credential markers, not key ids. They get fixed
// labels instead of being joined to a key name.
func TestAgentProvenanceLabelsNonNumericActors(t *testing.T) {
	s := provenanceFixture(t)
	now := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05Z")
	for _, actor := range []string{"apikey:t1", "admin"} {
		if _, err := s.db.Exec(
			`INSERT INTO audit_log (tenant_id, actor, action, ip_address, success, agent_id, created_at)
			 VALUES ('t1', ?, 'store', '203.0.113.50', 1, 'codex', ?)`, actor, now); err != nil {
			t.Fatal(err)
		}
	}
	rec := provenanceRequest(s, "t1", "codex", "test-key")
	got := decodeProvenance(t, rec)
	labels := map[string]int{}
	for _, row := range got.APIKeys {
		labels[row.Label] = row.Count
		if row.Label == "" {
			t.Errorf("empty actor label in %+v", got.APIKeys)
		}
	}
	for _, want := range []string{"tenant API key", "admin", "laptop"} {
		if _, ok := labels[want]; !ok {
			t.Errorf("api_keys = %+v, want a %q label", got.APIKeys, want)
		}
	}
}

// Admin basic auth works without an API key, through the same guard.
func TestAgentProvenanceAdminAuth(t *testing.T) {
	s := provenanceFixture(t)
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASS", "hunter2")
	rec := provenanceRequestAuth(s, "t1", "codex", func(req *http.Request) {
		req.SetBasicAuth("admin", "hunter2")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProvenance(t, rec); got.TotalActions != 3 {
		t.Errorf("total_actions = %d, want 3", got.TotalActions)
	}
}

// With cooperative visibility off, /agents omits name/note/isolated; provenance
// must not claim an isolation setting that nothing enforces.
func TestAgentProvenanceOmitsDisclosedFieldsWhenVisibilityOff(t *testing.T) {
	s := provenanceFixture(t)
	s.config.Features = map[string]bool{}
	rec := provenanceRequest(s, "t1", "codex", "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"display_name", "note", "isolated", "visibility_enabled"} {
		if _, ok := raw[field]; ok {
			t.Errorf("%s present with cross_agent_visibility off: %s", field, rec.Body.String())
		}
	}
}

// TestStoreAuditRecordsUserAgent closes the loop the provenance endpoint
// depends on: data-plane handlers must write the caller's User-Agent. The old
// fixture inserted user_agent rows directly, so /agent-provenance passed while
// /store, /query, /search and /graph/ask recorded nothing.
func TestStoreAuditRecordsUserAgent(t *testing.T) {
	f := newRecallFixture(t)
	body := `{"id":"ua-doc","text":"user agent fixture","agent_id":"codex","embedding":[1,0]}`
	req := httptest.NewRequest(http.MethodPost, "/store", strings.NewReader(body))
	req.Header.Set("X-HeurChain-API-Key", "test-key")
	req.Header.Set("X-HeurChain-Tenant", "t1")
	req.Header.Set("User-Agent", "provenance-test/1.0")
	rec := httptest.NewRecorder()
	f.s.handleStore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("store status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var ua, agent string
	if err := f.s.db.QueryRow(
		`SELECT user_agent, agent_id FROM audit_log WHERE tenant_id='t1' AND action='store' ORDER BY id DESC LIMIT 1`,
	).Scan(&ua, &agent); err != nil {
		t.Fatal(err)
	}
	if ua != "provenance-test/1.0" {
		t.Errorf("audit user_agent = %q, want provenance-test/1.0", ua)
	}
	if agent != "codex" {
		t.Errorf("audit agent_id = %q, want codex", agent)
	}

	// Oversized headers are capped before they reach the table.
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'a'
	}
	req = httptest.NewRequest(http.MethodPost, "/store", strings.NewReader(`{"id":"ua-doc-2","text":"second","embedding":[1,0]}`))
	req.Header.Set("X-HeurChain-API-Key", "test-key")
	req.Header.Set("X-HeurChain-Tenant", "t1")
	req.Header.Set("User-Agent", string(long))
	rec = httptest.NewRecorder()
	f.s.handleStore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second store status = %d (body: %s)", rec.Code, rec.Body.String())
	}
	var stored string
	if err := f.s.db.QueryRow(
		`SELECT user_agent FROM audit_log WHERE tenant_id='t1' AND action='store' ORDER BY id DESC LIMIT 1`,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 256 {
		t.Errorf("stored user_agent length = %d, want 256", len(stored))
	}
}
