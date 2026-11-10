# Findings from the self-hosted research line

These findings come from the self-hosted development line (October and November 2026).
Short commit hashes in the reports, such as `216e39e`, `7636913`, `8fb9ba6` and `61e81f7`,
identify snapshots of that line. They are labels for reproducibility, not commits in this
repository's history.

**What ships here:** the broker in [`broker/`](../broker) is the frozen **0.6.1** release.
The experimental retrieval levers studied below (session-head expansion, lexical bridge,
source-date annotations, operator lever defaults) are **not** part of the 0.6.1 broker.
Their designs are kept in [`design-notes/`](design-notes) as research, not as supported
features.

## 1. Retrieval quality against Mem0 (LoCoMo DEV, 4 conversations)

[Completed three-way comparison](../results/three-way-2026-10-07/RESULTS.md): 701 answers per
system and 543 paired binary verdicts from one local LLM judge pass.

| System | Correct / 543 |
|---|---:|
| HeurChain, frozen baseline | 360 (66.3%) |
| HeurChain, reviewed retrieval fixes | 363 (66.9%) |
| Mem0 OSS | 369 (68.0%) |

- HeurChain leads on single-hop (226 vs 212) and ties on multi-hop (57 vs 57).
- Mem0 leads on temporal questions (94 vs 72).
- The differences are within a few answers on 543 questions. One judge pass on four
  conversations does not establish human-checked accuracy or a general advantage. Six
  held-out conversations remain sealed.
- Mem0 used more model spend, because it runs an extraction model on every write;
  HeurChain stores text verbatim.

## 2. Why temporal questions are missed

[Temporal diagnostic](temporal-diagnostic/README.md), a manual review of all 46 temporal
misses of the frozen baseline:

- In 33 of the 46 misses, all the labelled evidence was already in HeurChain's context.
  Retrieval found the turns; the answer could not place them in time.
- 26 misses come from relative time ("last Saturday", "two weeks ago") that the answerer
  could not resolve, because in this corpus the date appears only in the session header,
  never in the stored turn text. Mem0's extracted facts carried explicit dates in 74% of
  evidence units; HeurChain's raw turns carried them in 0 of 2,440.

**Practical takeaway for 0.6.1 users:** when you store conversational memories, put the
absolute date in the stored text or in metadata the agent reads back (for example
`"2023-05-25 session: user said they went hiking last Saturday"`). This needs no
broker change.

## 3. Retrieval budget is the binding limit

[Expansion evaluation](expansion-2026-10-07/EVALUATION.md), offline evidence coverage only
(no answer-quality claim):

- The byte cap never cut a labelled evidence turn. The number of results (`limit`) did.
- Raising `limit` from 20 to 25 on `/search` recovers about as much evidence as either
  experimental expansion, at similar median size.
- The lexical bridge helped multi-hop evidence (+9 turns at `limit` 20, 1.3% precision).
  Session-head expansion helped temporal evidence (+11) but at 0.4% precision and more than
  twice the p90 response size; capping it to three sessions kept most of the size down.

**Practical takeaway for 0.6.1 users:** for multi-hop or conversational recall, ask
`/search` for more results (for example `"limit": 25` or `30`) before considering anything
more elaborate.

## 4. Source-date annotations

[Offline verification](../results/source-date-annotation-v1/README.md) of an opt-in feature
that attaches resolved absolute dates beside raw search results. It passed its offline build,
calendar and packing checks. 182 of 2,019 DEV sources received annotations; 95 resolved to a
date. **No answer-quality benefit was measured**, and the feature is not in 0.6.1. See the
[prototype method](design-notes/source-date-annotation-prototype.md).

## 5. Reliability work and its status

The self-hosted line also hardened dense-index repair (a durable repair queue so a failed
embedding or Qdrant write is retried instead of leaving a memory searchable only by BM25),
embedding-identity checks (vectors from a different embedding model are not mixed into
dense search), and revision preconditions on updates. The 0.6.1 broker addresses the
embedding-identity part through `embed.legacy_model_aliases` and model-tagged vectors; the
repair queue and the experimental levers remain research.

## Index

- [Research index and reconciliation](README.md)
- [Dataset attribution and terms](DATASET-TERMS.md)
- [Historical HeurChain vs Mem0 report](HeurChain%20vs%20Mem0%20on%20LoCoMo.md)
- [Expansion ideation, evaluation and adversarial review](expansion-2026-10-07/README.md)
- [Design notes for unshipped levers](design-notes/)
- [Completed results](../results/)
