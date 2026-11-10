package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Discovery — GET /
//
// Returns agent-readable usage instructions with no authentication required.
// Content negotiation:
//   - Accept: application/json  → structured JSON spec
//   - anything else             → text/markdown (browser- and agent-friendly)
//
// CORS: Access-Control-Allow-Origin: * (intentionally public and unauthenticated).
// The handler explicitly rejects any path other than "/" so that Go's catch-all
// mux behaviour does not shadow routes registered later.
// ---------------------------------------------------------------------------

const discoveryMarkdown = `# HeurChain API — Agent Onboarding

**Base URL:** ` + "`http://localhost:8080`" + `

You are an AI agent connecting to HeurChain — a persistent vector memory broker.
Read this once at the start of any new session. It answers: what to store, when
to store it, how to retrieve it, and how sessions work.

---

## Authentication

Memory endpoints require two headers (discovery, health and readiness are public):

` + "```" + `
X-HeurChain-API-Key: hc_<32-hex>
X-HeurChain-Tenant: <uuid>
` + "```" + `

**If these are not configured:** use the client secret store or ask the user to
configure the connection. Never log credentials or save secret values in memories.

---

## First-run checklist

Run these steps in order at the start of every new session:

1. **` + "`GET /memory/manifest`" + `** — see what's already stored (tag counts, agent
   counts, 10 most-recent items). Do this BEFORE asking the user for anything
   that might already be in memory (credentials, project context, decisions).
2. **Use the configured connection.** Recall credential locations and purposes,
   never copy secret values into agent memory.
3. **` + "`POST /session/start`" + `** — open a session. Save the returned ` + "`session_id`" + `
   and pass it in ` + "`metadata.session_id`" + ` on every ` + "`/store`" + ` call this conversation.
4. **` + "`GET /agent/{your_agent_id}`" + `** — recall your agent's own recent memories.
   Review before starting work.
5. **At session end:** call ` + "`POST /session/end`" + `, then write a session summary
   (see _What to store and when_ below).

---

## Inspect, correct, forget and checkpoint

POST /memory/get and /memory/list inspect exact records, including archives.
POST /memory/update requires expected_revision and a correction reason. Retained
superseded versions are explicit history, not ordinary recall.
POST /memory/forget requires expected_revision and confirm=true. Verify the
cleanup_complete receipt. It covers that ID and its retained versions, not
backups, independent copies, encrypted silos or previously returned client context.
POST /memory/checkpoint saves concise working context; /memory/resume reads it by
ID without dense indexing. Retain checkpoint_id in the harness handoff and verify
stored=true. A host hook or proactive agent call must invoke it; MCP does not
intercept automatic compaction. New snapshots need new IDs.
List/search/query accept conjunctive structured filters for author, namespace,
tags, lifecycle dates and scalar metadata. Search/query return active records.
Keep secrets/private reasoning out of all memories and checkpoints.

## Core write/read endpoints

### ` + "`POST /store`" + ` — write a memory item

` + "```json" + `
{
  "text":     "the content to remember",
  "agent_id": "kimi",
  "tags":     ["fact", "project-context"],
  "metadata": {
    "session_id": "ses_abc123",
    "source":     "user-statement",
    "project":    "openclaw"
  }
}
` + "```" + `

Optional top-level fields:
- ` + "`id`" + ` — stable key for this item. Auto-UUID if omitted. Reposting with the same ` + "`id`" + ` overwrites (safe to retry on transient errors).
- ` + "`embedding`" + ` (float[]) — bring your own 768-dim vector. Omit to let the server embed via BAAI/bge-base-en-v1.5.
- ` + "`namespace`" + ` — decay tier: ` + "`ops`" + ` | ` + "`notes`" + ` | ` + "`self`" + `. Default ` + "`notes`" + `.

#### Choosing a namespace

Memories lose activation as they age and gain it each time they are retrieved.
Below a threshold an item is moved to the cold vault, where it is preserved but
**stops appearing in ` + "`/search`" + ` and ` + "`/query`" + ` results**. The namespace sets how
fast that clock runs:

| namespace | decays | use for |
|-----------|--------|---------|
| ` + "`ops`" + ` | fastest | transient operational detail — job output, retry chatter, one-off status |
| ` + "`notes`" + ` (default) | medium | ordinary working memory |
| ` + "`self`" + ` | slowest | durable facts about the user, the agent, or the project you want kept |

Pick ` + "`self`" + ` for anything you would be annoyed to lose in a month. An
unrecognised value returns ` + "`400`" + ` rather than silently falling back, so a typo
cannot quietly retier your memories.

Response ` + "`200`" + `:
` + "```json" + `
{ "id": "uuid-or-your-id", "stored": true }
` + "```" + `

---

### ` + "`POST /query`" + ` — semantic similarity search

` + "```json" + `
{
  "text":      "what should I retrieve",
  "agent_id":  "kimi",
  "tags":      ["credential"],
  "top_k":     5,
  "threshold": 0.6
}
` + "```" + `

- ` + "`agent_id`" + ` — optional; filters to that agent's writes only
- ` + "`tags`" + ` — optional; AND semantics (all tags must match)
- ` + "`top_k`" + ` — default 10, max 100
- ` + "`threshold`" + ` — cosine similarity floor, default 0.0

Response ` + "`200`" + `:
` + "```json" + `
{
  "count": 2,
  "results": [
    {
      "id":         "uuid",
      "score":      0.87,
      "text":       "the stored content",
      "agent_id":   "kimi",
      "tags":       ["credential"],
      "metadata":   { "session_id": "ses_abc123" },
      "created_at": "2026-05-13T18:00:00Z"
    }
  ]
}
` + "```" + `

---

### ` + "`POST /search`" + ` — hybrid BM25 + dense search (RRF fusion)

Send text, limit (default 10, maximum 50), qdrant_candidates (default 50,
maximum 200), and optional agent_id/tags. The broker embeds the text and fuses
positive BM25 matches with dense results. Agent matching is exact; tags match
any requested tag. These filters apply before the final result limit.

Results contain source text, metadata, canonical document IDs, and available
BM25/dense ranks and scores. The retrieval object reports backend status,
candidate limits, unresolved IDs and missing text. A failed leg may return a
degraded result, including an empty result; inspect bm25_status/dense_status.

Non-silo /store now projects into both indexes. HTTP 200 with indexed=true
confirms completed indexing. HTTP 202 with stored=true and indexed=false means
the memory is durable but indexing is incomplete: retry the same document ID.
There is no background retry queue. Silo storage retains its encrypted path.

---

### ` + "`POST /embed`" + ` — embed without storing

` + "```json" + `
{ "text": "..." }
` + "```" + `

Response: ` + "`{ \"embedding\": [...768 floats...] }`" + `. Use when you control storage yourself.

---

## Session lifecycle

Sessions correlate writes across a conversation so you can replay the full
context of any prior session later.

### ` + "`POST /session/start`" + `

` + "```json" + `
{ "agent_id": "kimi", "ttl_seconds": 86400 }
` + "```" + `

Response: ` + "`{ \"session_id\": \"ses_...\" }`" + `

Save ` + "`session_id`" + ` and pass it in ` + "`metadata.session_id`" + ` on every ` + "`/store`" + ` call
this conversation. ` + "`ttl_seconds`" + ` defaults to 86400 (24 h) if omitted.

### ` + "`POST /session/end`" + `

` + "```json" + `
{ "session_id": "ses_..." }
` + "```" + `

Response: ` + "`{ \"ok\": true }`" + `

Call at conversation end, then immediately store a session summary (see below).

### ` + "`GET /session/{session_id}/context?n=20`" + `

Returns up to 50 items written under that session, ordered by recency:

` + "```json" + `
{
  "items": [
    { "id": "...", "text": "...", "tags": [...], "created_at": "..." }
  ]
}
` + "```" + `

Use this to resume a prior conversation: pass the previous session ID to
re-hydrate context without re-querying the user.

---

## Agent recall

### ` + "`GET /agent/{agent_id}`" + ` — your agent's recent memories

Returns recent memories written by this ` + "`agent_id`" + `. Call at session start to
re-orient before starting any task.

### ` + "`GET /agents`" + ` — list active agents

Returns distinct ` + "`agent_id`" + ` values from the last 7 days. Use to discover what
other agents have contributed to this tenant's memory.

---

## Memory manifest

### ` + "`GET /memory/manifest`" + `

` + "```json" + `
{
  "total":  42,
  "tags":   [{ "tag": "credential", "count": 3 }],
  "agents": [{ "agent_id": "kimi", "count": 18 }],
  "recent": [{ "id": "...", "agent_id": "...", "tags": [...], "created_at": "..." }],
  "tag_taxonomy": ["credential","decision","fact","project-context","system-config","conversation","code"],
  "generated_at": "2026-05-13T18:00:00Z"
}
` + "```" + `

**Always call this at the start of any non-trivial task.**

---

## Vault

### ` + "`GET /vault/stats`" + `

Vector count, byte estimate, oldest/newest item timestamps for this tenant.

### ` + "`POST /vault/flush-working-memory`" + `

Deletes items tagged ` + "`conversation`" + ` (or a custom tag set you specify). Use at
session end to prune conversational chaff before it pollutes future retrieval.

### ` + "`GET /vault/export`" + `

A zip archive containing every vector item for this tenant, one Markdown file
per item (YAML frontmatter: id, tenant, agent_id, created_at, tags; then the
body text; then the item's metadata as a fenced JSON block). Embeddings are not
included, so a restore re-embeds each body server-side. Use for backup or
migration.

### ` + "`POST /admin/vault/rehydrate?tenant_id=...`" + `

Admin-only recovery of one vaulted document. Send a JSON body with a ` + "`doc_id`" + ` field; the
broker restores Redis, BM25, and Qdrant from the preserved embedding and
returns ` + "`duration_ms`" + `. A newer active record returns ` + "`409 Conflict`" + `.

---

## What to store and when

**What goes in ` + "`text`" + `:** anything that should survive past this conversation.

| Content | Tag | Example |
|---|---|---|
| Credential location | system-config | Which secret store holds the configured connection, without its value |
| Architecture decision | ` + "`decision`" + ` | ` + "`\"Chose PostgreSQL over MySQL — reason: JSON operators\"`" + ` |
| User-stated fact | ` + "`fact`" + ` | ` + "`\"The user's email is alex@example.com\"`" + ` |
| Project layout | ` + "`project-context`" + ` | ` + "`\"OpenClaw: VM 242, user rm, pnpm monorepo at ~/openclaw\"`" + ` |
| Server / env config | ` + "`system-config`" + ` | ` + "`\"HeurChain tenant abc-123, key rotated 2026-05-13\"`" + ` |
| Session summary | ` + "`conversation`" + ` | One-paragraph summary of what was done and what's pending |
| Code snippet | ` + "`code`" + ` | Small, self-contained snippet useful across sessions |

**What NOT to store:** raw conversation transcripts, large file contents, data
that changes every run (dynamic UUIDs, ephemeral timestamps), anything that
belongs in a repo or config file.

**When to call ` + "`/store`" + `:**

- **Connection configured**: record its purpose and secret-store location only;
  never the API key, token or password itself.
- **Decision made** — when you and the user agree on an approach, architecture
  choice, or policy. Tag ` + "`decision`" + `.
- **User states a fact** — name, email, preference, account info. Tag ` + "`fact`" + `.
- **Context eviction** — context window filling up; store a summary before
  truncating. Tag ` + "`conversation`" + `.
- **Session end** — one-paragraph summary of accomplishments and open items.
  Tag ` + "`conversation`" + `.
- **NOT after every message** — that produces retrieval noise. Store selectively.

**Chunking:** max 512 tokens (~2000 chars) per item. Split larger content into
segments; link them via ` + "`metadata.session_id`" + ` or a shared ` + "`id`" + ` prefix
(e.g. ` + "`doc-plan-1`" + `, ` + "`doc-plan-2`" + `).

---

## Tags

Tag rules: lowercase alphanumeric, dash or underscore, max 32 chars, max 16 per item.

**Taxonomy** — use these names for cross-agent consistency:

| Tag | Use for |
|---|---|
| ` + "`credential`" + ` | Credential locations and purposes; never secret values |
| ` + "`decision`" + ` | Architecture, tooling, or policy choices |
| ` + "`fact`" + ` | User-stated facts, preferences, account info |
| ` + "`project-context`" + ` | Project structure, stack, file locations |
| ` + "`system-config`" + ` | Server config, env vars, host addresses |
| ` + "`conversation`" + ` | Session summaries, ephemeral notes |
| ` + "`code`" + ` | Code snippets, scripts, config files |

---

## Error handling

| Status | Meaning | Action |
|---|---|---|
| ` + "`200`" + ` | Success | — |
| ` + "`400`" + ` | Bad request (missing field, malformed JSON) | Fix payload, do not retry |
| ` + "`401`" + ` | Auth failed (bad/missing key or tenant) | Ask user for correct credentials |
| ` + "`404`" + ` | Item or session not found | Do not retry |
| ` + "`429`" + ` | Rate limit exceeded | Body: ` + "`{\"limit\":60,\"current\":63}`" + `. Back off 60 s then retry |
| ` + "`5xx`" + ` | Server error | Exponential backoff, max 3 attempts. Report if persistent |

All error responses are JSON: ` + "`{ \"error\": \"description\" }`" + `.

---

## OpenClaw / Hermes integration

- Set ` + "`agent_id`" + ` to ` + "`\"openclaw\"`" + ` or ` + "`\"hermes\"`" + ` consistently across all sessions for this tenant.
- At first boot: store your MCP server list, model routing config, and workspace
  path under tag ` + "`system-config`" + `.
- Each session start: call ` + "`GET /agent/openclaw`" + ` (or ` + "`/agent/hermes`" + `) before any task.
- If the HeurChain MCP server is configured, prefer MCP tools
  (` + "`mcp__heurchain__heurchain_search`" + `, ` + "`mcp__heurchain__heurchain_export`" + `).
  Fall back to direct HTTP if MCP is unavailable.
- ` + "`POST /promote`" + ` — increments a memory item's retrieval weight. Call after a
  human confirms a stored fact is correct.

---

## Discovery

- ` + "`GET /`" + ` — this document (` + "`text/markdown`" + ` default; ` + "`Accept: application/json`" + ` for structured spec). Send tenant headers to get a manifest summary appended.
- ` + "`GET /health`" + ` — service liveness (no auth)
- ` + "`GET /ready`" + ` — Redis/SQLite and authenticated Qdrant read connectivity (no auth); 503 if a dependency fails. Does not attest index coverage.
- ` + "`GET /memory/manifest`" + ` — tenant memory summary (auth required)
- ` + "`GET /openapi.json`" + ` — OpenAPI 3 spec (no auth)

`

// discoverySpec is the canonical structured representation of the API.
// Marshalled to JSON at request time so limits and endpoints stay in sync with
// the Markdown form above.
type discoverySpec struct {
	Service   string              `json:"service"`
	Version   string              `json:"version"`
	BaseURL   string              `json:"base_url"`
	Auth      discoveryAuth       `json:"auth"`
	Endpoints []discoveryEndpoint `json:"endpoints"`
	Limits    discoveryLimits     `json:"limits"`
	Gotchas   []string            `json:"gotchas"`
	FirstRun  []string            `json:"first_run_checklist"`
	Discovery discoveryLinks      `json:"discovery"`
}

type discoveryAuth struct {
	Type    string            `json:"type"`
	Headers []discoveryHeader `json:"headers"`
}

type discoveryHeader struct {
	Name   string `json:"name"`
	Format string `json:"format"`
}

type discoveryEndpoint struct {
	Path         string         `json:"path"`
	Method       string         `json:"method"`
	Summary      string         `json:"summary"`
	Body         map[string]any `json:"body,omitempty"`
	AuthRequired bool           `json:"auth_required"`
}

type discoveryLimits struct {
	MaxTextTokensPerRequest int    `json:"max_text_tokens_per_request"`
	AutoTruncate            bool   `json:"auto_truncate"`
	EmbeddingDimension      int    `json:"embedding_dimension"`
	EmbeddingModel          string `json:"embedding_model"`
	RateLimitDefaultRPM     int    `json:"rate_limit_default_rpm"`
}

type discoveryLinks struct {
	InstructionsMarkdown string `json:"instructions_markdown"`
	InstructionsJSON     string `json:"instructions_json"`
	OpenAPI              string `json:"openapi"`
}

var discoveryJSON = discoverySpec{
	Service: "heurchain",
	Version: brokerVersion,
	BaseURL: "http://localhost:8080",
	Auth: discoveryAuth{
		Type: "header",
		Headers: []discoveryHeader{
			{Name: "X-HeurChain-API-Key", Format: "hc_<32-hex>"},
			{Name: "X-HeurChain-Tenant", Format: "uuid"},
		},
	},
	Endpoints: []discoveryEndpoint{
		{
			Path:   "/store",
			Method: "POST",
			Summary: "Write a memory item. text is required unless embedding is provided. " +
				"Pass metadata.session_id from your active session. Ordinary upserts retain revisions; optional expected_revision protects against stale writes. Forgotten IDs cannot be reused.",
			Body: map[string]any{
				"text":      "string (required if no embedding)",
				"agent_id":  "string (recommended; e.g. kimi, openclaw, hermes)",
				"tags":      "string[] (optional; see tag taxonomy)",
				"id":        "string (optional; auto-UUID if omitted)",
				"embedding": "float[] (optional; 768-dim; server embeds if omitted)",
				"metadata":  "object (optional; include session_id, source, project)",
				"namespace": "string (optional; ops|notes|self, default notes) - decay tier; " +
					"'self' for durable facts you want kept longest, 'ops' for short-lived " +
					"operational chatter. Unknown values are rejected with 400.",
				"ontology": "object (optional; heurchain-ontology/v1 assertion; see GET /ontology/capabilities)",
			},
			AuthRequired: true,
		},
		{
			Path:   "/query",
			Method: "POST",
			Summary: "Semantic similarity search. Filter by agent_id or tags. " +
				"Use for natural-language retrieval. Returns id, score, text, tags, metadata, created_at per result.",
			Body: map[string]any{
				"text":      "string (required if no embedding)",
				"embedding": "float[] (optional)",
				"agent_id":  "string (optional filter)",
				"tags":      "string[] (optional; any-tag match)",
				"top_k":     "int (default 10, max 100)",
				"threshold": "number (cosine floor; default 0.0)",
			},
			AuthRequired: true,
		},
		{
			Path:    "/search",
			Method:  "POST",
			Summary: "Hybrid BM25 + dense search fused via RRF. Same body as /query. Better for short keyword lookups.",
			Body: map[string]any{
				"text":      "string (required if no embedding)",
				"agent_id":  "string (optional filter)",
				"tags":      "string[] (optional; any-tag match)",
				"top_k":     "int (default 10, max 100)",
				"threshold": "number (default 0.0)",
			},
			AuthRequired: true,
		},
		{
			Path:    "/embed",
			Method:  "POST",
			Summary: "Generate a 768-dim embedding without storing it.",
			Body: map[string]any{
				"text": "string",
			},
			AuthRequired: true,
		},
		{
			Path:   "/graph/ask",
			Method: "POST",
			Summary: "NOT YET IMPLEMENTED - returns 501. Multi-hop knowledge-graph query over the " +
				"heurgraph relationship index, for questions needing more than one hop " +
				"(\"what depends on X?\"), answered with per-edge provenance. Auth and validation " +
				"are live; traversal is not built. It 501s rather than returning an empty 200, " +
				"because an empty success is indistinguishable from \"no answer\". Do not depend " +
				"on this yet - use /search or /query.",
			Body: map[string]any{
				"query":    "string (required unless seed_ids given)",
				"seed_ids": "string[] (optional; max 32; explicit start entities)",
				"max_hops": "int (optional; default 2, clamped to 1-5)",
			},
			AuthRequired: true,
		},
		{
			Path:   "/session/start",
			Method: "POST",
			Summary: "Open a session to correlate writes across a conversation. " +
				"Returns session_id — pass it in metadata.session_id on every /store call.",
			Body: map[string]any{
				"agent_id":    "string (required)",
				"ttl_seconds": "int (optional; default 86400)",
			},
			AuthRequired: true,
		},
		{
			Path:    "/session/end",
			Method:  "POST",
			Summary: "Close a session. Call at conversation end, then store a session summary.",
			Body: map[string]any{
				"session_id": "string (required)",
			},
			AuthRequired: true,
		},
		{
			Path:         "/session/{session_id}/context",
			Method:       "GET",
			Summary:      "Replay a session: returns up to 50 items written under that session_id, ordered by recency. Query param: n (default 20, max 50).",
			AuthRequired: true,
		},
		{
			Path:         "/agent/{agent_id}",
			Method:       "GET",
			Summary:      "Recall recent memories written by this agent_id. Call at session start to re-orient before any task.",
			AuthRequired: true,
		},
		{
			Path:         "/agents",
			Method:       "GET",
			Summary:      "List distinct agent_ids active in the last 7 days for this tenant.",
			AuthRequired: true,
		},
		{
			Path:         "/memory/manifest",
			Method:       "GET",
			Summary:      "Tenant-wide memory summary: total vectors, counts per tag, counts per agent_id, 10 most-recent items, tag taxonomy. Call at the start of any non-trivial task.",
			AuthRequired: true,
		},
		{
			Path:         "/vault/stats",
			Method:       "GET",
			Summary:      "Storage usage: vector count, byte estimate, oldest/newest item timestamps.",
			AuthRequired: true,
		},
		{
			Path:         "/vault/flush-working-memory",
			Method:       "POST",
			Summary:      "Delete items tagged 'conversation' (or a custom tag set). Use at session end to prune chaff.",
			AuthRequired: true,
		},
		{
			Path:         "/vault/export",
			Method:       "GET",
			Summary:      "Zip archive of every vector item for this tenant, one Markdown file per item (frontmatter + body + metadata block); embeddings excluded, so a restore re-embeds. Use for backup or migration.",
			AuthRequired: true,
		},
		{
			Path:         "/admin/vault/rehydrate",
			Method:       "POST",
			Summary:      "Restore one vaulted document to active Redis, BM25, and Qdrant projections; reports duration_ms and preserves the vault copy on failure.",
			AuthRequired: true,
		},
		{
			Path:         "/promote",
			Method:       "POST",
			Summary:      "Increment a memory item's retrieval weight. Call after a human confirms a stored fact is correct.",
			AuthRequired: true,
		},
		{
			Path:         "/health",
			Method:       "GET",
			Summary:      "Redis/SQLite liveness check; does not check Qdrant.",
			AuthRequired: false,
		},
		{
			Path:         "/ready",
			Method:       "GET",
			Summary:      "Redis/SQLite and authenticated Qdrant read connectivity; 503 on failure, two-second budget. Index coverage is not checked.",
			AuthRequired: false,
		},
	},
	Limits: discoveryLimits{
		MaxTextTokensPerRequest: 512,
		AutoTruncate:            true,
		EmbeddingDimension:      768,
		EmbeddingModel:          "BAAI/bge-base-en-v1.5",
		RateLimitDefaultRPM:     60,
	},
	Gotchas: []string{
		"Call GET /memory/manifest before asking the user for anything — credentials, config, and decisions may already be stored",
		"Call POST /session/start at conversation start; pass session_id in metadata.session_id on every /store",
		"Call GET /agent/{agent_id} at session start to recall your own prior memories",
		"Store selectively — not after every message. Store decisions, facts and session summaries; never secret values or private reasoning",
		"Chunk text into 200-400 token segments for best retrieval; link segments via metadata.session_id",
		"agent_id is platform-level (openclaw, kimi, hermes) — put session-specific data in metadata",
		"Reposting /store with the same id overwrites — safe to retry on transient errors",
		"Tag memories with the standard taxonomy (credential, decision, fact, project-context, system-config, conversation, code) for cross-agent consistency",
		"On 429, back off 60s — response body contains {limit, current}",
		"On 5xx, retry with exponential backoff up to 3 times",
	},
	FirstRun: []string{
		"GET /memory/manifest — see what's already stored before asking the user anything",
		"Store credential locations/purposes only; obtain secrets through the configured client secret store",
		"POST /session/start — open a session; save session_id for all /store calls this conversation",
		"GET /agent/{your_agent_id} — recall your own recent memories before starting work",
		"At session end: POST /session/end, then POST /store a session summary tagged conversation",
	},
	Discovery: discoveryLinks{
		InstructionsMarkdown: "http://localhost:8080/",
		InstructionsJSON:     "http://localhost:8080/ (Accept: application/json)",
		OpenAPI:              "http://localhost:8080/openapi.json",
	},
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	// Go's ServeMux treats "/" as a catch-all. Only serve this handler at the
	// exact root path; let everything else fall through to a 404.
	if r.URL.Path != "/" {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	// CORS: intentionally open — this endpoint is designed to be hit by any
	// agent regardless of origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Accept")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Tenant-aware augmentation: if valid API key + tenant headers are present,
	// build a manifest summary and append. Failures degrade silently — discovery
	// itself remains unauthenticated.
	tenantSummary := ""
	var tenantManifest map[string]interface{}
	apiKey := r.Header.Get(s.config.Auth.APIKeyHeader)
	tenantID := r.Header.Get(s.config.Auth.TenantHeader)
	if apiKey != "" && tenantID != "" && validID.MatchString(tenantID) {
		ctx := r.Context()
		if _, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey); err == nil {
			if m, err := buildManifest(ctx, s.rdb, tenantID); err == nil {
				tenantManifest = m
				tenantSummary = manifestSummaryLine(m)
			}
		}
	}

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if tenantManifest != nil {
			combined := map[string]interface{}{
				"service":             discoveryJSON.Service,
				"version":             discoveryJSON.Version,
				"base_url":            discoveryJSON.BaseURL,
				"auth":                discoveryJSON.Auth,
				"endpoints":           discoveryJSON.Endpoints,
				"limits":              discoveryJSON.Limits,
				"gotchas":             discoveryJSON.Gotchas,
				"first_run_checklist": discoveryJSON.FirstRun,
				"discovery":           discoveryJSON.Discovery,
				"tenant_hint":         tenantSummary,
				"manifest":            tenantManifest,
			}
			_ = enc.Encode(combined)
			return
		}
		_ = enc.Encode(discoveryJSON)
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	body := discoveryMarkdown
	if tenantSummary != "" {
		body += "\n---\n\n## Your tenant\n\n" + tenantSummary + "\n"
	}
	_, _ = w.Write([]byte(body))
}
