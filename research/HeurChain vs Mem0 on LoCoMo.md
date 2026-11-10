HeurChain research · LoCoMo DEV split · 7 October 2026

# HeurChain vs Mem0 on LoCoMo

Mem0 leads by nine answers out of 543. Temporal questions account for all of it, and the overall gap is within noise.

**Provisional.** Local counts have been independently recomputed. Final preservation and cleanup checks are still finishing.

## Results

| Category | Local · same 543 questions | Published · context only |  |  |  |
| --- | --- | --- | --- | --- | --- |
| HeurChain + Luna | Mem0 + Luna | Net | Mem0 | Mem0-Graph |  |
| Single-hop | **78.40%**225 / 287 | 73.87%212 / 287 | HeurChain +13 | 67.13% | 65.71% |
| Multi-hop | 47.17%50 / 106 | **53.77%**57 / 106 | Mem0 +7 | 51.15% | 47.19% |
| Temporal | 62.30%76 / 122 | **77.05%**94 / 122 | Mem0 +18 | 55.51% | 58.13% |
| Open-domain | **32.14%**9 / 28 | 21.43%6 / 28 | HeurChain +3 | 72.93% | 75.71% |
| Overall | 66.30%360 / 543 | **67.96%**369 / 543 | Mem0 +9 | 66.88% | 68.44% |

Local columns: the 543 category 1–4 questions from the four LoCoMo DEV conversations, one shared Luna answerer, Luna grading. Published columns come from the Mem0 paper (Table 1 for categories, Table 2 for overall): full LoCoMo, GPT-4o-mini, a different judge setup.

## What the data shows

- **Temporal decides the result.** Mem0's +18 on temporal is twice the overall gap. If HeurChain matched Mem0 there and changed nothing else, it would lead by 9.
- **The overall gap is a tie.** Given how the categories net out, at least 41 questions split between the systems. Even at that minimum, an exact paired test gives p ≈ 0.21.
- **HeurChain keeps detail. Mem0 resolves and combines it.** HeurChain wins when the answer sits in one turn (single-hop +13). Mem0 wins when a date has to be resolved or facts combined across sessions (temporal, multi-hop).
- **Two published values are not usable targets.** The published Overall doesn't follow from the paper's own category scores, and the open-domain column may not cover the same questions.

## Where the nine answers went

Net correct answers by category. A bar to the right means HeurChain answered more of that category correctly; a bar to the left means Mem0 did.

HeurChain ahead Mem0 ahead

Hover or tab to a row for counts. Values are in the Results table.

## Which gaps are real

Accuracy with 95% intervals. Both systems answered the same questions, so the verdicts below use paired bounds rather than interval overlap. A *split* is a question one system answered correctly and the other did not.

HeurChain + Luna Mem0 + Luna

Dots are accuracy; lines are Wilson 95% intervals.

| Category | Net | HeurChain 95% | Mem0 95% | Verdict |
| --- | --- | --- | --- | --- |
| Temporal | Mem0 +18 | 53.4–70.4% | 68.8–83.6% | Likely realHolds at p \< 0.05 unless more than 84 of the 122 questions split. |
| Single-hop | HeurChain +13 | 73.3–82.8% | 68.5–78.6% | BorderlineReal only if 43 or fewer of the 287 questions split. |
| Multi-hop | Mem0 +7 | 37.9–56.6% | 44.3–63.0% | Not establishedNeeds 12 or fewer splits out of 106. |
| Open-domain | HeurChain +3 | 17.9–50.7% | 10.2–39.5% | Too smallCannot reach significance. Best case p = 0.25. |
| Overall | Mem0 +9 | 62.2–70.1% | 63.9–71.7% | Tiep ≈ 0.21 at the minimum 41 splits, higher in practice. |

These tests treat questions as independent. All 543 come from four conversations, so the true uncertainty is wider. Running the paired test on the per-question outcomes will replace these bounds with exact values.

## Why the published columns are context only

- **The published Overall doesn't follow from the paper's category scores.** Weighting Table 1 by LoCoMo's category sizes (841 / 282 / 321 / 96) gives 62.1% for Mem0 and 61.4% for Mem0-Graph, not 66.88% and 68.44%. The order of the two also flips. Don't use 68.44% as a bar.
- **The category mapping is unpublished.** Mem0 never stated which LoCoMo category number carries which label ([mem0ai/mem0#2609](https://github.com/mem0ai/mem0/issues/2609)), and at least one other team shipped transposed labels before retracting a claim ([widemem](https://widemem.ai/benchmarks)). Local Mem0 lands within 3–7 points of its published single-hop and multi-hop scores but 51.5 points below its published open-domain score.
- **Local Mem0 beats its own paper on temporal by 21.5 points.** Luna may handle date arithmetic better than GPT-4o-mini, Luna may grade dates more leniently, or the local Mem0 may be a newer version. Record the version.
- **The setup differs.** The paper used all ten LoCoMo conversations, GPT-4o-mini for extraction and answers, and an LLM judge averaged over ten runs, with the adversarial category excluded.

## What beating every metric takes

Local Mem0 is the bar that matters. HeurChain needs two gains and two holds.

| Category | HeurChain now | Mem0 (local) | HeurChain needs | Change |
| --- | --- | --- | --- | --- |
| Temporal | 76 / 122 | 94 / 122 | ≥ 95 (77.9%) | +19 |
| Multi-hop | 50 / 106 | 57 / 106 | ≥ 58 (54.7%) | +8 |
| Single-hop | 225 / 287 | 212 / 287 | ≥ 213 | can lose ≤ 12 |
| Open-domain | 9 / 28 | 6 / 28 | ≥ 7 | can lose ≤ 2 |
| Overall | 360 / 543 | 369 / 543 | ≥ 387 (71.3%) | +27 |

Against the published columns, HeurChain already leads on single-hop and temporal. Hitting the local targets also clears published multi-hop (51.15%) and the published Overall (68.44%, which would be 372 / 543). Only published open-domain (75.71%) would remain: 22 of 28, or +13, on the least comparable cell. Hold that target until the category mapping is settled.

## Research directions

Ranked by answers at stake. Each starts with a diagnostic, so the work goes where the misses actually are. HeurChain-specific causes are hypotheses until the diagnostic confirms them.

### Temporal

Mem0 +18 · need +19

Evidence already in hand: in the 3 Oct local diagnostic (HC-FN-020), HeurChain retrieved the decisive timestamped turn at rank 1, but the answer came back as "Yesterday". Mem0 had already stored the same event as May 7. That was an answer-side failure, not a retrieval miss.

#### Diagnose

- Sort HeurChain's 46 temporal misses, starting with the questions only Mem0 got right: evidence not retrieved; retrieved without its session date; date arithmetic wrong; relative phrase left unresolved ("last Saturday"); gold answer or grader wrong.
- Confirm both arms gave Luna the same answer prompt and a similar context size. Mem0's reference harness tells the answerer to convert relative times into dates using memory timestamps ([prompts.py](https://github.com/mem0ai/mem0/blob/2b58775c17eb1c1b7532242b7154af6744102280/evaluation/prompts.py)).
- Swap test: give Luna HeurChain's retrieved context with explicit date-resolution instructions. If most of the 18 close, fix the answer step before the memory.

#### Build

- Resolve relative dates when storing. Keep the raw turn and add the absolute date or range, so "yesterday" in a May 8 session also reads May 7.
- Anchor on the session timestamp, never ingestion time. HeurChain's native `created_at` is ingestion time, and Mem0's quarantined DEV wave 0 resolved some dates against the 2026 run date.
- Show every retrieved turn with its session date, in chronological order.
- Route when, before, after and how-long questions to a per-person event timeline, and do the date arithmetic in code.

### Multi-hop

Mem0 +7 · need +8

Evidence already in hand: the 3 Oct lexical-only retrieval diagnostic found all labeled evidence in the top 10 for 47.11% of DEV questions, with recall@50 at 67.90%. Dense retrieval, graph and consolidation were off in that run.

#### Diagnose

- Re-slice that diagnostic by category from the frozen artifacts. No new retrieval is needed.
- Low all-evidence recall on multi-hop points to retrieval. High recall with wrong answers points to synthesis.

#### Build

- For retrieval: retrieve, extract the linking entities and query again; split the question into sub-questions; or gather every fact about a person across sessions. If the frozen arm is lexical-only, dense or hybrid retrieval is an untested lever.
- For synthesis: have the answerer list the evidence before answering.
- A consolidated fact layer can sit beside the raw turns rather than replace them. Test it against single-hop. Mem0's own graph variant lost ground on multi-hop (51.15% → 47.19%), so more structure isn't automatically better.

### Single-hop: protect it

HeurChain +13 · budget 12

HeurChain's lead most likely comes from keeping raw turns, where Mem0 keeps LLM-extracted facts that can drop detail. Replacing raw turns with consolidated facts would give that lead away. Gate every temporal and multi-hop change on single-hop staying at 213 / 287 or above.

### Open-domain: low priority

HeurChain +3 · 28 questions

Both systems are weak here (21–32%). These LoCoMo questions mostly ask for an inference about a person ("Would X likely…?"). Count how many misses are "not mentioned" non-answers; letting the answerer infer from known facts is the cheap fix. One question moves the score 3.6 points, so don't tune on this slice.

## Evaluation practice

- **Keep DEV and holdout separate.** These 543 questions are the four DEV conversations. Diagnose and iterate here, then confirm once on the six sealed holdout conversations (997 category 1–4 questions).
- **Report per conversation.** Four conversations are too few for a conversation-level bootstrap, so the holdout carries any generalization claim.
- **Run paired tests per category** on the per-question outcomes, rather than comparing intervals.
- **Re-grade split questions with an independent judge.** Luna answers and grades both arms. Temporal gold answers often use relative phrasing such as "the Sunday before 25 May 2023", which LLM judges match inconsistently.
- **Pin each arm:** Mem0 version, answer prompt, and context tokens per question. A larger context can buy accuracy on its own.

## Sources and method

- Mem0 paper, Table 1 and §3: [arxiv.org/html/2504.19413v1](https://arxiv.org/html/2504.19413v1#S3.T1)
- Mem0 reference answer prompt: [mem0 evaluation/prompts.py at 2b58775](https://github.com/mem0ai/mem0/blob/2b58775c17eb1c1b7532242b7154af6744102280/evaluation/prompts.py)
- Category mapping question: [mem0ai/mem0#2609](https://github.com/mem0ai/mem0/issues/2609). Transposed-label retraction: [widemem benchmarks](https://widemem.ai/benchmarks)
- LoCoMo dataset: [snap-research/locomo](https://github.com/snap-research/locomo)
- Internal: 3 Oct lexical baseline, quality-review-20261002/incoming/20261003/locomo-baseline/README.md; experiment log entries HC-FN-016 and HC-FN-020, quality-review-20261002/experiment-notes/EXPERIMENT-LOG.md
- Counts were derived from the reported percentages; they are the only set consistent with all ten values and the 543 total. Category sizes match LoCoMo's official numbering (4 single-hop, 1 multi-hop, 2 temporal, 3 open-domain).
- Intervals are Wilson 95%. Split thresholds use the McNemar normal approximation; the overall and open-domain bounds use the exact binomial form.

Prepared 7 October 2026 from the 543-question DEV comparison. Provisional until preservation and cleanup checks finish.