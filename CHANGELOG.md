# Changelog

All notable changes to the self-hosted edition of HeurChain.

## Versioning

Broker versions in this edition read `<mainline>-oss.<n>`: `0.6.1-oss.1` is cut from the
mainline 0.6.1 broker, and `oss.<n>` counts changes made only in the self-hosted OSS edition.
`GET /health` reports the version and `"edition": "oss"`. Git tags use the same string with
a `v` prefix (`v0.6.1-oss.1`); npm package versions are separate (`npm-v*` tags).

## [0.6.1-oss.1] - 2026-11-10

### Removed
- Encrypted silos and engagements: the `/silos` and `/engagements` endpoints, silo binding
  on `/store`, `/query` and `/session/start` (`silo`, `engagement_id` fields), and the
  isolation and destruction certificates. `/store` and `/query` reject those fields with a
  400; `/session/start` ignores them.
- The knowledge-graph index and the ontology policy: `POST /graph/ask`,
  `GET /ontology/capabilities`, the `graph_mode`, `graph_hops` and `graph_weight` search
  options, and the background extraction worker. Search is BM25 + dense vectors fused
  with RRF, as before with default settings.
- The `research/` and `results/` directories.

### Compatibility
- `graph:` and `ontology:` sections in `broker.yaml` are still accepted and ignored, so a
  config written for mainline 0.6.1 loads.
- Schema versions 4-10 are kept as empty placeholders: databases created by this build and
  by `0.6.1` report the same `schema_version`. A database created by `0.6.1` keeps its
  (now unused) silo and graph tables; nothing reads them.

### Added
- `GET /health` reports `"edition": "oss"`.

## [0.6.1] - 2026-11-10

The self-hosted edition moves from the 1.x Node.js service to the Go memory broker, frozen at
version 0.6.1.

### Changed
- Replaced the 1.x Node.js/Ansible deployment with the Go broker (`broker/`), a Docker Compose
  stack (`deploy/`) and a systemd unit.
- Authentication uses per-tenant API keys (`X-HeurChain-API-Key` + `X-HeurChain-Tenant`),
  created through the admin API.
- The broker creates its SQLite schema on first start; no manual schema step.

### Removed
- Features that only applied to a managed multi-customer deployment: subscription billing and
  its webhooks, external account and identity integrations, transactional email, support
  tickets, tracing exporters, OAuth for MCP, plan quotas, and the experimental remote
  decision reranker. Tenants, API keys, rate limits and the admin API remain.

### Fixed (0.6.1 broker)
- An empty `embed.ollama_url` with no `embed.provider` now selects the OpenAI-compatible
  embedder instead of falling back to `localhost:11434`, and `embed.legacy_model_aliases`
  keeps vectors stored under an older model label searchable.

## [1.4.2] and earlier

The Node.js edition. See git history up to commit `0e0f6e1`.
