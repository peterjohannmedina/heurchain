# Source-date annotation candidate v1

This is an offline, opt-in HeurChain candidate based on reviewed commit
7636913af7c15406ffd03db97ffac34a9d008b36. It has no measured answer-accuracy gain.
No model, deployment, holdout, production, graph-extraction or scoring calls are
authorized by this package.

## Contract

`POST /search` accepts `annotate_event_dates: true` (default false). After the
existing retrieval, ranking and source-limit steps, each result may include a
`date_annotations` object. Text, metadata, source IDs, order and scores retain
their original values. Nothing is persisted. Neither `created_at` nor
`valid_from`/`valid_until` is used as a historical event anchor.

The object records the resolver version, original source ID, sample/turn IDs
when present, SHA-256 hashes of original text and serialized metadata, historical
session timestamp literal, local calendar date, timezone knowledge and anchor
field. Every reference has the original sentence quotation, exact expression,
UTF-8 byte span, resolution status, named rule and a date/range with precision,
or a concrete suppression/unresolved reason. Ranges are inclusive calendar dates,
not UTC instants. The provenance explicitly says these are temporal references,
not assertions that an event occurred.

The raw metadata byte hash is retained; admission additionally verifies a
`json-metadata-fingerprint/v1` semantic hash so reordered keys, whitespace and
Unicode escaping cannot detach an annotation from its actual anchor. The shared
Go/Python encoding is type-tagged, length-prefixed UTF-8, sorted object keys and
losslessly reduced decimal-rational numbers (literal <=128 bytes, exponent
magnitude <=1000). Booleans remain distinct from numbers; numeric negative zero
normalizes to zero. Python floats represent their JSON round-trip spelling;
captures with higher-precision decimals must retain a lossless parser or fail
the fingerprint check. Stale/changed metadata is rejected before admission.
The offline CLI accepts omitted or explicit-null metadata. Both normalize to
semantic null for admission; their separate original byte hashes remain distinct.
The packer represents either as `metadata: null`. Native `/search` retains its
existing `omitempty` behavior for absent metadata; this is not a storage change.

The anchor is `metadata.session_timestamp_literal`, or the structured raw session
header when that field is absent. Conflicting header/metadata dates, malformed
dates and unsupported anchor years fail closed. English historical timestamps,
ISO dates and RFC3339 timestamps are supported. RFC3339 keeps its original local
date, including its offset; unknown timezone still permits a source-local date
but never an absolute instant. Supported anchor years are 1000..9000.

## Resolution rules

* Yesterday/last night, today/tonight, tomorrow and N days ago use calendar-day
  offsets, with leap-year/month/year boundary handling.
* Last/this/next month and year produce the entire named calendar period. N
  months/years ago likewise has month/year precision; no day is invented.
* Last/this/next week records an ambiguous bounded envelope of ISO calendar-week
  and rolling/elapsed-week interpretations, with both alternatives retained.
* Last weekend/this past weekend records Friday-Sunday versus Saturday-Sunday
  alternatives for the most recent completed weekend.
* Last named weekday uses the strict previous weekday rule. A same-weekday
  anchor retains previous-week and same-day alternatives instead of picking one.
* N weeks ago records the historical calendar week and marks its unstated day
  ambiguous. This is not evidence of an exact N*7-days event date.
* Verbatim durations (`for 4 years`, `seven years now`) retain quantity/unit and
  do not infer event start dates. Supported integer quantities are 1..999.
* Quoted, negated, hypothetical/intended clauses and provided BLIP captions are
  conservatively suppressed. Seasonal references and future named weekdays are
  unresolved. Quote/scope detection is lexical, not semantic understanding.
* Compound, alternative, fractional and out-of-range quantities are not converted
  from a supported-looking suffix. Unsupported compound expressions get a
  diagnostic with the original quotation, rather than an invented exact date.

The scan is bounded at 65,536 raw UTF-8 bytes and eight references per turn.
Limit diagnostics are visible. Raw text is never truncated by the annotator.
There is no question/gold-aware extraction, pronoun resolution, entity model,
event-count aggregation, persistence or event-date retrieval index in v1.

## Offline projection and byte budget

`broker/cmd/date-annotate` reads source JSONL on stdin and writes annotations on
stdout. It opens no listener and invokes no model. It shares the exact resolver
with native `/search`; it is not a second Python date implementation.

`tools/event-date-prototype/packing.py` preserves the frozen answer instructions
and native rank order. It reserves whole raw units first under the **16,384 UTF-8
byte cap**, counting metadata, JSON brackets and separators, then admits complete
annotations in rank order using only remaining space. Missing space drops an
annotation, never a previously admitted raw unit. It reports all such omissions.

The historical packer counted each unit plus one separator, but not the final
extra array byte. The candidate counts the actual serialized evidence array.
The offline audit must report whether that one-byte correction changes any saved
DEV raw selection/prompt; any change requires a separate matched control.

Three proposed controls are raw, annotations, and byte-equivalent nonsemantic
padding. Padding uses the same selected units and exact annotation byte occupancy;
its string shape is different and it is not a perfect semantic placebo. An
audit-only annotated-first packer measures potential raw displacement and must
not become the answer collector's default.

No token count is inferred from byte length. A token limit requires an explicitly
supplied, qualified complete-request counter (including harness instructions).
Without that counter token-cap execution fails closed. Current offline byte
tests use a synthetic counter only to test the admission logic, not to claim a
model-token measurement.
Negative, boolean, fractional and nondeterministic token counts are rejected;
the final request is recounted and validated against the cap.

## Evidence and existing ideation history

The completed comparison has frozen/revised/Mem0 scores 360/363/369 of 543.
All 701 frozen/revised saved source lists and prompt hashes are identical.
This rules out a retrieval-input explanation for their measured score changes.
The updated research HTML at `heurchain/research/` (2026-10-07 14:16:46 UTC)
directs retaining the shared answer prompt and putting resolved dates beside raw
memory. Its manual classifications cover the frozen baseline's 46 temporal
misses; the revised candidate has 50 and needs its own classification.

The existing canonical history is
`heurchain/quality-review-20261002/HeurchainChangelog.md`, head
hc-e6507c1dd983d95905036a34cd7f412fa35a229d76c583c89c036e337fd1be1c.
No replacement ledger or formal packet/record run was created. Related existing
mechanisms are stored-granularity E2 (idea-5bcc54868b9e996a6901aa68), bounded
entity bridge (idea-e63031a0a96cc8a49cb45e43), and anchored co-occurrence expansion
(idea-07231ed589fa28463872b27e). They are separate future interventions.

The six CLI protocol stages were applied to this implementation: observed
context identity versus the untested annotation benefit; projection/persistence/
retrieval dimensions kept separate; critique of false precision and overhead;
raw/annotation/padding controls retained; finite paired experiments proposed;
and explicit scope/pins/limitations. This is not formal C-K or MAP-Elites execution.

## Verification and next-round boundary

Offline checks cover source preservation, opt-in defaults, deterministic output,
idempotence, provenance, calendar boundaries/leap years, ambiguity, missing and
conflicting anchors, timezones, quotes, hypothetical/negated clauses, durations,
Unicode bytes, exact byte boundaries and token-cap failure behavior. The complete
broker regression suite uses local synthetic backends; inherited real-backend
tests are not silently enabled. Windows builds use the available Go1.27.0, which
differs from the completed Linux Go1.27.1 comparison. Cross-compilation is not a
Linux runtime qualification.

The next-round proposal must pin this source/build, dataset, unchanged answer
instructions and all admitted projections. It must retain human/source-level
adjudication and repeated unchanged controls to separate answer/judge variation.
Preservation targets are current single-hop226 and open-domain8 with paired
regressions inspected; a single-hop213 floor is insufficient. No new inference
can start before exact manifest/budget approval and cost-enforcement qualification.
