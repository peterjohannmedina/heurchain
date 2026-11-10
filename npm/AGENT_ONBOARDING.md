# HeurChain API — Agent Onboarding

**Base URL:** `http://localhost:8080`

You are an AI agent connecting to HeurChain — a persistent vector memory broker.
Read this once at the start of any new session. It answers: what to store, when
to store it, how to retrieve it, and how sessions work.

---

## Authentication

Memory endpoints require two headers (discovery, health and readiness are public):

```
X-HeurChain-API-Key: hc_<32-hex>
X-HeurChain-Tenant: <uuid>
```

**If these are not configured:** use the client secret store or ask the user to
configure the connection. Never log credentials or save secret values in memories.

---

## First-run checklist

Run these steps in order at the start of every new session:

1. **`GET /memory/manifest`** — see what's already stored (tag counts, agent
   counts, 10 most-recent items). Do this BEFORE asking the user for anything
   that might already be in memory (credentials, project context, decisions).
2. **Use the configured connection.** Recall credential locations and purposes,
   never copy secret values into agent memory.
3. **`POST /session/start`** — open a session. Save the returned `session_id`
   and pass it in `metadata.session_id` on every `/store` call this conversation.
4. **`GET /agent/{your_agent_id}`** — recall your agent's own recent memories.
   Review before starting work.
5. **At session end:** call `POST /session/end`, then write a session summary
   (see _What to store and when_ below).

---

## Inspect, correct, forget and checkpoint

POST /memory/get and /memory/list inspect exact records, including archives.
POST /memory/update requires expected_revision and a correction reason. Retained
superseded versions are explicit history, not ordinary recall.
POST /memory/forget requires expected_revision and confirm=true. Verify the
cleanup_complete receipt. It covers that ID and its retained versions, not
backups, independent copies or previously returned client context.
POST /memory/checkpoint saves concise working context; /memory/resume reads it by
ID without dense indexing. Retain checkpoint_id in the harness handoff and verify
stored=true. A host hook or proactive agent call must invoke it; MCP does not
intercept automatic compaction. New snapshots need new IDs.
List/search/query accept conjunctive structured filters for author, namespace,
tags, lifecycle dates and scalar metadata. Search/query return active records.
Keep secrets/private reasoning out of all memories and checkpoints.

## Core write/read endpoints

### `POST /store` — write a memory item

```json
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
```

Optional top-level fields:
- `id` — stable key for this item. Auto-UUID if omitted. Reposting with the same `id` overwrites (safe to retry on transient errors).
- `embedding` (float[]) — bring your own 768-dim vector. Omit to let the server embed via BAAI/bge-base-en-v1.5.
- `namespace` — decay tier: `ops` | `notes` | `self`. Default `notes`.

#### Choosing a namespace

Memories lose activation as they age and gain it each time they are retrieved.
Below a threshold an item is moved to the cold vault, where it is preserved but
**stops appearing in `/search` and `/query` results**. The namespace sets how
fast that clock runs:

| namespace | decays | use for |
|-----------|--------|---------|
| `ops` | fastest | transient operational detail — job output, retry chatter, one-off status |
| `notes` (default) | medium | ordinary working memory |
| `self` | slowest | durable facts about the user, the agent, or the project you want kept |

Pick `self` for anything you would be annoyed to lose in a month. An
unrecognised value returns `400` rather than silently falling back, so a typo
cannot quietly retier your memories.

Response `200`:
```json
{ "id": "uuid-or-your-id", "stored": true }
```

---

### `POST /query` — semantic similarity search

```json
{
  "text":      "what should I retrieve",
  "agent_id":  "kimi",
  "tags":      ["credential"],
  "top_k":     5,
  "threshold": 0.6
}
```

- `agent_id` — optional; filters to that agent's writes only
- `tags` — optional; AND semantics (all tags must match)
- `top_k` — default 10, max 100
- `threshold` — cosine similarity floor, default 0.0

Response `200`:
```json
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
```

---

### `POST /search` — hybrid BM25 + dense search (RRF fusion)

Send text, limit (default 10, maximum 50), qdrant_candidates (default 50,
maximum 200), and optional agent_id/tags. The broker embeds the text and fuses
positive BM25 matches with dense results. Agent matching is exact; tags match
any requested tag. These filters apply before the final result limit.

Results contain source text, metadata, canonical document IDs, and available
BM25/dense ranks and scores. The retrieval object reports backend status,
candidate limits, unresolved IDs and missing text. A failed leg may return a
degraded result, including an empty result; inspect bm25_status/dense_status.

/store projects into both indexes. HTTP 200 with indexed=true
confirms completed indexing. HTTP 202 with stored=true and indexed=false means
the memory is durable but indexing is incomplete: retry the same document ID.
There is no background retry queue.

---

### `POST /embed` — embed without storing

```json
{ "text": "..." }
```

Response: `{ "embedding": [...768 floats...] }`. Use when you control storage yourself.

---

## Session lifecycle

Sessions correlate writes across a conversation so you can replay the full
context of any prior session later.

### `POST /session/start`

```json
{ "agent_id": "kimi", "ttl_seconds": 86400 }
```

Response: `{ "session_id": "ses_..." }`

Save `session_id` and pass it in `metadata.session_id` on every `/store` call
this conversation. `ttl_seconds` defaults to 86400 (24 h) if omitted.

### `POST /session/end`

```json
{ "session_id": "ses_..." }
```

Response: `{ "ok": true }`

Call at conversation end, then immediately store a session summary (see below).

### `GET /session/{session_id}/context?n=20`

Returns up to 50 items written under that session, ordered by recency:

```json
{
  "items": [
    { "id": "...", "text": "...", "tags": [...], "created_at": "..." }
  ]
}
```

Use this to resume a prior conversation: pass the previous session ID to
re-hydrate context without re-querying the user.

---

## Agent recall

### `GET /agent/{agent_id}` — your agent's recent memories

Returns recent memories written by this `agent_id`. Call at session start to
re-orient before starting any task.

### `GET /agents` — list active agents

Returns distinct `agent_id` values from the last 7 days. Use to discover what
other agents have contributed to this tenant's memory.

---

## Memory manifest

### `GET /memory/manifest`

```json
{
  "total":  42,
  "tags":   [{ "tag": "credential", "count": 3 }],
  "agents": [{ "agent_id": "kimi", "count": 18 }],
  "recent": [{ "id": "...", "agent_id": "...", "tags": [...], "created_at": "..." }],
  "tag_taxonomy": ["credential","decision","fact","project-context","system-config","conversation","code"],
  "generated_at": "2026-05-13T18:00:00Z"
}
```

**Always call this at the start of any non-trivial task.**

---

## Vault

### `GET /vault/stats`

Vector count, byte estimate, oldest/newest item timestamps for this tenant.

### `POST /vault/flush-working-memory`

Deletes items tagged `conversation` (or a custom tag set you specify). Use at
session end to prune conversational chaff before it pollutes future retrieval.

### `GET /vault/export`

A zip archive containing every vector item for this tenant, one Markdown file
per item (YAML frontmatter: id, tenant, agent_id, created_at, tags; then the
body text; then the item's metadata as a fenced JSON block). Embeddings are not
included, so a restore re-embeds each body server-side. Use for backup or
migration.

### `POST /admin/vault/rehydrate?tenant_id=...`

Admin-only recovery of one vaulted document. Send a JSON body with a `doc_id` field; the
broker restores Redis, BM25, and Qdrant from the preserved embedding and
returns `duration_ms`. A newer active record returns `409 Conflict`.

---

## What to store and when

**What goes in `text`:** anything that should survive past this conversation.

| Content | Tag | Example |
|---|---|---|
| Credential location | system-config | Which secret store holds the configured connection, without its value |
| Architecture decision | `decision` | `"Chose PostgreSQL over MySQL — reason: JSON operators"` |
| User-stated fact | `fact` | `"The user's email is alex@example.com"` |
| Project layout | `project-context` | `"OpenClaw: VM 242, user rm, pnpm monorepo at ~/openclaw"` |
| Server / env config | `system-config` | `"HeurChain tenant abc-123, key rotated 2026-05-13"` |
| Session summary | `conversation` | One-paragraph summary of what was done and what's pending |
| Code snippet | `code` | Small, self-contained snippet useful across sessions |

**What NOT to store:** raw conversation transcripts, large file contents, data
that changes every run (dynamic UUIDs, ephemeral timestamps), anything that
belongs in a repo or config file.

**When to call `/store`:**

- **Connection configured**: record its purpose and secret-store location only;
  never the API key, token or password itself.
- **Decision made** — when you and the user agree on an approach, architecture
  choice, or policy. Tag `decision`.
- **User states a fact** — name, email, preference, account info. Tag `fact`.
- **Context eviction** — context window filling up; store a summary before
  truncating. Tag `conversation`.
- **Session end** — one-paragraph summary of accomplishments and open items.
  Tag `conversation`.
- **NOT after every message** — that produces retrieval noise. Store selectively.

**Chunking:** max 512 tokens (~2000 chars) per item. Split larger content into
segments; link them via `metadata.session_id` or a shared `id` prefix
(e.g. `doc-plan-1`, `doc-plan-2`).

---

## Tags

Tag rules: lowercase alphanumeric, dash or underscore, max 32 chars, max 16 per item.

**Taxonomy** — use these names for cross-agent consistency:

| Tag | Use for |
|---|---|
| `credential` | Credential locations and purposes; never secret values |
| `decision` | Architecture, tooling, or policy choices |
| `fact` | User-stated facts, preferences, account info |
| `project-context` | Project structure, stack, file locations |
| `system-config` | Server config, env vars, host addresses |
| `conversation` | Session summaries, ephemeral notes |
| `code` | Code snippets, scripts, config files |

---

## Error handling

| Status | Meaning | Action |
|---|---|---|
| `200` | Success | — |
| `400` | Bad request (missing field, malformed JSON) | Fix payload, do not retry |
| `401` | Auth failed (bad/missing key or tenant) | Ask user for correct credentials |
| `404` | Item or session not found | Do not retry |
| `429` | Rate limit exceeded | Body: `{"limit":60,"current":63}`. Back off 60 s then retry |
| `5xx` | Server error | Exponential backoff, max 3 attempts. Report if persistent |

All error responses are JSON: `{ "error": "description" }`.

---

## OpenClaw / Hermes integration

- Set `agent_id` to `"openclaw"` or `"hermes"` consistently across all sessions for this tenant.
- At first boot: store your MCP server list, model routing config, and workspace
  path under tag `system-config`.
- Each session start: call `GET /agent/openclaw` (or `/agent/hermes`) before any task.
- If the HeurChain MCP server is configured, prefer MCP tools
  (`mcp__heurchain__heurchain_search`, `mcp__heurchain__heurchain_export`).
  Fall back to direct HTTP if MCP is unavailable.
- `POST /promote` — increments a memory item's retrieval weight. Call after a
  human confirms a stored fact is correct.

---

## Discovery

- `GET /` — this document (`text/markdown` default; `Accept: application/json` for structured spec). Send tenant headers to get a manifest summary appended.
- `GET /health` — service liveness (no auth)
- `GET /ready` — Redis/SQLite and authenticated Qdrant read connectivity (no auth); 503 if a dependency fails. Does not attest index coverage.
- `GET /memory/manifest` — tenant memory summary (auth required)
- `GET /openapi.json` — OpenAPI 3 spec (no auth)

