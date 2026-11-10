# Changelog (npm package)

## [2.0.0] - 2026-11-10

### Changed (breaking)
- HeurChain is now a self-hosted Go memory broker, version 0.6.1, run with Docker Compose
  or systemd from https://github.com/peterjohannmedina/heurchain. This package no longer
  ships a server: `server.js` and its dependencies are gone, and `main` is removed.
- `AGENT_CONFIG.json` is rewritten for the broker 0.6.1 API (schema
  `heurchain-agent-config/3.0`): two-header API-key auth, MCP over Streamable HTTP, the
  session protocol, every HTTP operation and every MCP tool.

### Added
- `AGENT_ONBOARDING.md`: the broker's own agent guide, as served at `GET /`.

## [1.4.2] and earlier

The Node.js MCP server. Deprecated; still installable as `heurchain@1`.
