# Lexical bridge (experimental, default off)

A `POST /search` option that runs one deterministic, model-free second retrieval pass to reach evidence connected through an entity the question never names. It targets the multi-hop finding that most misses lacked some labelled support (42 of 49 revised multi-hop misses). It also addresses graph traversal being seeded only from query phrases (`broker/graph_recall.go` `graphQueryPhrases`). It is PRD avenue A2 (ideation Q2a).

It is not benchmark-qualified and claims no answer-quality gain. For the offline evidence-coverage results, see `research/expansion-2026-10-07/EVALUATION.md`.

## Request

| Field | Default | Contract |
|---|---|---|
| `lexical_bridge` | `off` | `on` enables it. |
| `bridge_anchors` | 3 | The top ranked results whose text supplies bridge terms (1..10). |
| `bridge_terms` | 6 | Number of bridge terms (1..16). |
| `bridge_limit` | 3 | Maximum records appended (1..10). |
| `expansion_max_bytes` | 16384 | Appended UTF8 text fits the budget including ranked text, shared with `session_head`; over-budget ranked text is preserved. Metadata/annotations are excluded. |

## How it works

1. **Term selection.** Take tokens (the broker's BM25 tokenizer) of the top `bridge_anchors` ranked results. Drop query tokens, stopwords (`bridgeStopwords`), tokens under 3 bytes and pure numbers. Keep terms that occur in at least two documents but not in every document. Pick the `bridge_terms` with the lowest document frequency (highest BM25 IDF), ties by term.
2. **Candidates.** Eligible records (the same agent, tag and scope set as the ranked search) not already returned. When the anchors carry `sample_id`, candidates are limited to the anchors' conversations (`scope: conversation`). Otherwise all eligible records are used (`scope: tenant`).
3. **Scoring.** BM25 on the bridge terms over the candidates. Only records matching a bridge term qualify. They are ordered by bridge score plus query score, ties by ID.
4. **Appending.** Through the shared expansion path (`broker/expansion.go`): records are re-read and re-validated (active, same conversation, eligible) and appended within the byte cap, up to `bridge_limit`. Each appended result carries `"expansion": {"reason": "lexical_bridge", "anchor_id", "sample_id", "turn_id", "terms"}`. Here `terms` are the bridge terms the record matched, and `anchor_id` is the first ranked anchor that contains one of them.

Session heads, when also enabled, run first. The bridge then sees their additions as already returned and shares the remaining budget.

A bridge candidate's rehydrated current text must still match at least one
selected term. A concurrent overwrite removing every bridge term is skipped
as stale before consuming the result, byte or source quota.

## Contract

Same as `session_head` (`broker/expansion.go`):
- **Default off:** responses without the option are unchanged.
- **Append-only:** ranked results are never removed, reordered or rescored.
- **Scope:** appended records keep the same eligibility as ranked hits and never cross tenant or conversation scope.
- **Byte cap:** appended UTF8 text fits the cap including ranked text; ranked
  records are preserved even when their text alone exceeds it. Metadata,
  JSON framing and annotations need a separate complete-context budget.
- **Source cap:** `max_per_source` counts ranked results and both expansions
  together. Omitted candidates report `skipped.max_per_source`.
- **Fallback:** Redis errors return the input results unchanged (`status: failed`).
- **Counters:** appended records don't bump `access_count`, and `returned_documents` stays ranked-only.

## Diagnostics

`retrieval.lexical_bridge` reports:
- `status`: `applied` | `no_anchors` | `no_terms` | `failed`
- `anchors`
- `terms`: each with its `df`
- `scope`
- `candidates`: records scored
- `matched`: records matching a bridge term
- `added`, `added_bytes`, `start_bytes`, `budget_bytes`
- `skipped`: by reason (`stale`, `ineligible`, `budget`, `max_per_source`)
- `index_cached`, `ms`

## Cost

Two extra BM25 passes over the candidate set: the bridge terms over all candidates, then the query terms over the matched records only. Within a conversation that's a few hundred records; offline it added about 4 ms p50 at `limit` 20. In tenant scope the cost grows with the eligible corpus, like the existing BM25 scan.

## Limits

- Lexical only. It can't bridge paraphrases. A term like a person's name works best.
- Stopwords and the tokenizer are English-oriented.
- Only the `/search` route applies it.

## Operating it

For when an agent should turn this on, what it costs (latency, bytes, Redis work and scaling) and how to verify it ran, see [`docs/retrieval-levers.md`](retrieval-levers.md) and `GET /levers`.
