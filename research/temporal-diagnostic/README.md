# Temporal miss diagnostic: frozen HeurChain vs frozen Mem0 (LoCoMo DEV)

7 October 2026. Offline, deterministic reslice of frozen DEV artifacts. No model calls, no retrieval, no holdout reads, and no writes outside this folder.

## Inputs (read only)

- Verdicts: `heurchain-baseline-scoring/mem0-binary-judge-v3/run-001/questions/*/VERDICT.json` (HeurChain, 543) and `heurchain-baseline-scoring/mem0-oss-v3-comparison-v1/judge-resume-355-v1/FINAL543-VERDICTS.jsonl` (Mem0, 543)
- Answers: `heurchain-baseline-scoring/inputs/BASELINE-DEV-ANSWERS-COMPLETE701.jsonl` and `.../mem0-oss-v3-comparison-v1/inputs/MEM0-DEV-ANSWERS-COMPLETE701.jsonl`
- HeurChain retrieval receipts: `heurchain-luna-baseline/handoff-repository/native-evidence/retrievals/*.json` (hybrid BM25 + Nomic, RRF k=60, top 20)
- DEV source ledger: `heurchain-luna-baseline/handoff-repository/inputs/FULL-DEV-SOURCE.jsonl` (2,019 turns)
- Mem0 answer prompts: `heurchain-mem0-luna-comparison/mem0-answer-run-001/questions/*/REQUEST.json`
- Labels (scoring only): `heurchain-baseline-scoring/inputs/FULL-DEV-LABELS.json`

Integrity: 543/543 verdicts per arm, 701 answers and retrievals, packed evidence equals retrieval order minus exclusions for all 701 questions.

## Results

Both arms used the identical answer prompt ("If the evidence is insufficient, answer UNKNOWN"), top 20 units and a 16,384-byte evidence cap.

| Temporal (n = 122) | HeurChain | Mem0 |
|---|---:|---:|
| Correct | 76 | 94 |
| Answered UNKNOWN | 30 | 18 |
| Answer left as a relative phrase only | 7 | 0 |
| Evidence units stating an explicit date in their text | 0 of 2,440 | 846 of 1,149 (73.6%) |

- Paired temporal outcomes: both right 67, HeurChain only 9, Mem0 only 27, both wrong 19.
- By conversation (Mem0 minus HeurChain): conv-26 +4, conv-30 0, conv-41 +10, conv-50 +4.
- All labeled evidence was in HeurChain's context for 106 of 122 temporal questions. HeurChain answered 73 of those 106 correctly. 33 of its 46 temporal misses had every labeled evidence turn in context.
- Across all 2,019 DEV turns, the dialogue text names a month 4 times and a year 0 times. Dates exist only in the session header.

### HeurChain's 46 temporal misses (manual review of every row)

| Cause | Misses | Mem0 correct |
|---|---:|---:|
| Relative time in evidence; answerer abstained | 16 | 10 |
| Relative time copied into the answer unresolved | 5 | 5 |
| Session date given; relative offset not applied | 5 | 4 |
| Evidence not retrieved; question names a date the turn only implies | 6 | 2 |
| Evidence not retrieved; other | 5 | 4 |
| Needs arithmetic across turns or a duration | 4 | 0 |
| Wrong turn used despite correct evidence in context | 2 | 1 |
| Answer stated explicitly; answerer abstained | 1 | 1 |
| Gold label problem (both arms marked wrong) | 2 | 0 |
| **Total** | **46** | **27** |

Relative-time handling accounts for 26 misses, 19 of which Mem0 answered correctly. The temporal gap is 18 net answers.

Gold label problems: `conv-26-qa-005` (evidence says "last Saturday", gold says "The sunday before 25 May 2023") and `conv-41-qa-048` (gold reads "The sunday before 3` July 2023"; both arms answered 30 July 2023).

### Evidence coverage, all categories (HeurChain context, top 20)

| Category | All evidence in context | Partial | None | HeurChain correct with all / partial / none |
|---|---:|---:|---:|---|
| Single-hop (287) | 232 | 2 | 53 | 210/232 · 1/2 · 14/53 |
| Multi-hop (106) | 30 | 54 | 21 | 20/30 · 25/54 · 4/21 |
| Temporal (122) | 106 | 2 | 13 | 73/106 · 0/2 · 2/13 |
| Open-domain (28) | 12 | 3 | 9 | 4/12 · 2/3 · 2/9 |

One multi-hop, one temporal and four open-domain questions have no valid evidence labels. The byte cap never cut a labeled evidence turn; the top-20 limit is what binds.

## Files

- `diagnose_temporal.js`: the deterministic script (Node, one CPU).
- `TEMPORAL-MISS-DIAGNOSTIC-2026-10-07.json`: aggregates plus every HeurChain temporal miss with question, gold, both answers and labeled evidence.
- `MANUAL-REVIEW-2026-10-07.json`: the manual cause label for each of the 46 misses.
- `review.txt`: the readable dump used for the manual review.

Limits: one reviewer assigned causes; four DEV conversations only; the judge is the same model as the answerer with a generous historical rubric. Confirm any fix on the sealed holdout.

## Publication copy

JSON rows and review.txt omit raw questions, gold answers, system answers and dialogue excerpts. Metrics/classifications/IDs remain; originals and their hashes are retained. The diagnostic script requires explicit external offline input roots and an output root. See ../../PUBLICATION-OMISSIONS.json and ../DATASET-TERMS.md.
