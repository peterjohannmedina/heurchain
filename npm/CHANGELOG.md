# Changelog (npm package)

## [3.0.0] - 2026-11-20

### Changed
- The package is the complete self-hosted HeurChain again, not just the agent configuration.
  It ships the broker 0.6.1-oss.1 Go source and Docker Compose stack (`broker/`, `deploy/`),
  agent setup (`tools/setup/`), the `heurchain-memory` skill and the install and agent setup
  guides. Versions 2.0.0 to 2.0.4 contained only `AGENT_CONFIG.json` and
  `AGENT_ONBOARDING.md`.

### Added
- A `heurchain` CLI: `init` unpacks the server and writes `deploy/.env` with a random admin
  password, `up` builds and starts the stack and waits until the broker is ready, `tenant`
  creates a tenant and API key, `connect` runs agent setup, and `status` and `down` check or
  stop the stack. Re-running `init` upgrades the code and keeps `deploy/.env` and an edited
  `deploy/broker.yaml`.
- README: token-savings chart and links to HeurChain research.

## [2.0.4] - 2026-11-20

### Changed
- README: "Latest update" banner, mirrored from the repository README. It announces the agent
  setup tool (`tools/setup` in the repository), which adds automatic memory recall for Claude
  Code and Codex CLI. Package files are otherwise unchanged.

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
