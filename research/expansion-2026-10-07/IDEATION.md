# HeurChain performance ideation against baseline `8fb9ba6` (retrieval = `7636913`)

Integration review qualification of F9/Q1b/Q5: the saved top-20 context lacks
14 question-evidence occurrences involving 12 distinct turns; this does not
prove absence at every rank. Ten occurrences are turn1/2, and seven across
six frozen-baseline questions have their session represented in packed
context, before cue/scope/session/byte gates. Historical proposal text below
is retained with this correction; lexical-proxy character-count comparisons
also require the measurement qualification in EVALUATION.md.

> **Status (7 Oct 2026):** Q1b (session-head expansion) and Q2a (lexical bridge) are implemented as opt-in `/search` options on branches `claude/session-head-expansion` and `claude/lexical-bridge`. They are evaluated offline on the 4 DEV conversations in [`EVALUATION.md`](EVALUATION.md). Everything else below is still a proposal.

**Baseline:** the self-hosted development line at `8fb9ba6` (7 Oct 2026) ("Publish reviewed source-date candidate, completed results and research"). It adds the `61e81f7` date-annotation candidate as an **opt-in** search flag (`annotate_event_dates`, package `broker/internal/eventdates`), plus `results/` and `research/` (E1–E3).

Default retrieval code is unchanged from `7636913` (the PRD's "revised HC"). The only edits to retrieval files are the new option fields in `recall_options.go` and `rrf.go`. So the findings below hold, though line numbers in those two files shift by 1–2. The frozen build `216e39e` is still on `feature/dot-release-readiness-20261002`.

**Method:** I read the retrieval code at `7636913`. I made no model calls, ran no benchmarks and accessed no holdout, consistent with the PRD's scope. All counts below come from the PRD. Code-derived runtime figures are estimates, not measurements.

---

## 1. Facts in the code that change how the PRD avenues rank

| # | Finding | Where | What it means for the PRD |
|---|---|---|---|
| F1 | Search returns the top `limit` fused records (default 10, max 50) with **no expansion of any kind**: no neighbours, parent or span. | `broker/recall.go:380-465`, `broker/qdrant.go` (limit/candidates defaults) | Avenue A has no implementation to build on. The handoff doc already lists "parent/span expansion" as a pending, separate experiment (`docs/RECALL-INTEGRITY-TEST-HANDOFF.md`, last section). |
| F2 | The byte cap was not binding. The published report says: *"The 16 KB cap never cut evidence; the 20-unit limit binds."* Every category shows `cutByCap: 0`. | PRD §2; `research/HEURCHAIN_VS_MEM0_LOCOMO_DEV_2026-10-07.html`; `research/temporal-diagnostic/TEMPORAL-MISS-DIAGNOSTIC-2026-10-07.json` | The binding constraint is the **20-unit** `limit`, not the 16,384 bytes. Headroom can hold bundles **without displacing any ranked hit**. This is the cheapest safe form of avenue A. |
| F3 | Graph traversal is **seeded only from query phrases** (`graphQueryPhrases`). | `broker/graph_recall.go:74-99, 157+` | A bridge entity that isn't named in the question is reachable only through two hops from a query entity. Expansion seeded from the retrieved answers doesn't exist. That matters for the 42/49 multi-hop misses with absent support. |
| F4 | Temporal support is only an `as_of` validity filter on client-supplied `valid_from`/`valid_until`. There's no intent detection and no event-time field. | `broker/recall.go:133-137`, `broker/recall_options.go:136-166` | Avenue D is greenfield. The runbook confirms it: no recency boost, no supersession inference. |
| F5 | The tokenizer lowercases and splits on non-alphanumerics, with no stemming, stopwords or date normalisation. "2023-05-08" becomes `2023 05 08`, which never matches "May". | `broker/bm25.go:34-44` | Lexical recall for date and inflection mismatches relies entirely on the dense leg. |
| F6 | Of the temporal misses, **36/50 already had all labelled support packed** and 14/50 lacked some. | PRD §3 | Retrieval changes can address **at most ~14** temporal misses (upper bound). The larger temporal lever is reading and presentation (order, dates, abstention), not candidate generation. |
| F7 | Reads mutate state: every search increments `access_count` on returned records. | `broker/recall.go:498`, `broker/vectors.go:494` | This is harmless while `consolidation.promote_live: false` (`deploy/broker.yaml`). But if it's ever on, never-accessed records have activation −∞ and are **vaulted out of retrieval at the next hourly pass** (`broker/consolidation.go:99-102, 201-215`). Pin `promote_live:false` in every benchmark manifest and record `access_count` drift for FR03 replay. |
| F9 | **Missing temporal evidence is never retrieved, and it clusters at session openings.** All 14 unpacked labelled evidence turns across the 46 frozen temporal misses have **no rank** (not in the returned pool). 10 of those 14 are turn 1 or 2 of their session (`D7:1`, `D14:1`, `D17:1` ×2, `D21:1`, `D22:1`, `D27:2`, `D28:1`, `D31:2` ×2). | `research/temporal-diagnostic/TEMPORAL-MISS-DIAGNOSTIC-2026-10-07.json` (`heurchain_temporal_miss_rows`) | Session openers ("Hey! Last week I…") tend to carry the temporal anchor but share few terms with the question. That makes a precise, cheap target: **session-head expansion** (Q1b). The labels are incomplete and come from the frozen build, but the PRD reports identical prompts for the revised build. |
| F8 | Diagnostics give counts and `embedding/retrieval/total/graph_ms`, but no per-leg ranked IDs and scores, no BM25/dense/hydration stage times, and no per-ID drop reasons (eligibility, `max_per_source`, missing text). | `recallDiagnostics` | This is the main Phase-zero gap (FR02, FR03, FR10). See §4. |

---

## 2. Quality ideas, ranked (each one targets a PRD mechanism)

Every idea below has an **offline discriminating test that runs on saved artifacts plus the raw corpus** before any answer call. Support labels are used only to score these tests, never at runtime (FR06).

### Q1. Neighbour bundles that only use spare budget (A1) — do first
- **Mechanism:** after ranking, add ±1 (then ±2) adjacent turns of the top anchors only into unused bytes under the 16,384 cap. Never displace a ranked hit. Pack duplicates once.
- **Code:**
  - At store time, write a per-session order index (Redis ZSET `tenant:{t}:seq:{session}`, score = turn index) when metadata carries a session and turn ID (LoCoMo `dia_id` like `D3:12`).
  - Add an expansion step after hydration in `runRecallSearchWithGraph`, behind an option (`expand: "neighbors"`, default off for FR07).
  - Emit `anchor_id`, `neighbor_ids`, `reason`, `bytes` (PRD A requirements).
- **Offline test:** for the 42 multi-hop and 14 temporal "support absent" misses, compute the **turn distance from the nearest retrieved turn to each missing labelled turn**, plus each question's byte headroom.
- **Decision rule:** build A1 only if a meaningful share of the misses are within ±2 *and* fit in headroom.
- **Risks:** filler turns; wrong-session neighbours. Mitigate by keeping expansion within the same session and stopping at the cap.

### Q1b. Session-head expansion for temporal questions (a narrow, evidence-led A1 variant)
- **Mechanism:** when a question has temporal intent (see Q3's classifier), add turns 1–2 of each session that already has a retrieved anchor, using spare bytes only. It needs the same per-session order index as Q1.
- **Why:** F9 shows 10 of the 14 missing temporal evidence turns are session turns 1–2.
- **Offline test:** with the saved packed IDs, for each of those 14 items count how many already have *another* turn from the same session in the pack. That is the upper bound on what Q1b could recover. Also measure the bytes added across all 122 temporal questions.

### Q2. Bridge expansion seeded from retrieved answers (A2), with two cheap variants
- **(a) Lexical bridge, no extraction cost:**
  1. Take high-IDF terms and capitalised names from the top 1–3 hits that **don't appear in the query**.
  2. Run one second-round BM25 query restricted to the same conversation and tenant.
  3. Union the results with a fixed quota.

  This is deterministic pseudo-relevance feedback that reuses `scoreBM25` (no new index).
- **(b) Graph bridge:** seed one hop from `graph_relations` whose `source_memory_id` ∈ top-k anchors, instead of from query phrases (F3). Only worth it if extraction coverage holds up on real turns (see Q7).
- **Offline test:** on missing-bridge cases, does round 2 retrieve the absent labelled support within k at matched bytes? Compare against Q1. The PRD rejects A2 if neighbours explain the same gains.
- **Risks:** topic drift; wrong-person names. Mitigate with a same-conversation scope and an explicit `reason: lexical-bridge` label (not treated as fact).
- The published report already recommends this direction ("gap-driven second pass", citing EviMem and IRCoT). Q2a is the deterministic, model-free version, which suits PRD Stage one.

### Q3. Temporal endpoint retrieval (D1 + D3, rule-based)
- **Mechanism:**
  - Use a regex intent classifier: `when`, `what date`, `how long`, `before`/`after`, `first`/`last time`, `ago`, `since`, weekday names, month names. It outputs an intent or `unknown`.
  - For duration and order questions, split into **endpoint sub-queries** and give each its own quota. Merge the results under the cap, with ordinary hybrid search as the fallback.
  - No hard time filters, per the PRD rejection rule.
- **Index-side, optional:** add canonical date tokens derived from **session date** metadata (`2023`, `2023-05`, `may`, weekday) to the BM25 term set only. Displayed text is unchanged. Keep session, event and ingestion time distinct (FR04). Don't use `created_at` as event time.
- **Offline test:** run it only on the **14** temporal misses with absent support (F6). Does endpoint retrieval surface both endpoints? Route the 36 "support packed" misses to Q4 and the reader arm instead.

### Q4. Order-only presentation control (B3), the cheapest temporal reading test
- **Mechanism:** identical selected evidence, ordered chronologically by session date with a date header per unit, versus relevance order.
- **Why:** it isolates presentation from retrieval (the PRD requires this before crediting annotations). It's the natural comparator for the `61e81f7` annotation candidate on the 36 support-packed temporal misses.
- **Cost:** packer-only. No broker change.

### Q5. Raise `limit` at matched bytes, as a separate arm (B, larger pool)
- F2 says bytes weren't binding, so the simplest coverage arm is `limit` 20→30/40 under the same 16,384 cap with relevance-order packing.
- F9 lowers the expected gain for temporal questions, because the missing turns weren't ranked anywhere in the returned pool. The PRD requires its own runtime accounting.
- Use it as the baseline any bundle must beat. If a bigger `limit` recovers the same support, bundles aren't needed.

### Q6. Lexical normalisation (light stemming, query stopwords)
- This changes the index, so it needs a reindex. Test it as a lexical-only ablation (`retrieval_mode: lexical`) on saved queries first. Medium priority.
- Note: the BM25 IDF formula (`bm25.go:369`) already makes very common terms near-zero, so stopword pruning is mainly a runtime win (R1).

### Q7. Graph feasibility (F1) before graph investment
- An upstream synthetic graph benchmark run (not included in this publication) extracted **17/24** sources: 7 were retry-exhausted and 3 produced no relationships.
- Measure extraction coverage on real LoCoMo turns before attempting Q2b. If coverage stays low, the lexical bridge (Q2a) is the graph's cheaper competitor.

**Deferred, per the PRD:**
- **C (units):** consider only if Q1 and Q2 show split-fact failures.
- **E (calibrated reader):** a reader arm on identical saved contexts, outside the broker code.
- **G (planner):** paper prototype only.

---

## 3. Runtime ideas (each one produces identical results, so it's a clean runtime-only arm)

All of these leave ranked IDs and order unchanged, so prompt hashes stay identical (PRD §13: "a runtime-only arm gets identical evidence and prompts"). Verify byte-identical results on a replay of the 701 queries, then compare stage timings.

| # | Change | Current behaviour | Expected effect (estimate) |
|---|---|---|---|
| R1 | **BM25 posting lists** (term → `{docID: tf}` hash). Also cache `docCount`, `avgdl` and doc lengths. | One Redis GET per *(eligible doc × query term)*, plus one per doc length, every query. The code says "full-corpus lexical scan, not a posting-list index" (`bm25.go:285-316`). At ~6k turns × ~15 terms that's ~90k GETs in ~47 pipelines per query. | Cost scales with matching postings instead of corpus × terms. This is the largest runtime lever, with exact same scores. |
| R2 | Hydrate with `HMGET text, metadata, agent_id, tags` instead of `HGETALL`. | `HGETALL` (`recall.go:404`) also pulls the stored `embedding` JSON, kilobytes per record (`vectors.go:122`). | Large cut in Redis payload per search. Trivial change. |
| R3 | Skip the Qdrant `has_id` list when every eligible ID is active and unscoped. | Any query embedding with an identity sets `embeddingSpace` (`qdrant.go:381`). That sends **every eligible ID** in the filter (`legacy_recall_scope.go:49-62`) and runs a full `denseEligibleIDs` pass. | Request size drops from O(corpus) to constant in the common case. |
| R4 | Fetch `text` in the eligibility pass only when `exclude_legacy_session_noise` is set. | Scoped searches read `text` for the **whole corpus** (`recall.go:164`). | Less Redis transfer on scoped queries. |
| R5 | Move `bumpAccessCounts` off the hot path, or let benchmarks disable it. | A write on every read (`recall.go:498`). | Small latency gain, and it makes reads replay-safe (F7). |
| R6 | Batch graph traversal per depth (an `IN` query per frontier). | One SQL query per frontier node (`graph_recall.go` ~190). | Matters only if the graph is enabled. |

Measure them in this order: R2 and R3 (trivial, safe), then R1 (the largest).

---

## 4. Phase-zero instrumentation the PRD needs (small, broker-only)

Add an opt-in `trace: true` search option that returns:
- **Per leg:** ranked IDs and scores (BM25, dense, graph), and the fused order before the cut.
- **Drops:** every dropped ID with its reason (`ineligible`, `max_per_source`, `missing_text`, `unresolved_dense`).
- **Stage times:** eligibility, BM25, dense, fusion, hydration and graph in ms, plus Redis command counts and Qdrant request bytes.
- **For bundles (Q1/Q2):** anchor, added IDs, reason, bytes.

This meets FR02, FR03 and FR10, and makes artifact replay possible without model calls. It's the prerequisite for scoring Q1–Q5 offline.

---

## 5. First five offline checks (no answerer or judge calls)

1. **Headroom audit:** bytes used per saved prompt against 16,384 (decides Q1 and Q5).
2. **Neighbour reachability:** turn distance to the missing labelled support for the 56 absent-support misses (decides Q1).
3. **Bridge reachability:** a second-round lexical query from top-hit rare terms on the multi-hop misses (decides Q2a against Q1).
4. **Temporal split:** endpoint sub-queries on the 14 absent-support temporal misses, and an order-only packer variant on the 36 packed ones (decides Q3 and Q4).
5. **Runtime replay:** R2+R3, then R1, on the 701 saved queries. Require identical result hashes, then report stage-latency deltas.

E2 and E3 summaries are now on GitHub (`research/`, `results/three-way-2026-10-07/`). Checks 1–4 still need the per-question **packed ID lists** and the LoCoMo corpus. Those, and `benchmarkluna_answer_collect.py`, aren't in the repo. The dataset terms (`research/DATASET-TERMS.md`) may be why.

---

## 6. Limits of this pass

- I read code, the PRD and the published research summaries. I ran no models or benchmarks. Counts come from the PRD and `research/`. F9 is my own tabulation of the published temporal diagnostic rows.
- The LoCoMo harness and the E1–E3 artifacts are local, so I couldn't check headroom or distances myself.
- I didn't run the pinned Go 1.27.1 toolchain or the test suites.
- File line numbers are for `7636913`. In `8fb9ba6`, `rrf.go` and `recall_options.go` shift by 1–2 lines.
