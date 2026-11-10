# HeurChain (self-hosted)

**One memory system for every AI agent you run, on your own infrastructure.**

HeurChain is a persistent memory broker for AI agents. Claude Code, Codex, Gemini, local
models and any HTTP or MCP client share one store: they write facts, decisions and session
summaries, and recall them later with hybrid search (BM25 + dense vectors, fused with RRF).

This repository is the **self-hosted edition**: one Go binary plus Redis, an embedding model
(Ollama or any OpenAI-compatible endpoint) and Qdrant for dense search. Individuals and organizations can run it on a laptop, a
home server or a private VM, with no external accounts, billing or cloud services.

> **Broker version: 0.6.1 (frozen).** This edition tracks the 0.6.1 broker. Changes here
> are limited to fixes and documentation; new broker features are not added to this line.

## What you get

- **REST API and MCP server** on one port: store, search, query, sessions, promotion,
  vault export, agent recall. MCP over Streamable HTTP or legacy SSE.
- **Hybrid retrieval**: BM25 inverted index in Redis, Qdrant dense search, RRF fusion,
  optional knowledge-graph recall.
- **Tenants and API keys**: isolated memory spaces per person, team or project, with per-key
  rate limiting and an admin API to create and revoke keys.
- **Decay and vault**: ACT-R style consolidation moves stale memories out of the active set
  into a vault you can export (dry run by default).
- **Session continuity**: start, end and resume sessions with summaries across agents.

## Quick start (Docker Compose)

```sh
cp deploy/.env.example deploy/.env      # set ADMIN_USER and ADMIN_PASS
docker compose -f deploy/compose.yaml up -d --build
docker compose -f deploy/compose.yaml exec ollama ollama pull nomic-embed-text
curl -s http://127.0.0.1:8080/health
```

Then create a tenant and API key, and connect your agents: see
**[docs/install.md](docs/install.md)**. That guide also covers native systemd installs,
TLS, backups and upgrading from the 1.x Node edition.

## Repository layout

| Path | Contents |
|---|---|
| [`broker/`](broker) | The Go memory broker, version 0.6.1 |
| [`broker/schema.sql`](broker/schema.sql) | SQLite baseline schema, embedded in the broker and applied on first start |
| [`deploy/`](deploy) | Dockerfile, Compose stack, example config, systemd unit |
| [`docs/`](docs) | Installation and operation |
| [`research/`](research) | Retrieval research and findings ([summary](research/FINDINGS.md)) |
| [`results/`](results) | Completed evaluation results and offline verification receipts |

## Build and test

```sh
cd broker
go build ./...
go test ./...      # uses an in-memory Redis; no services needed
```

Go 1.23 or newer. The broker uses pure-Go SQLite, so no C toolchain is required.

## Research findings

The self-hosted research line compared HeurChain with Mem0 on LoCoMo and studied why
retrieval misses happen. The headline results and the practical advice for 0.6.1 users
(put absolute dates in stored text; ask `/search` for more results on multi-hop recall) are
in [research/FINDINGS.md](research/FINDINGS.md).

## Upgrading from 1.x

Releases up to 1.4.2 were a Node.js service deployed with Ansible. They are replaced by this
Go broker; the old code remains in this repository's git history (tag-free; last commit
`0e0f6e1`). The data model changed, so see the upgrade notes in
[docs/install.md](docs/install.md#upgrading-from-the-1x-node-edition).

## License

[MIT](LICENSE). Third-party Go modules keep their own licenses; see `broker/go.mod`.
