# Retrieval levers: operating guide for agents

Historical run2 size fields retain their original numbers, which count Unicode
characters rather than UTF8 bytes. Matched-byte, token-estimate and evidence-per-KB
comparisons remain unqualified; see the correction in `EVALUATION.md`. No
answer-quality result is claimed. `expansion_max_bytes` bounds appended UTF8
text including ranked text; it preserves already-over-budget ranked records
and excludes metadata/annotations. Enforce complete source JSON and actual
reader token limits separately.

How an agent chooses `POST /search` options per request, what each one costs, and how to check that it ran.

The authoritative, machine-readable form is **`GET /levers`** (no auth), the embedded copy of [`broker/retrieval_levers.json`](../broker/retrieval_levers.json). Read that file for the exact values, bounds, defaults, dependencies and per-lever performance. This page explains the model and the trade-offs. `TestRetrievalLeverManifest` fails the build if the manifest drifts from the code (fields, enum values, bounds, defaults, dependencies, handler clamps, and the code constants quoted in the performance notes).

## Control model

- **Per request, within operator controls.** Levers are top-level fields of the `/search` body, next to `text`. Nothing persists between requests.
  - Operators can set deployment defaults and a kill switch (see Operator controls below).
  - The active settings are the `operator` member at the end of `GET /levers`.
- **Defaults:**
  - An omitted field takes the operator default if there is one, otherwise the documented default.
  - Sending `0` or `""` selects the documented default.
  - Every expansion and experimental lever defaults off, so with no operator defaults a request without them behaves exactly as before.
- **Invalid values:** `enforcement: rejected` fields return HTTP 400 naming the field. `limit` and `qdrant_candidates` are clamped instead.
- **Dependencies:** a lever's `with` names the values it needs. `with_missing` says what happens without them:
  - `rejected` (HTTP 400): the expansion parameters, `expansion_max_bytes`, and `graph_mode` outside hybrid.
  - `ignored` (accepted, no effect): `graph_hops`, `graph_weight`.
- **Safety invariants** (`safety_invariants` in the manifest):
  - Expansions only append after the ranked results and carry an `expansion` object.
  - They obey the same scope filters as ranked results.
  - They stay within `expansion_max_bytes`.
  - On any error they return the ranked results unchanged with status `failed`.

## Decision procedure

The manifest's `decision_policy` in order:

1. **Session start:** read `GET /levers` once and note its `operator` member. A disabled lever will be forced off or rejected, and an operator default changes what an omitted field means.
2. **Scope.** Set `agent_id` and/or `tags` to the caller in multi-user tenants. Scoping is the privacy control, and it shrinks the BM25 scan.
3. **Pick a budget profile** (below): latency, tokens, recall or shared backend.
4. **Conversational QA:** raise `limit` to 20–30 before any expansion.
5. **Multi-hop** (the link entity isn't named in the question): `lexical_bridge: "on"`, with `expansion_max_bytes` set to the reader's budget.
6. **Temporal, when `limit` can't rise:** `session_head: "temporal"` (cap 3) and/or `annotate_event_dates: true`.
7. **Identifiers in the query:** `rerank_mode: "exact_identifiers"`.
8. **Point-in-time, with validity metadata:** `as_of`.
9. **After every request:** read `effective_mode`, the expansion `status` values and the timings. `disabled`, `no_anchors`, `no_terms` or `failed` means the lever did nothing.
10. **Bridge `scope: tenant`, or `index_cached: false` on most requests:** add `sample_id` or tags scoping, or turn the expansion off.
11. **Never** use `decision_mode` (disabled), or `retrieval_mode: "dense"` without checking dense health (it returns 503).

## Operator controls (deployment)

Operators set these in the broker config. They're read at startup, so restart the broker to apply a change; an invalid setting stops startup with a message naming the field.

```yaml
retrieval:
  defaults:                 # used when a request omits the field
    limit: 20
    lexical_bridge: "on"
    expansion_max_bytes: 16384
  disabled: [session_head]  # kill switch
  disabled_action: ignore   # ignore (force off and report) | reject (HTTP 400)
```

- **Defaults:**
  - Any search field can be defaulted except `agent_id`, `tags` (caller scope), `as_of` (a fixed instant) and `decision_mode`. Each lever's `operator_default` says so.
  - Values are validated like a request, but `limit` and `qdrant_candidates` must be in range rather than clamped.
  - A field the request sends, including `"off"`, `false` or `0`, always wins.
  - Dependent fields (for example `bridge_terms`) can be defaulted on their own. They apply only to requests that turn their parent lever on.
  - A defaulted `expansion_max_bytes` is dropped when no expansion is on, and a defaulted `graph_mode` is dropped for lexical or dense requests.
- **Kill switch:**
  - Covers `session_head`, `lexical_bridge`, `graph_mode`, `rerank_mode` and `annotate_event_dates` (`kill_switch: true` in the manifest).
  - A disabled lever is forced off for every request, together with its dependent fields.
  - With `ignore`, the search runs and `retrieval.operator.ignored` names the lever. With `reject`, the request fails with HTTP 400: "<lever> is disabled on this deployment".
  - Requests are validated before a kill switch clears fields. Invalid explicit values and dependencies remain HTTP 400 errors, including under `ignore`.
  - A default may not turn on a disabled lever.
  - Dense retrieval is switched off with `embed.provider: none`, not here.
- **Where it shows:**
  - The `operator` member of `GET /levers`: `{defaults, disabled, disabled_action}`.
  - `retrieval.operator` on each search when a policy is set: `disabled`, `defaults_applied` and `ignored`.
  - It also covers the MCP search tool, which uses the same handler.
- **When to use the kill switch:**
  - Redis contention: turn off `lexical_bridge` first, since it is the only lever with measurable query cost.
  - Write-heavy tenants: turn off `session_head` and `lexical_bridge`, which pay cold index rebuilds.
  - Graph backends that are slow or unhealthy: turn off `graph_mode`.
  - Any lever under investigation.
- **Defaults to consider:**
  - `limit` 20–30 for conversational deployments; it was the strongest simple lever.
  - `expansion_max_bytes` sized to the reader's budget.
  - `lexical_bridge: "on"` only after a hybrid re-evaluation.

## Performance

### What every search costs

Symbols:
- **N** = active records in the tenant
- **E** = eligible records after scope filters
- **T** = distinct query terms (at most 64)
- **L** = `limit`
- **P** = `qdrant_candidates`

| Step | Work | Notes |
|---|---|---|
| Active set | `SMEMBERS`, O(N) | Always. |
| Eligibility | One `HMGET` (agent_id, tags, metadata, text) per active record, 128 per pipeline | Only when `agent_id`, `tags`, `as_of`, `memory_kind` or a legacy field is set. O(N) records, including their text. |
| BM25 | E × (T+1) `GET`s, plus T+2 per 128-record pipeline | hybrid and lexical. A full scan: no posting lists. |
| Dense | 1 embedding call, plus 1 Qdrant search for P | hybrid and dense. Concurrent with BM25. Skipped on an embedding-cache hit. |
| Graph | SQLite traversal, at most 256 edges | `graph_mode: "on"`. Concurrent. |
| Hydration | One `HGETALL` per returned record | L normally; up to 200 with identifier rerank; more than L with `max_per_source`. |
| Access count | One `HINCRBY` per ranked result | Every search writes; appended records are not counted. |

So the cost of a search grows with tenant size (N), not with `limit`. Only `hybrid` and `dense` call a model: one embedding per search.

### Lever costs at a glance

| Lever | Model calls | Latency | Response bytes | Backend work / scaling |
|---|---|---|---|---|
| `limit` | 0 | Flat across 25–40 (26–27 ms p50 at equal load) | About 150 B per turn | L `HGETALL` + L `HINCRBY` |
| `retrieval_mode` | 1 embedding (hybrid, dense) | `+embedding_ms` (timeouts 15 s OpenAI-compatible, 30 s Ollama) | — | lexical: E×T; dense: ANN search, no BM25 |
| `agent_id`, `tags`, `as_of`, `memory_kind`, legacy fields | 0 | Eligibility pass | — (better relevance per byte) | O(N) reads; BM25 shrinks from N×T to E×T |
| `qdrant_candidates` | 0 | Larger Qdrant reply | — | Linear in P; not measured |
| `rrf_k`, `*_weight` | 0 | None | — | Arithmetic only |
| `max_per_source` | 0 | Extra hydration to refill | — | Extra `HGETALL` per dropped record |
| `graph_mode`, `graph_hops` | 0 at query time | Concurrent, `graph_ms` | — | Capped at 256 edges |
| `rerank_mode` | 0 | Only with identifiers: up to 200 hydrations | — | Bounded by P |
| `annotate_event_dates` | 0 | CPU only | Adds `date_annotations`, **outside** `expansion_max_bytes` | None |
| `session_head` | 0 | 0 ms with a warm index; cold rebuild after writes | Cap 3: p90 3.8 → 4.4 KB; uncapped 8.5 KB | Warm: independent of N. Cold: O(N) |
| `lexical_bridge` | 0 | +4 ms p50 alone, +8–9 ms under contention (about +40%) | p90 3.8 → 4.3 KB | Two more BM25 passes over C candidates; tenant scope makes C = E |
| `expansion_max_bytes` | 0 | None | Appended UTF8 text fits remaining budget after ranked text; metadata/annotations need separate caps | None |

### Measured (offline, indicative)

The environment:
- one broker and local Redis on a 4-vCPU VM
- lexical-only
- N 2,019 records in 4 LoCoMo DEV conversations, each search tag-scoped (E about 500)
- 543 questions per configuration

`limit` 20 ran with one client. `limit` 25–40 ran with three concurrent clients. Source: [`research/expansion-2026-10-07/EVALUATION.md`](../research/expansion-2026-10-07/EVALUATION.md) run 2.

| Configuration | Clients | Retrieval ms p50 / p95 | Stage ms p50 | Bytes p50 / p90 | Evidence turns (of 740) | Extra evidence per extra KB (p50) |
|---|---|---|---|---|---|---|
| control, `limit` 20 | 1 | 13 / 19 | — | 2,993 / 3,809 | 387 | — |
| session_head cap 3, `limit` 20 | 1 | 13 / 19 | 0 | 3,382 / 4,397 | 394 | 18 |
| session_head uncapped, `limit` 20 | 1 | 13 / 19 | 0 | 3,559 / 8,455 | 407 | 35 (but p90 ×2.2) |
| lexical_bridge, `limit` 20 | 1 | 18 / 24 | 4 | 3,489 / 4,335 | 407 | **40** |
| bridge + session_head cap 3, `limit` 20 | 1 | 18 / 24 | 0 + 4 | 3,906 / 4,922 | 412 | 27 |
| control, `limit` 25 | 3 | 26 / 38 | — | 3,751 / 4,716 | 409 | 29 |
| control, `limit` 30 | 3 | 26 / 37 | — | 4,567 / 5,613 | 421 | 22 |
| control, `limit` 40 | 3 | 27 / 38 | — | 6,090 / 7,505 | 446 | 19 |
| lexical_bridge, `limit` 30 | 3 | 36 / 49 | 8 | 5,041 / 6,176 | 439 | — |

What this means for an operating agent:
- **`limit` is cheap in time and linear in bytes.** It is the default way to buy recall.
- **Contention doubles latency.** Three clients took control from 13 to 26 ms p50. Redis is single-threaded, so every agent and tenant on a node shares the scan cost.
- **The bridge is the most byte-efficient lever** for multi-hop, and the only one that costs noticeable time.
- **Session heads cost no time but buy little.** At matched bytes, `limit` 25 is better.
- **Tokens:** reader input is about bytes ÷ 4 for English text (a rule of thumb, not a tokenizer count).

The full tables (all limits and arms) are in `performance_model.measured` in the manifest.

### Conversation index cache

`session_head` and `lexical_bridge` (in conversation scope) share an index of conversation turns:
- **Where:** in memory, per broker process. It holds 8 Redis-client/tenant entries, first in, first out. Different backend clients cannot share a tenant's index.
- **Stays warm across searches:** access-count updates don't invalidate it.
- **Rebuilt (O(N) metadata pass) after:**
  - any write to the tenant: store, replace, delete, vault move or rehydrate
  - eviction by a ninth Redis-client/tenant entry
  - a restart
- **Check it:** `retrieval.session_head.index_cached` and `retrieval.lexical_bridge.index_cached` report it per request.
- **Write-heavy tenants pay the rebuild on almost every expanded search.** Prefer `limit` there.

### Scaling limits

From [`ADVERSARIAL-REVIEW.md`](../research/expansion-2026-10-07/ADVERSARIAL-REVIEW.md):
- BM25, eligibility and cold index builds are all O(N) per search.
- Large tenants slow every tenant on the same Redis.
- More than 8 Redis-client/tenant entries using expansions on one broker evict each other's indexes.
- A bridge without `sample_id` repeats the full scan and can cross users in an unscoped multi-user tenant.
- Appended text is 0.4–1.3% labelled evidence, so reader cost scales with traffic.
- The fix is store-time indexes, not per-request tuning. Until then, keep expansions per request and scoped.

### Budget profiles

| Profile | When | Do | Check |
|---|---|---|---|
| `latency_critical` | Search is on the user's critical path | Hybrid only while `embedding_ms` fits, else lexical. Expansions off unless the question type needs them. Never a tenant-scope bridge. `graph_hops` 1. | `retrieval.total_ms` |
| `token_critical` | Reader context or input spend binds | Set `expansion_max_bytes` to the remaining budget (about 4 × tokens). Keep `limit` minimal. Bridge for multi-hop. Never uncapped `session_head`. Count annotation bytes separately. | Ranked plus added bytes |
| `recall_critical` | Offline or batch | `limit` 30–40, bridge on, `session_head` temporal cap 3–5, a generous `expansion_max_bytes`. | `skipped.budget` is 0 |
| `shared_backend` | Many agents on one Redis | Prefer `limit` over expansions. Always scope. Avoid expanded searches straight after bulk writes. Watch p95. | `retrieval_ms` p95 |

## Verify and self-measure

Every lever has a `verify` path in the manifest. The cost fields to read on your own tenant (figures above come from a small benchmark tenant):

| Field | Meaning |
|---|---|
| `retrieval.total_ms`, `retrieval_ms`, `embedding_ms`, `graph_ms` | Time spent; `embedding_cache_hit` shows a free embedding |
| `retrieval.eligible_documents` | E, the size of the BM25 scan |
| `retrieval.session_head.{status, ms, index_cached, added, added_bytes, ranked_bytes, capped_sessions}` | Session-head effect and cost |
| `retrieval.lexical_bridge.{status, ms, index_cached, scope, candidates, matched, added, added_bytes, terms}` | Bridge effect and cost (C = candidates, M = matched) |
| `skipped.budget` in either expansion object | Records dropped at `expansion_max_bytes` |

To decide whether a lever pays off on your tenant, send the same question with and without it, then compare `total_ms` and the added bytes against what the answer needed.

## Recipes

```json
{"text": "<question>", "limit": 20, "tags": ["<conversation>"]}
{"text": "<question>", "limit": 20, "tags": ["<conversation>"], "lexical_bridge": "on", "expansion_max_bytes": 16384}
{"text": "<question>", "limit": 20, "tags": ["<conversation>"], "session_head": "temporal", "session_head_max_sessions": 3, "annotate_event_dates": true}
{"text": "<question with ID>", "limit": 10, "rerank_mode": "exact_identifiers"}
```

## Evidence scope

The measured figures come from:
- a lexical-only local broker
- four LoCoMo DEV conversations
- labelled-evidence coverage, not answer quality
- no model calls

Latency comes from one VM. Re-evaluate on hybrid retrieval, and measure your own tenant, before relying on these numbers.

## Maintaining the manifest

Edit `broker/retrieval_levers.json` whenever a `/search` field, bound, default or cost changes, then run `go test ./...` in `broker/`. `TestRetrievalLeverManifest` checks:
- every `RecallOptions` JSON field plus `limit`, `qdrant_candidates`, `agent_id` and `tags` is documented, and nothing else
- each enum value is accepted, and an unknown value is rejected
- min and max are accepted, and values just outside them are rejected
- defaults equal what `normalize()` fills in
- `with` and `with_missing` match what `normalize()` does
- the handler clamps for `limit` and `qdrant_candidates` match their constants
- `operator_default` and `kill_switch` match the operator policy code (`broker/retrieval_policy.go`)
- recipes are valid
- the quoted constants (64 query terms, 8 cached Redis-client/tenant entries, 256 graph edges, 256 bridge terms, 4 × `bridge_limit` hydrations, 200 candidates) match the code
