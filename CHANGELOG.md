# Changelog

All notable changes to the self-hosted edition of HeurChain.

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

### Added
- Research findings from the self-hosted research line: HeurChain vs Mem0 on LoCoMo DEV,
  a temporal-miss diagnostic, expansion evaluations and source-date annotation verification
  (`research/`, `results/`).

## [1.4.2] and earlier

The Node.js edition. See git history up to commit `0e0f6e1`.
