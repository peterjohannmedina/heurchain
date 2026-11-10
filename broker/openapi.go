package main

import "net/http"

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondJSON(w, openAPISpec())
}

func openAPISpec() map[string]interface{} {
	ref := func(name string) map[string]interface{} {
		return map[string]interface{}{"$ref": "#/components/schemas/" + name}
	}
	jsonBody := func(schema map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"required": true,
			"content": map[string]interface{}{
				"application/json": map[string]interface{}{"schema": schema},
			},
		}
	}
	responses := func(description string, schema map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"200": map[string]interface{}{
				"description": description,
				"content": map[string]interface{}{
					"application/json": map[string]interface{}{"schema": schema},
				},
			},
			"400": map[string]interface{}{"$ref": "#/components/responses/Error"},
			"401": map[string]interface{}{"$ref": "#/components/responses/Error"},
		}
	}
	secured := []map[string][]string{{"HeurChainAPIKey": {}, "HeurChainTenant": {}}}
	object := map[string]interface{}{"type": "object"}
	storeResponses := responses("Stored and indexed memory", object)
	storeResponses["202"] = map[string]interface{}{
		"description": "Memory stored, but indexing incomplete. indexed=false; retry the same document ID. No automatic retry is scheduled.",
		"content":     map[string]interface{}{"application/json": map[string]interface{}{"schema": object}},
	}
	queryResponses := responses("Query results", object)
	queryResponses["503"] = map[string]interface{}{"description": "Text query embedding provider is unavailable or returned an invalid vector"}

	spec := map[string]interface{}{
		"openapi": "3.1.0",
		"info": map[string]interface{}{
			"title":       "HeurChain Memory API",
			"version":     brokerVersion,
			"description": "Persistent, tenant-isolated memory for AI agents. Send both HeurChain headers on every secured operation.",
		},
		"servers":  []map[string]interface{}{{"url": "http://localhost:8080"}},
		"security": secured,
		"paths": map[string]interface{}{
			"/health": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getHealth", "summary": "Check Redis/SQLite liveness; does not check Qdrant", "security": []interface{}{},
				"responses": responses("Health status", object),
			}},
			"/ready": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getReadiness", "summary": "Check Redis, SQLite and authenticated Qdrant read connectivity within two seconds; not index coverage", "security": []interface{}{},
				"responses": map[string]interface{}{
					"200": map[string]interface{}{"description": "Dependencies reachable; index_coverage is not_checked", "content": map[string]interface{}{"application/json": map[string]interface{}{"schema": object}}},
					"503": map[string]interface{}{"description": "One or more dependencies unavailable; includes per-dependency status", "content": map[string]interface{}{"application/json": map[string]interface{}{"schema": object}}},
				},
			}},
			"/openapi.json": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getOpenAPISpec", "summary": "Get this OpenAPI document", "security": []interface{}{},
				"responses": responses("OpenAPI document", object),
			}},
			"/store": map[string]interface{}{"post": map[string]interface{}{
				"operationId": "storeMemory", "summary": "Store a durable memory",
				"requestBody": jsonBody(ref("StoreRequest")), "responses": storeResponses,
			}},
			"/search": map[string]interface{}{"post": map[string]interface{}{
				"operationId": "searchMemories", "summary": "Search durable memories by meaning",
				"requestBody": jsonBody(ref("SearchRequest")), "responses": responses("Ranked memories", ref("MemoryList")),
			}},
			"/query": map[string]interface{}{"post": map[string]interface{}{
				"operationId": "queryMemories", "summary": "Query memories with text or a client-provided embedding",
				"requestBody": jsonBody(ref("QueryRequest")), "responses": queryResponses,
			}},
			"/admin/vault/rehydrate": map[string]interface{}{"post": map[string]interface{}{
				"operationId": "rehydrateVaultDocument", "summary": "Restore one vaulted document to active retrieval",
				"security":   []map[string][]string{{"AdminBasicAuth": {}}},
				"parameters": []map[string]interface{}{{"name": "tenant_id", "in": "query", "required": true, "schema": map[string]interface{}{"type": "string"}}},
				"requestBody": jsonBody(map[string]interface{}{"type": "object", "required": []string{"doc_id"}, "properties": map[string]interface{}{
					"doc_id": map[string]interface{}{"type": "string"},
				}}),
				"responses": map[string]interface{}{
					"200": map[string]interface{}{"description": "Document rehydrated", "content": map[string]interface{}{"application/json": map[string]interface{}{"schema": object}}},
					"400": map[string]interface{}{"$ref": "#/components/responses/Error"},
					"401": map[string]interface{}{"$ref": "#/components/responses/Error"},
					"409": map[string]interface{}{"$ref": "#/components/responses/Error"},
					"500": map[string]interface{}{"$ref": "#/components/responses/Error"},
				},
			}},
			"/memory/manifest": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getMemoryManifest", "summary": "Inspect available memory before asking the user",
				"responses": responses("Memory manifest", object),
			}},
			"/agent/{agent_id}": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getAgentMemories", "summary": "Get an agent's most recent memories",
				"parameters": []map[string]interface{}{
					{"name": "agent_id", "in": "path", "required": true, "schema": map[string]interface{}{"type": "string"}},
					{"name": "limit", "in": "query", "schema": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 50, "default": 10}},
				},
				"responses": responses("Recent agent memories", ref("MemoryList")),
			}},
			"/agent/{agent_id}/recall": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getAgentSessions", "summary": "Get active session IDs for compatibility",
				"parameters": []map[string]interface{}{{"name": "agent_id", "in": "path", "required": true, "schema": map[string]interface{}{"type": "string"}}},
				"responses":  responses("Active sessions", object),
			}},
			"/session/start": map[string]interface{}{"post": map[string]interface{}{
				"operationId": "startSession", "summary": "Start an agent session",
				"requestBody": jsonBody(ref("SessionStartRequest")), "responses": responses("Started session", object),
			}},
			"/session/end": map[string]interface{}{"post": map[string]interface{}{
				"operationId": "endSession", "summary": "End an agent session",
				"requestBody": jsonBody(map[string]interface{}{"type": "object", "required": []string{"session_id"}, "properties": map[string]interface{}{"session_id": map[string]interface{}{"type": "string"}}}),
				"responses":   responses("Session ended", object),
			}},
			"/session/{session_id}/context": map[string]interface{}{"get": map[string]interface{}{
				"operationId": "getSessionContext", "summary": "Get recent session context",
				"parameters": []map[string]interface{}{
					{"name": "session_id", "in": "path", "required": true, "schema": map[string]interface{}{"type": "string"}},
					{"name": "n", "in": "query", "schema": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 50, "default": 20}},
				},
				"responses": responses("Session context", object),
			}},
		},
		"components": map[string]interface{}{
			"securitySchemes": map[string]interface{}{
				"HeurChainAPIKey": map[string]interface{}{"type": "apiKey", "in": "header", "name": "X-HeurChain-API-Key"},
				"HeurChainTenant": map[string]interface{}{"type": "apiKey", "in": "header", "name": "X-HeurChain-Tenant"},
				"AdminBasicAuth":  map[string]interface{}{"type": "http", "scheme": "basic"},
			},
			"responses": map[string]interface{}{
				"Error": map[string]interface{}{"description": "Error", "content": map[string]interface{}{"application/json": map[string]interface{}{"schema": ref("Error")}}},
			},
			"schemas": map[string]interface{}{
				"Error": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"error": map[string]interface{}{"type": "string"}}},
				"Memory": map[string]interface{}{
					"type": "object", "required": []string{"id"},
					"properties": map[string]interface{}{
						"id": map[string]interface{}{"type": "string"}, "text": map[string]interface{}{"type": "string"},
						"agent_id": map[string]interface{}{"type": "string"}, "tags": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
						"metadata": object, "created_at": map[string]interface{}{"type": "string", "format": "date-time"},
					},
				},
				"MemoryList": map[string]interface{}{
					"type": "object", "properties": map[string]interface{}{
						"agent_id": map[string]interface{}{"type": "string"}, "count": map[string]interface{}{"type": "integer"},
						"items": map[string]interface{}{"type": "array", "items": ref("Memory")}, "results": map[string]interface{}{"type": "array", "items": ref("Memory")},
					},
				},
				"StoreRequest": map[string]interface{}{
					"type": "object", "required": []string{"text"},
					"properties": map[string]interface{}{
						"text": map[string]interface{}{"type": "string"}, "agent_id": map[string]interface{}{"type": "string"},
						"tags": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}, "metadata": object,
						"namespace": map[string]interface{}{
							"type": "string", "enum": []string{"ops", "notes", "self"}, "default": "notes",
							"description": "Memory decay tier. Governs how fast this item loses activation and is " +
								"eventually moved to the cold vault (where it stops appearing in search results). " +
								"'ops' decays fastest (operational chatter), 'notes' is the default, 'self' decays " +
								"slowest (durable facts about the agent or user). An unrecognised value is rejected " +
								"with 400 rather than silently defaulting.",
						},
					},
				},
				"SearchRequest": map[string]interface{}{
					"type": "object", "required": []string{"text"},
					"properties": map[string]interface{}{
						"retrieval_mode":    map[string]interface{}{"type": "string", "enum": []string{"hybrid", "lexical", "dense"}, "default": "hybrid"},
						"rrf_k":             map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "maximum": 1000, "default": 60},
						"lexical_weight":    map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "maximum": 10, "default": 1},
						"dense_weight":      map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "maximum": 10, "default": 1},
						"as_of":             map[string]interface{}{"type": "string", "format": "date-time", "description": "Validity of the current active memory at this instant; does not select historical revisions."},
						"memory_kind":       map[string]interface{}{"type": "string", "enum": []string{"factual", "creative"}},
						"max_per_source":    map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 50, "default": 0},
						"qdrant_candidates": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 200, "default": 50},
						"rerank_mode":       map[string]interface{}{"type": "string", "enum": []string{"none", "exact_identifiers"}},
						"text":              map[string]interface{}{"type": "string"}, "limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 50},
						"agent_id": map[string]interface{}{"type": "string"}, "tags": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"QueryRequest": map[string]interface{}{
					"type":        "object",
					"description": "Provide a nonempty embedding or nonblank text. A supplied nonempty embedding takes precedence; otherwise the broker generates one using its configured embedding provider.",
					"anyOf": []map[string]interface{}{
						{"required": []string{"embedding"}, "properties": map[string]interface{}{"embedding": map[string]interface{}{"minItems": 1}}},
						{"required": []string{"text"}, "properties": map[string]interface{}{"text": map[string]interface{}{"pattern": `\S`}}},
					},
					"properties": map[string]interface{}{
						"embedding": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "number"}, "maxItems": 4096},
						"text":      map[string]interface{}{"type": "string"}, "top_k": map[string]interface{}{"type": "integer", "minimum": 1},
						"threshold": map[string]interface{}{"type": "number"}, "agent_id": map[string]interface{}{"type": "string"},
						"tags": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"SessionStartRequest": map[string]interface{}{
					"type": "object", "required": []string{"agent_id"},
					"properties": map[string]interface{}{"agent_id": map[string]interface{}{"type": "string"}, "ttl_seconds": map[string]interface{}{"type": "integer", "minimum": 1}},
				},
			},
		},
	}
	paths := spec["paths"].(map[string]interface{})
	schemas := spec["components"].(map[string]interface{})["schemas"].(map[string]interface{})
	for _, tool := range mcpToolList {
		name := tool["name"].(string)
		if path, ok := memoryToolPaths[name]; ok {
			result := responses("Memory operation receipt or record", object)
			for code, description := range map[string]string{"202": "Stored/forgotten, but projection cleanup is incomplete; inspect receipt", "404": "Record or retained revision not found", "409": "Revision conflict or immutable checkpoint", "410": "Memory forgotten", "429": "Rate limit exceeded", "503": "Memory dependency unavailable"} {
				result[code] = map[string]interface{}{"description": description}
			}
			paths[path] = map[string]interface{}{"post": map[string]interface{}{"summary": tool["description"], "security": secured, "requestBody": jsonBody(tool["inputSchema"].(map[string]interface{})), "responses": result}}
		}
		for toolName, schemaName := range map[string]string{"heurchain_store": "StoreRequest", "heurchain_search": "SearchRequest", "heurchain_query": "QueryRequest"} {
			if name != toolName {
				continue
			}
			property := "filters"
			if name == "heurchain_store" {
				property = "expected_revision"
			}
			properties := schemas[schemaName].(map[string]interface{})["properties"].(map[string]interface{})
			properties[property] = tool["inputSchema"].(map[string]interface{})["properties"].(map[string]interface{})[property]
		}
	}
	return spec
}
