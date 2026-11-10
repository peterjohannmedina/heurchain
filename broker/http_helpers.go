package main

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// setCORSHeaders marks a response as readable by browser clients. The broker
// authenticates with API keys sent in headers rather than cookies, so a wildcard
// origin does not expose credentials to a third-party page.
func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// handleCORSPreflight short-circuits OPTIONS requests with full CORS headers,
// returning 204 No Content. Returns true if the method was OPTIONS so the
// caller can early-return.
func handleCORSPreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	setCORSHeaders(w)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+apiKeyHeaderName+", "+tenantHeaderName)
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// apiKeyHeaderName and tenantHeaderName are the default header names. They are
// used only for the preflight response; handlers read the configured names.
const (
	apiKeyHeaderName = "X-HeurChain-API-Key"
	tenantHeaderName = "X-HeurChain-Tenant"
)

// requestAPIKey reads the caller's API key from the configured API-key header
// or, failing that, from an Authorization: Bearer header.
func (s *Server) requestAPIKey(r *http.Request) string {
	if key := r.Header.Get(s.config.Auth.APIKeyHeader); key != "" {
		return key
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// mcpCredentials returns the caller's API key and tenant. The tenant is always
// taken from the tenant header; a bare key does not identify a tenant.
func (s *Server) mcpCredentials(r *http.Request) (apiKey, tenantID string) {
	return s.requestAPIKey(r), r.Header.Get(s.config.Auth.TenantHeader)
}

// auditUserAgent returns the request's User-Agent for an audit row, capped at
// 256 bytes. The header is caller-controlled and unbounded; storing it whole
// lets one client inflate the audit table (and every later provenance GROUP BY
// over it). Empty is stored as NULL, matching nullableString.
func auditUserAgent(r *http.Request) interface{} {
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	return nullableString(ua)
}

// auditAction records one authenticated action in audit_log.
//
// `actor` names the credential space: "admin" for basic auth and
// "apikey:<tenant>" for the machine path. A bare value is stored verbatim.
// Best-effort: a failure to audit must not fail the caller's request, but it is
// logged because a silent gap is the problem the table exists to prevent.
func (s *Server) auditAction(r *http.Request, tenantID, actor, action, resource string, success bool, details string) {
	ok := 0
	if success {
		ok = 1
	}
	if actor == "" {
		actor = "unknown"
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO audit_log (tenant_id, actor, action, resource, ip_address, user_agent, success, details)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableString(tenantID), actor, action, nullableString(resource),
		nullableString(clientIP(r)), auditUserAgent(r), ok, nullableString(details),
	); err != nil {
		log.Printf("audit_log write failed action=%s tenant=%s actor=%s: %v", action, tenantID, actor, err)
	}
}

// clientIP returns the caller's address for audit purposes. X-Forwarded-For is
// trusted only when the immediate peer is loopback, i.e. the request arrived
// through a local reverse proxy; otherwise the header could be spoofed by any
// direct client.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	return host
}
