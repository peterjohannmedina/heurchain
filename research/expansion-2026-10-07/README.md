# Expansion ideation and offline evaluation (7 Oct 2026)

- [`IDEATION.md`](IDEATION.md): code-grounded performance ideation against `8fb9ba6` (the PRD "HeurChain Improvement Exploration" scope). Covers the quality ideas Q1–Q7, the runtime ideas R1–R6, the Phase-zero instrumentation gaps and the first offline checks.
- [`EVALUATION.md`](EVALUATION.md): offline evidence-coverage evaluation of the two implemented ideas on the 4 LoCoMo DEV conversations, with integrity checks, results, matched-byte comparators, caveats and next steps.
- [`ADVERSARIAL-REVIEW.md`](ADVERSARIAL-REVIEW.md): what could limit this plan as HeurChain scales or grows.
- [`eval/`](eval/): machine outputs of `tools/expansion-eval/run_eval.py`.
  - `run1-uncapped/` is the first run; `run2-session-caps/` is the session-cap sweep.
  - Each holds `summary-limit{20,25,30,40}.json` aggregates and a slim `per-question-limit20.jsonl` (IDs, evidence IDs, counts and bytes; no question, answer or turn text).

Implemented features, both opt-in and default off:

| Idea | `/search` option | Code | Doc |
|---|---|---|---|
| Q1b session-head expansion | `session_head: temporal\|always`, `session_head_max_sessions` | `broker/session_head.go` | `docs/session-head-expansion.md` |
| Q2a lexical bridge | `lexical_bridge: on` | `broker/lexical_bridge.go` | `docs/lexical-bridge.md` |
| Shared append-only contract | `expansion_max_bytes` | `broker/expansion.go` | both docs |

No model, judge or embedding calls were reported. The old collector loaded
the full master before DEV filtering; ingestion/scoring used DEV only, but
“no holdout access” is not established. See the integration correction in
EVALUATION.md for byte, ranked-prefix and input-boundary limits. Historical
numeric outputs remain unchanged; the fixed collector accepts a registered
DEV-only bundle.
