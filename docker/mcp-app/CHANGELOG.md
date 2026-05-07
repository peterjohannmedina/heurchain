# HeurChain Changelog

## v1.3.0 — 2026-05-07

### New: Frictionless REST layer

- `GET /api/session-context?agent=X&hint=Y` — token-budgeted payload (proc knobs + last session + top BM25 hits) designed for system-prompt injection via hook at session start. Zero MCP tool calls required.
- `GET /api/search?q=...` — BM25 search over the full index via plain HTTP. No SSE session, no tool definition overhead.
- `POST /api/buffer` — fire-and-forget turn capture. Flushes to Redis on 5-min heartbeat or session end. No agent action required.
- `POST/GET/DELETE /api/proc` — procedural memory tier. Stable agent preferences (coding style, language, patterns) stored with no TTL or decay.

### New: Slim MCP profile

- Connect with `?profile=slim` on `/sse` to load 14 core tools instead of 29 (~750 token savings on tool definitions per context window).
- Infrastructure tools (Prometheus, Grafana, Proxmox, Ceph) only load on `?profile=full`.

### New: MCP tools

- `proc_set` / `proc_get` — read and write procedural memory from within an MCP session.

### Fix: BM25 index was Redis-blind

- Silent bug in node-redis `scanIterator` caused it to yield key pages (arrays) instead of individual keys. Every `redis.get(array)` call failed silently, so BM25 was built from Obsidian vault files only — Redis keys were never indexed. Fixed via `scanKeys()` normalizer. Index now correctly covers both tiers (465 docs vs 447 before).

---

## v1.2.1 — 2026-05-06

### Security

- Path traversal fix: `validateKey()` now rejects `..` segments, null bytes, and slash characters before resolving vault paths.
- Bearer token auth (`MCP_AUTH_TOKEN`) enforced on all endpoints. `/health` remains open for healthcheck probes.
- Grafana tool fails closed when credentials are not configured rather than exposing unauthenticated access.

### Fixes

- SSE session cap (50) with cleanup on both `req.on("close")` and `res.on("close")` to prevent transport leaks.
- BM25 rebuild is atomic: `isBuilding` flag + `pendingOps` queue replays writes that arrived during a rebuild onto the fresh index after swap. No silent memory loss during hourly rebuild.
- `bulkAdd()` for O(N log N) index builds instead of O(N²) per-insert `_recalcStats()` calls.
- Per-key try/catch in `buildIndex` Redis scan — a single bad key type no longer aborts the entire scan.
- Tier validation: unknown tier strings fall back to `"all"` instead of returning empty results.

### Features

- `heurchain_export` — glacier sync: exports Redis longterm-tier entries to Obsidian vault as markdown. Idempotent.
- `minScore` parameter on `heurchain_search` to filter low-signal results.
- Namespace isolation: `namespace` parameter scopes search and write operations to an agent-specific key prefix.
- Session noise filter: suppresses low-signal session metadata keys from BM25 results.

---

## v1.2.0 — 2026-05-05

### Features

- Native BM25 search (Okapi BM25, k1=1.5, b=0.75) implemented in-process. Replaced dead broker call to port 3012.
- Redis integration: `redis-cache` service added to Docker stack; `REDIS_URL` fixed to use Docker service name.
- `heurchain_search` now indexes all Redis string keys + Obsidian vault `.md` files on startup with hourly rebuild.

---

## v1.1.0 — initial release

- MCP SSE server with 27 tools across cache, vault, search, monitoring, infrastructure, and user context categories.
- Tiered storage: Redis (longterm) + Obsidian vault (working).
- nginx gateway for SSE proxying.
- Ansible role for managed host deployment.
