# Changelog (npm package)

## [2.0.2] - 2026-11-10

### Changed
- Changelog wording only; package contents are otherwise the same as 2.0.1.

## [2.0.1] - 2026-11-10

### Changed
- README: states up front that this package is the agent kit and the server comes from the
  GitHub repository; adds architecture, store/recall and session-protocol diagrams.
- `AGENT_CONFIG.json` and `AGENT_ONBOARDING.md` describe broker `0.6.1-oss.1`, which omits
  features that are incompatible with the self-hosted version.

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
