# Source-date annotation candidate staged

The offline prototype is implemented and built on isolated branch
`candidate/source-date-annotations-v1`, commit
**61e81f7cb077c1e9a9425b986c2263b11f44d57f**, tree
`2596af69d69abcc5edf1b012819823db448d30dc`, based on reviewed source
`7636913af7c15406ffd03db97ffac34a9d008b36`. The frozen baseline remains
`216e39eb8d91b69a75452037efd96bd597b5ffb6`.

Worktree: `worktrees/candidate`. Source/build pins are in
[SOURCE-BUILD-PINS.json](SOURCE-BUILD-PINS.json); specification and fixtures are
in [source-date-annotation-prototype.md](worktrees/candidate/docs/research/source-date-annotation-prototype.md).
The portable [source-date-candidate.bundle](artifacts/source-date-candidate.bundle)
contains the three prototype commits and requires the reviewed7636913 base; local
bundle verification passes. See `evidence/SOURCE-BUNDLE.json` for its SHA256
and exact ref verification.
No remote fetch or push was used.

## Implemented behavior

An opt-in native `/search` projection and a shared compiled offline resolver
attach concrete temporal-reference annotations beside original records: exact
quotation/UTF-8 span, required source ID and metadata-provided sample/turn IDs,
historical anchor, resolved date
or inclusive bounded range, precision, rule, ambiguity/suppression reason and
text/metadata provenance. Event references are separate from broker CreatedAt
and validity intervals. Raw text/metadata and retrieval ordering are preserved.
Durations retain quantity/unit without invented start dates.
When metadata is omitted/null, the raw header can supply the date anchor;
optional sample/turn IDs remain absent. Header/metadata turn identity consistency
is not checked. All2019 DEV records provide their sample/turn identity metadata.

The packer keeps frozen answer instructions and reserves whole raw records
first under the same16384-byte evidence cap. Complete annotations use only
remaining space. The raw/annotation/padding controls have identical raw source
IDs/order; padding matches admitted annotation bytes, with explicitly limited
semantic/token equivalence. Stale metadata, invalid/nondeterministic token
counts and unsupported partial quantities fail closed.

Changed existing files: `broker/recall_options.go`, `broker/rrf.go`,
`broker/recall.go`, `broker/openapi.go`. New implementation files cover native
projection, resolver/fingerprint, CLI and fixtures; offline tools cover packing,
tests, audit and build receipts. Exact changed paths/hashes are pinned in
SOURCE-BUILD-PINS and `evidence/CANDIDATE-SOURCE-FILES.json`.

## Verified offline evidence

* Complete Go suite: **494 test/subtest pass events**, three inherited
  real-backend skips; calendar package38 pass events. Vet and cached-module
  verification pass. Python packing7 and compiled Go-to-Python contract3 tests
  pass. No dependencies were downloaded.
* Clean-commit Windows annotator/broker and Linux cross-build pass. VCS metadata
  embeds61e81f7 with `vcs.modified=false`. Race testing is blocked by absence of
  a local C compiler. Linux runtime and real backend qualification remain open.
* All2019 DEV sources projected identically on two passes.182 sources carry
  annotations:95 resolved,65 ambiguous,19 suppressed,3 unresolved and2 stated
  duration references.
* All701 saved raw prompts match **both** completed HeurChain arms. Protected
  raw-first packing displaces **zero** raw records;673 annotation occurrences
  are admitted and792 omitted for the cap. Total admitted overhead781354 bytes.
  Audit-only annotation-first packing would displace923 raw record occurrences
  across320 questions. This counterfactual is not the collector.
* Original baseline336/reviewed348 source files, source commit/tree identities,
  original staging artifacts and completed answer/verdict bundle hashes remain
  unchanged. No original store/run/port/model/process was modified.

Receipts: [DEV-OFFLINE-AUDIT.json](evidence/DEV-OFFLINE-AUDIT.json),
[ORIGINAL-PRESERVATION.json](evidence/ORIGINAL-PRESERVATION.json),
[EXAMPLES.md](EXAMPLES.md).

## Independent review

The initial24d21d4 review found stale metadata binding, permissive token counts,
partial quantities and quote/scope errors. Original counterexamples/failures are
preserved under `evidence/independent-review/` and `qualification/`.
0459267 repairs each finding and follow-up edge; final61e81f7 also rejects
ordinary/thin-space numeric continuations, with regression tests.
Independent final exact-build review is complete: **OFFLINE_PASS /
LIVE_NOT_QUALIFIED**, with no blocking source findings. All360 source files are
verified, all three final binaries rebuild byte for byte, and26 bounded
counterexample probes pass. The receipt binds the final source/build/test/audit
and proposal hashes and records accepted limitations.

[INDEPENDENT-REVIEW-FINAL.json](evidence/independent-review/INDEPENDENT-REVIEW-FINAL.json),
SHA256 `6d62335cbe6276384c79c442933642a65f984876fe3b7c6d1b2b557e0f4550b1`.
The source remains frozen at61e81f7. Later-phase native, token/cost enforcement,
new generation and scoring work is documented, not added to this prototype.

## Closed next-round proposal and remaining gates

[NEXT-ROUND-PROPOSAL.json](NEXT-ROUND-PROPOSAL.json) materializes32 DEV questions:
16 temporal cases (14 historical candidate misses and2 correct temporal controls)
and16 historically correct single-hop controls,
four single-hop per conversation. Raw/annotation/padding arms repeated twice
produce192 answer attempts, with no automatic retry, native, embedding,
extraction, judge or heldout calls. Selection is purpose-driven and cannot prove
all543 accuracy or preservation of226 single-hop and8 open-domain answers.

The proposed13 estimated-credit ceiling uses a conservative all-input-uncached
arithmetic envelope of12.7488 at **historical** rates, conditional on proposed
24000 complete-input/512 total-completion token caps for every attempt. These
are finite planning limits, not measured tokens, an invoice or an enforced
provider spending limit. Complete-request counting, token/output caps,
billable-attempt/cost metering, worker/cleanup qualification and current frozen
harness/model pins are unresolved. No answer collector is supplied. Human/source
adjudication ownership and scoring approval remain unresolved; answer collection
can be approved independently, but accuracy claims require scoring.

[FULL-COMPARISON-MANIFEST-DRAFT.json](FULL-COMPARISON-MANIFEST-DRAFT.json)
prepares701 DEV question/retrieval/projection pins and marks full-budget unknowns.
The original Qwen27B16-question110call/3200embed/7200second approval cannot cover
a full run or this later Luna-derived protocol. Full701 testing requires its own
reviewed budget and confirmation after measured authorized pilot costs. The
full target387 is95 temporal+58 multi-hop+226 single-hop+8 open-domain; it is a
goal, not a predicted benefit.

Current candidate work made **zero model, judge, embedding or fresh native
deployment calls**, no push or production write, and no heldout reads.
Activation remains disabled. [DEPLOYMENT-AND-ROLLBACK.md](DEPLOYMENT-AND-ROLLBACK.md)
records offline commands, precise live prerequisites and reversible flag/raw
projection rollback. No answer-accuracy benefit has been established.
