# Changelog

All notable changes to the self-hosted edition of HeurChain.

## Versioning

Broker versions in this edition read `<mainline>-oss.<n>`: `0.6.1-oss.1` is cut from the
mainline 0.6.1 broker, and `oss.<n>` counts changes made only in the self-hosted OSS edition.
`GET /health` reports the version and `"edition": "oss"`. Git tags use the same string with
a `v` prefix (`v0.6.1-oss.1`); npm package versions are separate (`npm-v*` tags).

## [0.6.1-oss.1] - 2026-11-10

### Changed
- This release omits features that are incompatible with the self-hosted version. Core
  memory, hybrid search, sessions, recall, MCP, the vault and consolidation are unchanged.
- Configuration files written for the mainline 0.6.1 broker still load: settings for the
  omitted features are accepted and ignored. Schema versions match mainline 0.6.1.

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

### Omitted
- Features that are incompatible with the self-hosted version. Tenants, API keys, rate
  limits and the admin API are included.

### Fixed (0.6.1 broker)
- An empty `embed.ollama_url` with no `embed.provider` now selects the OpenAI-compatible
  embedder instead of falling back to `localhost:11434`, and `embed.legacy_model_aliases`
  keeps vectors stored under an older model label searchable.

## [1.4.2] and earlier

The Node.js edition. See git history up to commit `0e0f6e1`.
