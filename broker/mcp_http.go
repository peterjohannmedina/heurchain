package main

import (
	"mime"
	"net/http"
	"strings"
)

// Stateless Streamable HTTP on the existing public MCP resource. Notifications
// are accepted without a response body; JSON-RPC requests get a JSON response.
// No session identifier is used as an authentication credential.
func (s *Server) handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	if !prepareMCPRequest(w, r) {
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		jsonError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		jsonError(w, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedMCPVersion(v) {
		jsonError(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	apiKey, tenantID := s.mcpCredentials(r)
	sess := &mcpSession{tenantID: tenantID, apiKey: apiKey}
	if sess.apiKey == "" || !validID.MatchString(sess.tenantID) {
		jsonError(w, "missing credentials", http.StatusUnauthorized)
		return
	}
	if _, err := validateAPIKey(r.Context(), s.rdb, s.db, sess.tenantID, sess.apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var status string
	if s.db.QueryRowContext(r.Context(), `SELECT status FROM tenants WHERE id=?`, sess.tenantID).Scan(&status) != nil || status != "active" {
		jsonError(w, "tenant suspended or cancelled", http.StatusForbidden)
		return
	}
	var req rpcReq
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		jsonError(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	respondJSON(w, s.dispatchMCP(r.Context(), sess, &req))
}
