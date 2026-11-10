# Research and result reconciliation — 7 October 2026

The historical [HeurChain vs Mem0 report](HeurChain%20vs%20Mem0%20on%20LoCoMo.md)
and [HTML report](HEURCHAIN_VS_MEM0_LOCOMO_DEV_2026-10-07.html) are preserved as
dated source artifacts. Their provisional language, frozen-only comparisons,
targets and significance bounds have not been silently rewritten as new results.
Use the completed reports below for the final current counts and paired tables.

| Artifact | Actual coverage | Source/status |
|---|---|---|
| [Completed three-way results](../results/three-way-2026-10-07/RESULTS.md), [details](../results/three-way-2026-10-07/DETAILS.md), [JSON](../results/three-way-2026-10-07/THREE-WAY-COMPARISON.json) | 2019 turns/117 batches, 701 answers per arm, 543 paired binary grades; four DEV conversations | Frozen `216e39e`: 360/543; reviewed `7636913`: 363/543; Mem0 OSS `94c3fe9`: 369/543 |
| [Temporal diagnostic](temporal-diagnostic/README.md) | Frozen HeurChain's 46 temporal misses; frozen versus Mem0 and evidence coverage | Offline deterministic reslice/manual review; not a diagnostic of reviewed candidate's 50 temporal misses |
| [Annotation offline verification](../results/source-date-annotation-v1/README.md) | Source/build/synthetic regressions and saved DEV projection/packing only | `61e81f7`, offline independent PASS, live unqualified, no annotation answer-quality result |

Completed generation and grading used the same local `gpt-6-luna`, low effort,
default tier, mandatory Codex harness, one judge pass and historical reference
rubric. They are separate from earlier Qwen experiments. Same answer prompt,
top 20 native units and 16384 UTF-8-byte whole-unit cap were used; native memory
units and metadata differ between HeurChain and Mem0. Temperature-zero control,
human factual accuracy, judge-bias measurement and a generalizable advantage
are not established. The net reviewed HeurChain gain is three answers (+0.55pp),
concentrated in conversation 41; other three conversations together lose eight.
Six heldout conversations remain sealed; adversarial questions are excluded
from the 543 primary binary grades.

Successful usage estimates exclude unknown failed-call charges and are not
invoices or enforced spending caps. Native CPU embedding has no monetary cost
estimate here. Wall-time differences and model-call estimates do not prove a
retrieval latency improvement. Historical targets and proposed annotation
experiments remain proposals; this publication makes no additional inference.

See [dataset attribution/terms](DATASET-TERMS.md), root provenance and omissions
manifests. Original local files remain unchanged. Machine-local unrelated
receipts and full underlying corpora/answers are not included.
