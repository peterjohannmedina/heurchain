package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// GET /agent-provenance?tenant_id=<id>&agent_id=<name>
//
// Aggregates what the stack knows about an agent so a user can distinguish
// overlapping instances that share a name across machines. Combines what the
// agent DISCLOSED (Phase-7 display name / note) with what the stack OBSERVED
// (source IPs, user-agents, which API key, timing, activity mix) from
// audit_log + usage_events.
//
// It authorises exactly like /agents and /vault/stats: a tenant-scoped API key
// or admin basic auth. See resolveVaultCaller.
//
// Only the recent window is scanned. audit_log and usage_events grow without
// bound, and a grouped scan over every row a tenant ever wrote is both slow
// and unrepresentative: provenance answers "which of this agent's instances is
// calling today", not "what did this name ever do". window_days in the reply
// states the horizon so a zero count is not misread as "never seen".
// ---------------------------------------------------------------------------

const provenanceWindowDays = 90

type provIPRow struct {
	IP       string `json:"ip"`
	Count    int    `json:"count"`
	LastSeen string `json:"last_seen"`
}
type provCountRow struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// provTimeNormaliser converts the timestamp text SQLite stores into RFC 3339
// UTC. CURRENT_TIMESTAMP writes "YYYY-MM-DD HH:MM:SS" with no offset; browsers
// parse that as local time, so a timestamp produced on the server in UTC is
// shown shifted by the viewer's offset. RFC 3339 with a Z suffix removes the
// ambiguity. Unparseable values are returned unchanged (dropping the row would
// hide data) and logged once per request to avoid flooding on bad rows.
type provTimeNormaliser struct{ warned bool }

func (n *provTimeNormaliser) normalise(raw string) string {
	if raw == "" {
		return ""
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05", // SQLite CURRENT_TIMESTAMP
		"2006-01-02T15:04:05Z",
		time.RFC3339,
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	if !n.warned {
		log.Printf("agent provenance: unparsable timestamp %q returned unchanged", raw)
		n.warned = true
	}
	return raw
}

func (s *Server) handleAgentProvenance(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	agentID := r.URL.Query().Get("agent_id")
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}
	if agentID == "" {
		jsonError(w, "missing agent_id", http.StatusBadRequest)
		return
	}
	// Same charset rule as DELETE /agents: "unspecified" is the UI sentinel for
	// rows with no agent_id; every other value must be a valid identifier.
	if agentID != "unspecified" && !validID.MatchString(agentID) {
		jsonError(w, "invalid agent_id", http.StatusBadRequest)
		return
	}

	// tenantStatusGuard authorises the tenant named in the header, while
	// resolveVaultCaller authorises the tenant_id query parameter. They must
	// name the same tenant: otherwise a suspended tenant could put an active
	// tenant's id in the header to pass the status check and then read its own
	// data through the query parameter. The guard guarantees the header is
	// present and valid, and it has already set the CORS headers.
	if r.Header.Get(s.config.Auth.TenantHeader) != tenantID {
		jsonError(w, "tenant header does not match tenant_id", http.StatusBadRequest)
		return
	}

	if _, ok := s.resolveVaultCaller(w, r, tenantID); !ok {
		return
	}
	ctx := r.Context()
	times := &provTimeNormaliser{}

	// The window and the "unspecified" → IS NULL mapping are part of one
	// fragment so every query below uses exactly the same scope. created_at is
	// stored by SQLite as "YYYY-MM-DD HH:MM:SS" (CURRENT_TIMESTAMP), so the
	// cutoff is formatted the same way and compared lexicographically. It is
	// bound, never interpolated.
	cutoff := time.Now().UTC().AddDate(0, 0, -provenanceWindowDays).Format("2006-01-02 15:04:05")
	filterFrom := func(base []interface{}) (string, []interface{}) {
		clause := "created_at >= ?"
		args := append(base, cutoff)
		if agentID == "unspecified" {
			return clause + " AND agent_id IS NULL", args
		}
		return clause + " AND agent_id = ?", append(args, agentID)
	}

	out := map[string]interface{}{
		"agent_id":    agentID,
		"window_days": provenanceWindowDays,
	}

	// Disclosed: Phase 7 settings (name / note / isolated). Like /agents, these
	// are only reported when the cooperative-visibility feature is on; with the
	// flag off no agent can set them and echoing stored values would imply
	// isolation that is not enforced.
	if crossAgentEnabled(s.config) {
		st := getAgentSettings(ctx, s.rdb, tenantID, agentID)
		out["display_name"] = st.DisplayName
		out["note"] = st.Note
		out["isolated"] = st.Isolated
		out["visibility_enabled"] = true
	}

	// Observed: first/last seen + total logged actions in the window.
	{
		clause, args := filterFrom([]interface{}{tenantID})
		var first, last sql.NullString
		var total int
		err := s.db.QueryRowContext(ctx,
			"SELECT MIN(created_at), MAX(created_at), COUNT(*) FROM audit_log WHERE tenant_id=? AND "+clause,
			args...).Scan(&first, &last, &total)
		if err != nil {
			log.Printf("agent provenance summary tenant=%s agent=%s: %v", tenantID, agentID, err)
		}
		out["first_seen"] = times.normalise(first.String)
		out["last_seen"] = times.normalise(last.String)
		out["total_actions"] = total
	}

	// Source IPs — the primary discriminator for same-named instances.
	out["source_ips"] = queryProvIPs(ctx, s.db, tenantID, agentID, times,
		"SELECT ip_address, COUNT(*) c, MAX(created_at) FROM audit_log WHERE tenant_id=? AND {A} "+
			"AND ip_address IS NOT NULL AND ip_address != '' GROUP BY ip_address ORDER BY c DESC LIMIT 25",
		filterFrom)

	// User-agents / client strings (secondary discriminator).
	out["user_agents"] = queryProvCounts(ctx, s.db, tenantID, agentID,
		"SELECT user_agent, COUNT(*) c FROM audit_log WHERE tenant_id=? AND {A} "+
			"AND user_agent IS NOT NULL AND user_agent != '' GROUP BY user_agent ORDER BY c DESC LIMIT 10",
		filterFrom)

	// Which API key(s) this agent authenticated with, labelled by key name.
	out["api_keys"] = queryProvKeys(ctx, s.db, tenantID, agentID, filterFrom)

	// Activity mix from usage_events (store/query/search/embed/flush).
	out["event_types"] = queryProvCounts(ctx, s.db, tenantID, agentID,
		"SELECT event_type, COUNT(*) c FROM usage_events WHERE tenant_id=? AND {A} GROUP BY event_type ORDER BY c DESC",
		filterFrom)

	respondJSON(w, out)
}

// queryProvIPs returns one page of source addresses. A database error is logged
// and yields an empty list: provenance is diagnostic, so a partial reply (200
// with an empty section) is more useful than failing the whole view.
func queryProvIPs(ctx context.Context, db *sql.DB, tenantID, agentID string, times *provTimeNormaliser, tmpl string, filter func([]interface{}) (string, []interface{})) []provIPRow {
	clause, args := filter([]interface{}{tenantID})
	rows, err := db.QueryContext(ctx, strings.ReplaceAll(tmpl, "{A}", clause), args...)
	if err != nil {
		log.Printf("agent provenance source_ips tenant=%s agent=%s: %v", tenantID, agentID, err)
		return []provIPRow{}
	}
	defer rows.Close()
	out := []provIPRow{}
	for rows.Next() {
		var r provIPRow
		var last sql.NullString
		if err := rows.Scan(&r.IP, &r.Count, &last); err != nil {
			log.Printf("agent provenance source_ips scan tenant=%s agent=%s: %v", tenantID, agentID, err)
			continue
		}
		r.LastSeen = times.normalise(last.String)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("agent provenance source_ips rows tenant=%s agent=%s: %v", tenantID, agentID, err)
	}
	return out
}

func queryProvCounts(ctx context.Context, db *sql.DB, tenantID, agentID, tmpl string, filter func([]interface{}) (string, []interface{})) []provCountRow {
	clause, args := filter([]interface{}{tenantID})
	rows, err := db.QueryContext(ctx, strings.ReplaceAll(tmpl, "{A}", clause), args...)
	if err != nil {
		log.Printf("agent provenance counts tenant=%s agent=%s: %v", tenantID, agentID, err)
		return []provCountRow{}
	}
	defer rows.Close()
	out := []provCountRow{}
	for rows.Next() {
		var r provCountRow
		if err := rows.Scan(&r.Label, &r.Count); err != nil {
			log.Printf("agent provenance counts scan tenant=%s agent=%s: %v", tenantID, agentID, err)
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("agent provenance counts rows tenant=%s agent=%s: %v", tenantID, agentID, err)
	}
	return out
}

func queryProvKeys(ctx context.Context, db *sql.DB, tenantID, agentID string, filter func([]interface{}) (string, []interface{})) []provCountRow {
	clause, args := filter([]interface{}{tenantID})
	// actor holds the api_keys.id (as text) for data-plane calls, an
	// "apikey:<tenant>" marker, or "admin". Only the numeric form is joined to
	// a key name; the rest are reported as fixed labels. The join is
	// tenant-scoped so a same-numbered key in another tenant can never name
	// this row.
	q := strings.ReplaceAll(`
		SELECT CASE
		         WHEN a.actor = 'admin'            THEN 'admin'
		         WHEN a.actor LIKE 'apikey:%'     THEN 'tenant API key'
		         ELSE COALESCE(k.name, 'key ' || a.actor)
		       END AS label,
		       COUNT(*) c
		FROM audit_log a
		LEFT JOIN api_keys k ON k.id = a.actor AND k.tenant_id = a.tenant_id
		WHERE a.tenant_id=? AND a.{A}
		GROUP BY label
		ORDER BY c DESC
		LIMIT 10`, "{A}", clause)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		log.Printf("agent provenance api_keys tenant=%s agent=%s: %v", tenantID, agentID, err)
		return []provCountRow{}
	}
	defer rows.Close()
	out := []provCountRow{}
	for rows.Next() {
		var r provCountRow
		if err := rows.Scan(&r.Label, &r.Count); err != nil {
			log.Printf("agent provenance api_keys scan tenant=%s agent=%s: %v", tenantID, agentID, err)
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("agent provenance api_keys rows tenant=%s agent=%s: %v", tenantID, agentID, err)
	}
	return out
}
