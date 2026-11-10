# Adversarial review: what could cap this plan as HeurChain scales

The two expansions and the evaluation are sound for what they claim. These are the ways the approach could become the limiting factor as the corpus, tenant count, client mix or product scope grows. Ordered by expected impact.

## 1. Every expansion is O(corpus) work at query time
- **Lexical search:** BM25 is already a full scan, one Redis GET per (eligible document × query term), with no posting lists (`broker/bm25.go`). The bridge adds two more scans over the candidate set. Inside a conversation that's hundreds of records; in tenant scope (no `sample_id`) it's the whole eligible corpus.
- **The conversation index** (`broker/expansion.go`) is rebuilt by reading the metadata of every active record whenever the tenant's write revision or active-set size changes. In a write-heavy tenant, such as agents storing memories continuously, almost every expanded search pays that O(N) rebuild.
- **The cache** is in-process: per broker, first-in-first-out, 8 tenants. With more than 8 active tenants it thrashes, and multiple brokers each hold their own copy.
- **Redis is single-threaded.** These O(N) pipelines delay other tenants, a noisy-neighbour problem that grows with the V2 multi-tenant plan.
- **The way out is store-time indexes:**
  - term → postings (ideation R1)
  - conversation → ordered turns
  - conversation → member IDs

  Writes stay O(1), and query cost follows the matches rather than the corpus. The read-time design was chosen to avoid migrations for an experiment. It shouldn't ship to large tenants as is.

## 2. Coupled to LoCoMo's data shape
- Both features key on metadata `sample_id` + `dia_id` = `D<session>:<turn>`.
- Real HeurChain clients (Claude Code, Kimi, OpenClaw, Hermes) write through `/agent/store` and sessions with `session_id`, not `dia_id`. For live traffic the features report `no_anchors` and do nothing.
- Growth into new agent types needs one general turn schema (conversation ID, session ID, turn ordinal, speaker, event time). Otherwise each client format becomes a special case.

## 3. Tuned on four synthetic, two-speaker conversations
- About 500 turns and 19–32 sessions each. Real memory spans months, many agents and many more sessions.
- At that scale the number of anchored sessions and bridge candidates grows, and "openers carry the time anchor" is a trait of LoCoMo's chat style that agent logs may not share.
- Defaults (cap 3, 6 bridge terms) were chosen on DEV. The six holdout conversations are the only guard against overfitting. Keep them sealed, and expect real-traffic behaviour to differ.

## 4. Recall bought with volume, not precision
- About 1% of appended text is labelled evidence; the other 99% is filler.
- That text becomes reader input tokens on every answered query, so cost grows linearly with traffic, and long contexts can hurt reading ("lost in the middle", PRD R2).
- Raising `limit` has the same problem, and run 2 shows it is as effective as the expansions at matched bytes.
- The scalable lever is **precision**: support-aware reranking (PRD B2) and better units (PRD C), not more recall by volume.

## 5. Lexical and English-only ceilings
- The bridge can't follow paraphrases, synonyms or other languages.
- The tokenizer, stopwords and temporal cue rules are English-oriented.
- International growth, or users who describe the same entity in different words, needs the dense or graph legs, which this evaluation didn't exercise (lexical-only proxy).

## 6. Architecture gap against the comparator
- On the PRD's DEV results, Mem0 leads temporal 94 vs 72–76.
- That gap more plausibly comes from representation (extracted, consolidated facts) than from retrieval expansion.
- These features are local optimizations with diminishing returns. If they absorb the roadmap, the larger lever (PRD C: adaptive units and versioned consolidation) waits.

## 7. Option sprawl
- This work adds 8 request options on top of graph, annotation, rerank, legacy-scope and fusion settings. The interaction matrix grows faster than the tests, and clients must tune knobs they can't evaluate.
- Prefer a few named presets (for example `recall_profile: conversational`), backed by evaluation, over raw parameters as the public surface.

## 8. Privacy at multi-user scale
- Expansion respects tenant, agent, tag and scope eligibility, but it only stays inside a conversation when records carry `sample_id`.
- In tenant scope, a multi-user tenant whose clients don't set `agent_id` or tags can have the bridge surface another user's records through a shared rare term.
- Before multi-user tenants (the V2 PRD), make conversation or user scoping mandatory for expansions. Don't fall back to tenant scope by default.

## 9. Evidence that transfers poorly
- Evidence coverage on a lexical proxy with approximate ingestion is not answer quality on the hybrid production path.
- The decisions that scale need the Phase-zero trace (ideation §4), a hybrid re-run, and the PRD's judged pilot. Without them, growth decisions rest on proxy metrics.

## What would keep the plan scalable
1. Move to store-time indexes before any default-on use: postings, conversation turn order and membership.
2. Define a general conversation/turn schema in `/store` and `/agent/store`.
3. Spend the next effort on precision (reranking, units) rather than more recall by volume.
4. Require conversation or user scope for expansions in multi-user tenants.
5. Expose presets, not knobs.
6. Re-evaluate on hybrid retrieval, then pilot, before raising any defaults.
