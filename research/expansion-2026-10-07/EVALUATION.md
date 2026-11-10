# Offline evidence-coverage evaluation: session heads (Q1b) and lexical bridge (Q2a)

_7 Oct 2026. Branch `claude/lexical-bridge`, stacked on `claude/session-head-expansion`, base `main` `8fb9ba6`._

**Integration review correction:** Historical run1/run2 outputs and numeric
tables are preserved. Fields labelled bytes count Python Unicode characters,
not UTF8 or complete source-unit JSON bytes; matched-byte, token-cost and
evidence-per-KB conclusions remain unqualified. The old prefix check compared
dia_id lists, not scores/text/metadata. It loaded and parsed the full master
before selecting DEV, while ingestion/scoring used DEV only. The original
“holdout not accessed/dropped unread” and complete-byte-cap assertions are not
established by that collector. No heldout ingestion or scoring is demonstrated.
The fixed collector requires a registered DEV-only bundle, compares full ranked
records, and counts UTF8 text and complete source-unit JSON separately. No new
retrieval/model run replaced these historical results.

**What this is:** a mechanism check under PRD Stage one. It measures whether each expansion puts more **labelled evidence turns** into the returned context, at what byte cost, and with no change to ranked results.

**What it isn't:** an answer-quality result. There were no answerer, judge, embedding or model calls, the holdout was not accessed, and the evaluation retrieval was lexical-only.

## Setup

| Item | Value |
|---|---|
| Corpus | LoCoMo upstream `snap-research/locomo@3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376` `data/locomo10.json`, sha256 `79fa87e90f04081343b8c8debecb80a9a6842b76a7aa537dc9fdf651ea698ff4` (the revision registered for the benchmark). Not stored in git. |
| Conversations | DEV only: `conv-26`, `conv-30`, `conv-41`, `conv-50`. 2,019 turns and 701 questions, of which 543 are scored non-adversarial (106 multi-hop, 122 temporal, 28 open-domain, 287 single-hop). This matches the PRD counts. The six other conversations are reserved holdout: the script drops them unread. |
| Question IDs | `<sample>-qa-<0-based index>`. Matches all 46 rows of `research/temporal-diagnostic/TEMPORAL-MISS-DIAGNOSTIC-2026-10-07.json` (category and evidence). |
| Broker | Built from this branch, `deploy/native/broker.example.yaml` (`embed.provider: none`, lexical-only), local Redis, fresh tenant. |
| Ingestion | One record per turn via `/store`. Text is `"<speaker>: <text>"` plus `" [shares <blip_caption>]"` when present. Tag `[sample_id]`. Metadata `{sample_id, dia_id, speaker, session_timestamp_literal}`. **This is an approximation, not the pinned benchmark's ingestion.** |
| Queries | `/search`, `retrieval_mode: lexical`, `tags: [sample_id]`, `limit` 20 (the benchmark's binding unit limit), `expansion_max_bytes` 16384. |
| Arms | `control`; `session_head_temporal`; `session_head_always`; `lexical_bridge` (defaults: 6 terms, 3 results, 3 anchors); `both` (session heads temporal + bridge). Plus `control` at `limit` 25/30/40 as matched-byte comparators. |
| Labels | LoCoMo `evidence` dia_ids are used **only** to score. The broker never sees them (FR06). |
| Tool | `tools/expansion-eval/run_eval.py`. Raw outputs are in [`eval/run1-uncapped/`](eval/run1-uncapped/) (before the session cap) and [`eval/run2-session-caps/`](eval/run2-session-caps/) (cap sweep): summaries and slim per-question flags (IDs only, no text). |

## Integrity checks (pass)

- **Ranked prefix identical to control** in every expansion arm for all 543 questions, in every run (`limit` 20, 25, 30 and 40). Expansion never changed which ranked results were returned, their order or their scores (FR07).
- **No response exceeded the 16,384-byte cap** in any arm (FR05).
- Turns stored: 2,019 of 2,019.

## Run 1 results at `limit` 20 (before the session cap)

Run 1 used uncapped session heads, equivalent to `session_head_max_sessions: 50`. Run 2 below reproduces these control, uncapped and bridge numbers exactly.

Labelled evidence turns present in the returned context. "Full" counts questions whose labelled evidence was entirely present.

| Arm | Multi-hop evidence (of 270) | Temporal (of 131) | Single-hop (of 294) | Open-domain (of 45) | All (of 740) | Full-support questions (of 543) |
|---|---|---|---|---|---|---|
| control | 88 | 96 | 192 | 11 | 387 | 295 |
| session_head_temporal | 92 (+4) | **107 (+11)** | 197 (+5) | 11 | 407 (+20) | 311 (+16) |
| session_head_always | 108 (+20) | 107 (+11) | 199 (+7) | 13 (+2) | 427 (+40) | 315 (+20) |
| lexical_bridge | **97 (+9)** | 98 (+2) | 201 (+9) | 11 | 407 (+20) | 309 (+14) |
| both | 101 (+13) | 107 (+11) | 206 (+14) | 11 | 425 (+38) | 323 (+28) |

Newly fully supported questions:
- **Session heads (temporal):** temporal 11, single-hop 5.
- **Bridge:** multi-hop 3, single-hop 9, temporal 2.
- **Both:** multi-hop 3, single-hop 14, temporal 11.

### Cost and precision

| Arm | Appended turns (543 q) | Appended that are labelled evidence | Returned bytes p50 / p90 / max | Retrieval ms p50 / p95 | Expansion ms p50 |
|---|---|---|---|---|---|
| control | 0 | n/a | 2,993 / 3,809 / 4,693 | 14 / 19 | n/a |
| session_head_temporal | 4,911 | 20 (0.4%) | 3,559 / 8,455 / 9,995 | 14 / 19 | 0 (cached index) |
| session_head_always | 12,103 | 40 (0.3%) | 7,557 / 9,114 / 10,953 | 14 / 19 | 0 |
| lexical_bridge | 1,574 | 20 (1.3%) | 3,489 / 4,335 / 5,321 | 18 / 25 | 4 |
| both | 6,477 | 38 (0.6%) | 4,102 / 8,922 / 10,837 | 18 / 24 | 0 + 4 |

The temporal gate applied to 122/122 temporal questions and to 93/421 others (80 single-hop, 12 multi-hop, 1 open-domain). The bridge applied to all 543 questions, appending 2.9 turns on average.

## Matched-byte comparator: just raise `limit`

| Configuration | Multi-hop | Temporal | Single-hop | All evidence | Full-support | Bytes p50 / p90 |
|---|---|---|---|---|---|---|
| control `limit` 20 | 88 | 96 | 192 | 387 | 295 | 2,993 / 3,809 |
| control `limit` 25 | 94 | 101 | 202 | 409 | 314 | 3,751 / 4,716 |
| control `limit` 30 | 97 | 104 | 208 | 421 | 324 | 4,567 / 5,613 |
| control `limit` 40 | 109 | 108 | 216 | 446 | 338 | 6,090 / 7,505 |
| bridge, `limit` 20 | 97 | 98 | 201 | 407 | 309 | 3,489 / 4,335 |
| session heads (temporal), `limit` 20 | 92 | 107 | 197 | 407 | 311 | 3,559 / 8,455 |
| both, `limit` 20 | 101 | 107 | 206 | 425 | 323 | 4,102 / 8,922 |
| both, `limit` 30 | 110 | 114 | 221 | 457 | 349 | 5,893 / 11,962 |

## Reading the results

1. **Raising `limit` is a strong, simple baseline.**
   - At similar median bytes, `limit` 25 matches either expansion on total evidence (409 vs 407), with a smaller tail.
   - The PRD's binding-limit finding holds here: a bigger pool recovers a lot.
2. **The bridge earns its place on multi-hop.**
   - At `limit` 20 it reaches 97 multi-hop evidence turns: as many as `limit` 30 (4,567 median bytes), at about 3,500 median bytes.
   - Its appended turns are the most precise of the three expansions (1.3%), and the tail stays small (p90 4.3 KB).
   - It costs about 4 ms of extra lexical scoring per query.
3. **Session heads (temporal) earn their place on temporal evidence, but they are costly.**
   - They give +11 temporal evidence turns, more than `limit` 30 adds (+8) and close to `limit` 40 (+12).
   - But precision is 0.4% and p90 bytes more than double, because every anchored session gets its openers.
   - The upper bound check agrees with the design: the control missed 353 evidence turns, 68 were session turns 1–2, and **40** of those sat in a session that already had a ranked turn. `session_head_always` recovered exactly 40.
4. **The mechanisms are complementary.** `both` at `limit` 30 (457) beats `limit` 40 alone (446) at a lower median size, though with a heavier tail.

## Run 2: capping anchored sessions (`session_head_max_sessions`)

Run 1 showed session heads appending openers for every anchored session. This run adds `session_head_max_sessions`: only the first N anchored sessions, by the rank of their best returned turn, are expanded. The broker was rebuilt from the merged branch and the corpus re-ingested into a fresh tenant. Every arm and run again kept the ranked prefix identical to the control, with no response over the cap.

| `limit` 20 | Multi-hop | Temporal | Single-hop | All evidence | Full-support | Appended (evidence) | Precision | Bytes p50 / p90 |
|---|---|---|---|---|---|---|---|---|
| control | 88 | 96 | 192 | 387 | 295 | 0 | n/a | 2,993 / 3,809 |
| session heads, uncapped | 92 | 107 | 197 | 407 | 311 | 4,911 (20) | 0.4% | 3,559 / 8,455 |
| session heads, cap 1 | 88 | 98 | 194 | 391 | 299 | 346 (4) | 1.2% | 3,112 / 3,963 |
| session heads, cap 2 | 88 | 99 | 194 | 392 | 300 | 724 (5) | 0.7% | 3,246 / 4,143 |
| session heads, cap 3 (new default) | 88 | 101 | 194 | 394 | 302 | 1,113 (7) | 0.6% | 3,382 / 4,397 |
| session heads, cap 5 | 89 | 104 | 194 | 398 | 305 | 1,887 (11) | 0.6% | 3,558 / 5,136 |
| lexical bridge | 97 | 98 | 201 | 407 | 309 | 1,574 (20) | 1.3% | 3,489 / 4,335 |
| bridge + session heads cap 3 | 97 | 101 | 203 | 412 | 314 | 2,683 (25) | 0.9% | 3,906 / 4,922 |
| control `limit` 25 | 94 | 101 | 202 | 409 | 314 | n/a | n/a | 3,751 / 4,716 |
| control `limit` 30 | 97 | 104 | 208 | 421 | 324 | n/a | n/a | 4,567 / 5,613 |

At `limit` 30 the pattern repeats:
- control: 421 total, temporal 104
- session heads cap 3: 427, temporal 108, p90 6.2 KB
- bridge: 439, multi-hop 105
- bridge + cap 3: 443, p90 6.7 KB
- control at `limit` 40: 446, p90 7.5 KB

**What the cap shows**
- Capping fixes the byte tail (p90 8.5 → 4.4 KB at cap 3) but removes most of the temporal gain (+11 → +5).
- **At matched bytes, capped session heads are dominated by simply raising `limit`.** `limit` 25 (p90 4.7 KB) reaches the same temporal evidence as cap 3 (101) and more evidence overall (409 vs 394). Cap 5 reaches `limit` 30's temporal evidence (104) but far less overall (398 vs 421).
- **The lexical bridge is the only expansion that holds an edge at matched bytes, and only on multi-hop:**
  - against `limit` 25: 97 vs 94 at a lower median (3.5 vs 3.8 KB)
  - against `limit` 30: 105 vs 97
- Bridge plus capped session heads at `limit` 20 roughly equals `limit` 25 (412 vs 409) at similar bytes.
- The default cap is 3, keeping session heads bounded if anyone enables them. The evidence does not support piloting session heads ahead of a larger `limit`.

## Caveats

- **Lexical-only stand-in.** The tested production path was hybrid (dense active). The control here finds fewer turns than the real system, and gains may shrink once dense candidates are present.
- **Ingestion format differs** from the pinned benchmark's local harness. That harness isn't in this repository.
- **Evidence coverage is not sufficiency or answer quality.** The PRD warns that support labels are incomplete and that added turns can distract a reader. Low precision means most appended text is filler.
- **Four DEV conversations are dependent clusters,** not 543 independent samples. There is no significance claim.
- **Latency depends on load.** `limit` 20 ran with one client. `limit` 25, 30 and 40 ran as three concurrent clients against the same broker, so their retrieval times (control 26–27 ms p50) include Redis contention and aren't comparable with `limit` 20 (13 ms). Within the same load, `limit` 25–40 latency was flat. Latency in `docs/retrieval-levers.md` is reported with this split.

## Recommended next steps (no paid calls)

1. **Session-head cap: done (run 2).** Capped session heads don't beat a larger `limit` at matched bytes, so drop them from the pilot shortlist. Revisit only if a hybrid (dense) run shows a different picture.
2. **Make `limit` 25–30 the control for any answer pilot,** since the binding constraint is the unit limit, not bytes.
3. **Re-run with the dense leg** (a frozen embedding model and a fresh index) to check that the gains survive hybrid retrieval.
4. **Only then** pick at most two arms for the PRD's approved DEV answer pilot. Current shortlist: the lexical bridge against control at a byte-matched `limit` (25–30). See `ADVERSARIAL-REVIEW.md` for the scaling risks to weigh first.
