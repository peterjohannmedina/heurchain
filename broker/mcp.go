package main

// Remote MCP server over SSE — implements the MCP 2024-11-05 HTTP+SSE transport.
//
// Subscribers connect with just a URL and credentials:
//
//   {"mcpServers":{"heurchain":{"url":"http://localhost:8080/mcp/sse",
//     "headers":{"Authorization":"Bearer <key>","X-HeurChain-Tenant":"<id>"}}}}
//
// The API key may also be sent in the configured X-HeurChain-API-Key header.
// No local install, no Redis, no Node.js required.
//
// Transport:
//   GET  /mcp/sse          — open SSE stream; first event contains the message URL
//   POST /mcp/message      — send JSON-RPC requests; responses arrive on the SSE stream
//   POST /mcp/sse          — stateless Streamable HTTP transport
//
// Each SSE connection is a session. Auth is validated once on connection;
// subsequent tool calls reuse the session credentials via the same internal
// httptest dispatch path used by the REST handlers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── Session hub ───────────────────────────────────────────────────────────────

type mcpSession struct {
	tenantID string
	apiKey   string
	ch       chan string
	ctx      context.Context
	cancel   context.CancelFunc
}

type mcpSessionHub struct {
	mu   sync.RWMutex
	sess map[string]*mcpSession
}

var mcpHub = &mcpSessionHub{sess: make(map[string]*mcpSession)}

func (h *mcpSessionHub) add(id string, s *mcpSession) {
	h.mu.Lock()
	h.sess[id] = s
	h.mu.Unlock()
}

func (h *mcpSessionHub) remove(id string) {
	h.mu.Lock()
	delete(h.sess, id)
	h.mu.Unlock()
}

func (h *mcpSessionHub) get(id string) (*mcpSession, bool) {
	h.mu.RLock()
	s, ok := h.sess[id]
	h.mu.RUnlock()
	return s, ok
}

func (h *mcpSessionHub) push(id, msg string) {
	s, ok := h.get(id)
	if !ok {
		return
	}
	select {
	case s.ch <- msg:
	case <-s.ctx.Done():
	default:
		log.Printf("mcp: dropped event for session %s (buffer full)", id)
	}
}

// ── JSON-RPC types ────────────────────────────────────────────────────────────

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcRespErr `json:"error,omitempty"`
}

type rpcRespErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcOK(id interface{}, result interface{}) rpcResp {
	return rpcResp{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErr(id interface{}, code int, msg string) rpcResp {
	return rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcRespErr{Code: code, Message: msg}}
}

// ── GET /mcp/sse — open SSE stream ───────────────────────────────────────────

func (s *Server) handleMCPSSE(w http.ResponseWriter, r *http.Request) {
	// The same public URL also accepts stateless Streamable HTTP requests, so
	// modern browser clients do not need a different resource/audience.
	if r.Method == http.MethodPost {
		s.handleMCPHTTP(w, r)
		return
	}
	if !prepareMCPRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiKey, tenantID := s.mcpCredentials(r)
	if apiKey == "" || tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing credentials", http.StatusUnauthorized)
		return
	}
	if _, err := validateAPIKey(r.Context(), s.rdb, s.db, tenantID, apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Enforce tenant active status — suspended/cancelled tenants may not open sessions.
	var tenantStatus string
	if err := s.db.QueryRowContext(r.Context(), `SELECT status FROM tenants WHERE id = ?`, tenantID).Scan(&tenantStatus); err != nil {
		jsonError(w, "tenant not found", http.StatusForbidden)
		return
	}
	if tenantStatus != "active" {
		jsonError(w, "tenant suspended or cancelled", http.StatusForbidden)
		return
	}

	// Disable write deadline so the SSE connection can stay open indefinitely.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("mcp: could not clear write deadline: %v", err)
	}

	sessID := uuid.New().String()
	sessCtx, sessCancel := context.WithCancel(r.Context())
	sess := &mcpSession{
		tenantID: tenantID,
		apiKey:   apiKey,
		ch:       make(chan string, 64),
		ctx:      sessCtx,
		cancel:   sessCancel,
	}
	mcpHub.add(sessID, sess)
	defer func() {
		sessCancel()
		mcpHub.remove(sessID)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx/Caddy: disable proxy buffering
	w.WriteHeader(http.StatusOK)

	// First event: tell the client where to POST messages.
	fmt.Fprintf(w, "event: endpoint\ndata: /mcp/message?sessionId=%s\n\n", sessID)
	if err := rc.Flush(); err != nil {
		log.Printf("mcp: initial SSE flush failed: %v", err)
		return
	}

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			if err := rc.Flush(); err != nil {
				return
			}
		case msg := <-sess.ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// ── POST /mcp/message — receive JSON-RPC from client ─────────────────────────

func (s *Server) handleMCPMessage(w http.ResponseWriter, r *http.Request) {
	if !prepareMCPRequest(w, r) {
		return
	}
	if handleCORSPreflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessID := r.URL.Query().Get("sessionId")
	if sessID == "" {
		jsonError(w, "missing sessionId", http.StatusBadRequest)
		return
	}
	sess, ok := mcpHub.get(sessID)
	if !ok {
		jsonError(w, "session not found or expired", http.StatusNotFound)
		return
	}
	// A session ID is a routing handle, not an access credential.
	key, tenantID := s.mcpCredentials(r)
	if key != sess.apiKey || tenantID != sess.tenantID {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := validateAPIKey(r.Context(), s.rdb, s.db, sess.tenantID, key); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var status string
	if s.db.QueryRowContext(r.Context(), `SELECT status FROM tenants WHERE id = ?`, sess.tenantID).Scan(&status) != nil || status != "active" {
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

	// Notifications carry no id — acknowledge with 202, no SSE response.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.dispatchMCP(r.Context(), sess, &req)
	b, _ := json.Marshal(resp)
	mcpHub.push(sessID, string(b))
	w.WriteHeader(http.StatusAccepted)
}

// ── Method dispatch ───────────────────────────────────────────────────────────

func (s *Server) dispatchMCP(ctx context.Context, sess *mcpSession, req *rpcReq) rpcResp {
	switch req.Method {
	case "ping":
		return rpcOK(req.ID, map[string]interface{}{})
	case "initialize":
		version := "2024-11-05"
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) != 0 && json.Unmarshal(req.Params, &params) != nil {
			return rpcErr(req.ID, -32602, "invalid initialize params")
		}
		switch params.ProtocolVersion {
		case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
			version = params.ProtocolVersion
		default:
			if params.ProtocolVersion != "" {
				version = "2025-11-25"
			}
		}
		return rpcOK(req.ID, map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "heurchain-mcp", "version": brokerVersion},
		})

	case "tools/list":
		return rpcOK(req.ID, map[string]interface{}{"tools": mcpToolList})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, "invalid params")
		}
		result, toolErr := s.invokeMCPTool(ctx, sess, p.Name, p.Arguments)
		if toolErr != nil {
			return rpcOK(req.ID, map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": toolErr.Error()}},
				"isError": true,
			})
		}
		return rpcOK(req.ID, map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": string(result)}},
		})

	default:
		return rpcErr(req.ID, -32601, "method not found: "+req.Method)
	}
}

// ── Tool invocation ───────────────────────────────────────────────────────────

// invokeMCPTool dispatches a tool call to the corresponding HTTP handler via
// httptest.NewRecorder — no network round-trip, full business-logic reuse.
func (s *Server) invokeMCPTool(ctx context.Context, sess *mcpSession, tool string, args json.RawMessage) (json.RawMessage, error) {
	// Each tool call gets a 30-second budget independent of the session lifetime.
	callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
	defer callCancel()

	// call constructs a fake HTTP request, invokes the handler, and returns the body.
	call := func(method, path string, body []byte, handler http.HandlerFunc) (json.RawMessage, error) {
		var bodyR io.Reader
		if len(body) > 0 {
			bodyR = bytes.NewReader(body)
		}
		req := httptest.NewRequest(method, path, bodyR)
		req = req.WithContext(callCtx)
		req.Header.Set(s.config.Auth.APIKeyHeader, sess.apiKey)
		req.Header.Set(s.config.Auth.TenantHeader, sess.tenantID)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		s.tenantStatusGuard(s.rpmGuard(handler))(rec, req)

		if rec.Code >= 400 {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &e) == nil && e.Error != "" {
				return nil, fmt.Errorf("%s", e.Error)
			}
			return nil, fmt.Errorf("tool %s returned %d", tool, rec.Code)
		}
		return rec.Body.Bytes(), nil
	}

	get := func(path string, handler http.HandlerFunc) (json.RawMessage, error) {
		return call("GET", path, nil, handler)
	}
	del := func(path string, handler http.HandlerFunc) (json.RawMessage, error) {
		return call("DELETE", path, nil, handler)
	}

	switch tool {
	case "heurchain_memory_get":
		return call("POST", "/memory/get", args, s.handleMemoryGet)
	case "heurchain_memory_list":
		return call("POST", "/memory/list", args, s.handleMemoryList)
	case "heurchain_memory_update":
		return call("POST", "/memory/update", args, s.handleMemoryUpdate)
	case "heurchain_memory_forget":
		return call("POST", "/memory/forget", args, s.handleMemoryForget)
	case "heurchain_checkpoint":
		return call("POST", "/memory/checkpoint", args, s.handleMemoryCheckpoint)
	case "heurchain_resume":
		return call("POST", "/memory/resume", args, s.handleMemoryResume)
	// ── Memory ────────────────────────────────────────────────────────────────
	case "heurchain_store":
		return call("POST", "/store", args, s.handleStore)

	case "heurchain_search":
		return call("POST", "/search", args, s.handleSearch)

	case "heurchain_query":
		return call("POST", "/query", args, s.handleQuery)

	case "heurchain_promote":
		// MCP uses {id}; handler expects {doc_id, tenant_id}
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.ID == "" {
			return nil, fmt.Errorf("id is required")
		}
		body, _ := json.Marshal(map[string]string{"doc_id": p.ID, "tenant_id": sess.tenantID})
		return call("POST", "/promote", body, s.handlePromote)

	case "heurchain_manifest":
		return get("/memory/manifest", s.handleMemoryManifest)

	// ── Sessions ──────────────────────────────────────────────────────────────
	case "heurchain_session_start":
		return call("POST", "/session/start", args, s.handleSessionStart)

	case "heurchain_session_end":
		return call("POST", "/session/end", args, s.handleSessionEnd)

	case "heurchain_session_context":
		var p struct {
			SessionID string `json:"session_id"`
			N         *int   `json:"n,omitempty"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session_id is required")
		}
		path := "/session/" + url.PathEscape(p.SessionID) + "/context"
		if p.N != nil {
			path += fmt.Sprintf("?n=%d", *p.N)
		}
		return get(path, s.handleSessionCtx)

	case "heurchain_agent_recall":
		var p struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.AgentID == "" {
			return nil, fmt.Errorf("agent_id is required")
		}
		return get("/agent/"+url.PathEscape(p.AgentID)+"/recall", s.handleAgentRecall)

	// ── Provenance ─────────────────────────────────────────────────────────────
	case "heurchain_memory_history":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.ID == "" {
			return nil, fmt.Errorf("id is required")
		}
		return get("/vectors/"+url.PathEscape(p.ID)+"/history", s.handleVectorHistory)

	// ── Embedding ──────────────────────────────────────────────────────────────
	case "heurchain_embed":
		return call("POST", "/embed", args, s.handleEmbed)

	// ── Agents ─────────────────────────────────────────────────────────────────
	case "heurchain_agents":
		// handleAgents reads tenant_id from the query string (admin/JWT-style auth);
		// scope it to the session tenant.
		return get("/agents?tenant_id="+url.QueryEscape(sess.tenantID), s.handleAgents)

	// ── Vault ─────────────────────────────────────────────────────────────────
	case "heurchain_vault_stats":
		return get("/vault/stats", s.handleVaultStats)

	case "heurchain_vault_flush":
		// handleVaultFlush reads tenant_id from the JSON body; inject the session tenant.
		body, _ := json.Marshal(map[string]string{"tenant_id": sess.tenantID})
		return call("POST", "/vault/flush-working-memory", body, s.handleVaultFlush)

	case "heurchain_vault_export":
		// handleVaultExport reads tenant_id from the query string and streams a zip.
		return get("/vault/export?tenant_id="+url.QueryEscape(sess.tenantID), s.handleVaultExport)

	// ── Cache ─────────────────────────────────────────────────────────────────
	case "heurchain_cache_set":
		return call("POST", "/cache", args, s.handleCache)

	case "heurchain_cache_get":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Key == "" {
			return nil, fmt.Errorf("key is required")
		}
		return get("/cache?key="+url.QueryEscape(p.Key), s.handleCache)

	case "heurchain_cache_delete":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Key == "" {
			return nil, fmt.Errorf("key is required")
		}
		return del("/cache?key="+url.QueryEscape(p.Key), s.handleCache)

	default:
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
}

// ── Tool definitions (returned by tools/list) ─────────────────────────────────

var mcpToolList = []map[string]interface{}{
	{
		"name":        "heurchain_store",
		"description": "Store a fact, decision or concise summary. Store secret references, never passwords, tokens or private reasoning. Supply a stable ID for retries; use memory_update with expected_revision for corrections.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text":     map[string]interface{}{"type": "string", "description": "Content to remember (max ~2000 chars)."},
				"agent_id": map[string]interface{}{"type": "string", "description": "Your agent identifier (e.g. 'kimi', 'openclaw')."},
				"tags":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Tags: credential, decision, fact, project-context, system-config, conversation, code."},
				"id":       map[string]interface{}{"type": "string", "description": "Stable ID for idempotent re-stores. Auto-UUID if omitted."},
				"metadata": map[string]interface{}{"type": "object", "description": "Arbitrary JSON metadata (session_id, source, project, etc.)."},
				"namespace": map[string]interface{}{
					"type": "string", "enum": []string{"ops", "notes", "self"},
					"description": "Decay tier controlling how long this stays retrievable before moving to " +
						"the cold vault (where it no longer appears in search). 'self' = durable facts you " +
						"want kept longest, 'notes' = default working memory, 'ops' = short-lived " +
						"operational detail. Unknown values are rejected.",
				},
			},
			"required": []string{"text"},
		},
	},
	{
		"name":        "heurchain_search",
		"description": "Hybrid BM25 + dense vector search. Best for keyword lookups — project names, credential labels, specific identifiers.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text":     map[string]interface{}{"type": "string", "description": "Search query."},
				"agent_id": map[string]interface{}{"type": "string", "description": "Filter to this agent's memories."},
				"tags":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Filter by any of these tags."},
				"limit":    map[string]interface{}{"type": "number", "description": "Max results (default 10, max 50)."},
			},
			"required": []string{"text"},
		},
	},
	{
		"name":        "heurchain_query",
		"description": "Semantic similarity search. Best for natural-language retrieval ('what did I decide about authentication?').",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text":      map[string]interface{}{"type": "string", "description": "Natural language query."},
				"agent_id":  map[string]interface{}{"type": "string", "description": "Filter to this agent's memories."},
				"tags":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Filter by any of these tags."},
				"top_k":     map[string]interface{}{"type": "number", "description": "Max results (default 10)."},
				"threshold": map[string]interface{}{"type": "number", "description": "Cosine similarity floor 0.0–1.0 (default 0.0)."},
			},
			"required": []string{"text"},
		},
	},
	{
		"name":        "heurchain_promote",
		"description": "Increment a memory item's retrieval weight. Call after a human confirms a stored fact is correct.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string", "description": "Vector ID to promote."}},
			"required":   []string{"id"},
		},
	},
	{
		"name":        "heurchain_manifest",
		"description": "Get a memory summary: total count, per-tag counts, per-agent counts, 10 most-recent items. Call at task start before asking for things already stored.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		"name":        "heurchain_session_start",
		"description": "Open a session to correlate all store calls in this conversation. Returns a session_id — pass it in metadata.session_id on every store call.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_id":    map[string]interface{}{"type": "string", "description": "Your agent identifier."},
				"ttl_seconds": map[string]interface{}{"type": "number", "description": "Session TTL (default 86400 = 24 h)."},
			},
			"required": []string{"agent_id"},
		},
	},
	{
		"name":        "heurchain_session_end",
		"description": "Close a session. Call at conversation end, then store a session summary tagged 'conversation'.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"session_id": map[string]interface{}{"type": "string"}},
			"required":   []string{"session_id"},
		},
	},
	{
		"name":        "heurchain_session_context",
		"description": "Replay a prior session: returns items written under that session_id, ordered by recency. Use to resume a conversation without re-querying the user.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"session_id": map[string]interface{}{"type": "string"},
				"n":          map[string]interface{}{"type": "number", "description": "Items to return (default 20, max 50)."},
			},
			"required": []string{"session_id"},
		},
	},
	{
		"name":        "heurchain_agent_recall",
		"description": "List session IDs for an agent. Use at session start to discover prior sessions, then call session_context on each.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"agent_id": map[string]interface{}{"type": "string"}},
			"required":   []string{"agent_id"},
		},
	},
	{
		"name":        "heurchain_memory_history",
		"description": "Audit trail for a single memory item: the rows in this tenant's audit log whose resource is this id, newest first, capped at the 100 most recent. In practice these are store and lifecycle events (the writes that stamped resource=<id>), not reads — a query that happened to retrieve this item is not recorded against it, so an empty result does not mean the item was never retrieved.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string", "description": "Vector/document ID to trace."}},
			"required":   []string{"id"},
		},
	},
	{
		"name":        "heurchain_embed",
		"description": "Embed text into a dense vector using the server's active embedding model. Use to pre-compute an embedding for query, or to inspect dimensionality.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"text": map[string]interface{}{"type": "string", "description": "Text to embed."}},
			"required":   []string{"text"},
		},
	},
	{
		"name":        "heurchain_agents",
		"description": "List agent_ids active in this tenant over the last 7 days, with last-seen time and event counts. Use to discover which agents have been writing memory.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		"name":        "heurchain_vault_stats",
		"description": "Storage stats for this tenant: vector count, total bytes, size in MB, estimated token count.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		"name":        "heurchain_vault_flush",
		"description": "Flush working memory: consolidate hot vectors into the durable vault for this tenant. Call at the end of a long session to persist accumulated memories.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		"name":        "heurchain_vault_export",
		"description": "Export this tenant's stored memories as a zip archive of human-readable Markdown files (embeddings excluded). Use for backup or audit.",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		"name":        "heurchain_cache_set",
		"description": "Store a short-lived key-value pair in the tenant cache (Redis, TTL-backed). For ephemeral state: current session_id, active model, in-progress task ID.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key":         map[string]interface{}{"type": "string", "description": "Cache key (max 256 chars, [A-Za-z0-9._:-])."},
				"value":       map[string]interface{}{"type": "string", "description": "Value (max 64 KB)."},
				"ttl_seconds": map[string]interface{}{"type": "number", "description": "TTL in seconds (default 86400, max 2592000)."},
			},
			"required": []string{"key", "value"},
		},
	},
	{
		"name":        "heurchain_cache_get",
		"description": "Retrieve a value from the tenant cache. Returns {found: false} if absent or expired.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"key": map[string]interface{}{"type": "string"}},
			"required":   []string{"key"},
		},
	},
	{
		"name":        "heurchain_cache_delete",
		"description": "Delete a key from the tenant cache.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"key": map[string]interface{}{"type": "string"}},
			"required":   []string{"key"},
		},
	},
}
