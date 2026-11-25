# HeurChain (self-hosted)

**One memory system for every AI agent you run, on your own infrastructure.**

[![self-host smoke test](https://github.com/peterjohannmedina/heurchain/actions/workflows/selfhost-smoke.yml/badge.svg)](https://github.com/peterjohannmedina/heurchain/actions/workflows/selfhost-smoke.yml)
[![npm](https://img.shields.io/npm/v/heurchain?label=npm%20agent%20config)](https://www.npmjs.com/package/heurchain)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

<!-- latest-update:start (keep README.md and npm/README.md identical; checked by tools/readme-banner.test.mjs) -->
> **Latest update: your agent now checks memory on its own.** Claude Code and Codex CLI can
> search HeurChain *before* they answer, without you asking them to. A small hook reads each
> prompt; when it looks like it depends on earlier work (a past decision, a release plan, a
> runbook), it runs a memory search and hands the agent the matching records together with
> your prompt. Prompts that stand on their own are never sent, and it is easy to turn off.
>
> One command connects the agent and installs the memory skill. Automatic recall
> is enabled by default only on qualified builds:
> `node tools/setup/cli.mjs connect --client claude --tenant <tenant_id> --apply`
> ([setup guide](https://github.com/peterjohannmedina/heurchain/blob/master/docs/agent-setup.md)). It works with the existing 0.6.1-oss.1 broker,
> with no broker upgrade. It is on by default for Claude Code 2.1.329 and Codex CLI 0.170.0 on
> Windows, the builds it was tested with; other versions and platforms can opt in.
<!-- latest-update:end -->

<!-- research:start (keep README.md and npm/README.md identical; checked by tools/readme-banner.test.mjs) -->
### Fewer tokens than carrying the whole history

![Bar charts for three readers on LoCoMo: HeurChain answers 436.3, 469 and 467.5 of 543 questions against 426, 495 and 492 when the whole conversation is sent, using about 4,900 to 5,000 input tokens per question instead of about 117,000.](https://raw.githubusercontent.com/peterjohannmedina/heurchain/master/docs/charts/token-savings.png)

On 543 scored LoCoMo development questions, HeurChain used about 4,900–5,000 input tokens per question versus about 117,000 with the full conversation (about 4% as many). Across three readers, scores ranged from 4.8 percentage points below the full-history baseline to 1.9 points above it.

Method, per-category results and comparisons with Mem0, Zep and others:
[HeurChain on LoCoMo](https://heurchain.com/research/papers/heurchain-on-locomo/) ·
[all HeurChain research papers](https://heurchain.com/research)
<!-- research:end -->

HeurChain is a persistent memory broker for AI agents. Claude Code, Codex, Gemini, local
models and any HTTP or MCP client share one store. They write facts, decisions and session
summaries, and later recall them with hybrid search: keyword (BM25) and meaning (dense
vectors), fused into one ranking.

This repository is the **self-hosted edition**, and it is the complete package: the broker
source, a Docker Compose stack that builds and runs it with everything it needs, a systemd
unit for native installs, and the docs. No accounts, billing or cloud services are involved.

> **Broker version: 0.6.1-oss.1 (frozen).** This edition is cut from the 0.6.1 broker.
> Versions read `<mainline>-oss.<n>`, and `GET /health` reports `"edition": "oss"`. Changes
> here are limited to fixes and documentation; new broker features are not added to this
> line. See [CHANGELOG.md](CHANGELOG.md) for what differs from mainline 0.6.1.

### Where to get what

| You want to | Use |
|---|---|
| Connect an MCP client | [MCP connection guide and registry descriptor](mcp/README.md): choose the hosted service **or** your self-hosted broker. |
| Teach an agent to use a connected HeurChain store | [HeurChain memory skill](skills/heurchain-memory/SKILL.md). This is an optional instruction file, not an MCP server or installer. |
| **Run HeurChain** (the memory server) | **This repository**: clone the repo and run `docker compose -f deploy/compose.yaml up -d --build` after configuring `deploy/.env`. See the [quick start](#quick-start). |
| Install and run everything from npm | The [`heurchain` npm package](https://www.npmjs.com/package/heurchain): the broker source and Compose stack, a `heurchain` CLI (`npx heurchain init`, `up`, `tenant`, `connect`), agent setup, the skill and `AGENT_CONFIG.json`. Needs Docker Compose. |
| The old 1.x Node.js server | Deprecated. Still installable as `heurchain@1`; see [Upgrading from 1.x](#upgrading-from-1x). |

## How it works

![HeurChain architecture](docs/diagrams/architecture.svg)

The broker is one Go binary listening on one port. Agents reach it over **MCP** (Claude Code
and other MCP clients) or plain **HTTP + JSON** (scripts, services, anything with `curl`).
Every request carries two headers, an API key and a tenant ID; each tenant is a separate
memory space for a person, team or project.

Behind the broker sit four services, all on your machine:

| Service | Holds | Needed? |
|---|---|---|
| **Redis** | The memory records themselves (text, tags, agent, session, timestamps), the BM25 keyword index, sessions and the vault | Yes. It is the source of truth. |
| **Embedder** | Turns text into vectors. Ollama with `nomic-embed-text` by default, or any OpenAI-compatible endpoint (OpenAI, TEI, vLLM, LM Studio) | Yes. `/store` returns 503 without it. |
| **Qdrant** | Dense vectors for search by meaning | Strongly recommended. Without it, stores succeed with an "indexing incomplete" warning and search falls back to keywords only. |
| **SQLite** (a file) | Tenants, API keys, audit log and usage counts. The schema is created on first start. | Yes, built into the broker. |

### Store and recall

![Store, recall and lifecycle](docs/diagrams/dataflow.svg)

- **Store.** The broker checks the key and tenant, embeds the text, writes the memory record
  to Redis, then indexes it twice: BM25 terms in Redis and a vector point in Qdrant. The
  write is recorded in the audit log.
- **Recall.** The query is embedded (repeat queries hit a cache), and both indexes return
  candidates. Reciprocal rank fusion (RRF) merges the two rankings, so a memory that matches
  on exact words *and* on meaning rises to the top. Agent, tag and date filters apply, and
  the response includes the scores and a `retrieval` block that says what each stage did.
- **Lifecycle.** Each memory has an activation score in the ACT-R style: every access
  raises it and time lowers it. A background worker moves memories whose activation falls
  below a threshold into the **vault**: kept and exportable, but no longer searched. It
  ships as a dry run (`promote_live: false`) that only logs what it would move.

### A session, step by step

![Agent session protocol](docs/diagrams/session.svg)

Agents that follow this protocol pick up where they, or another agent, left off. The
machine-readable version is in [`npm/AGENT_CONFIG.json`](npm/AGENT_CONFIG.json), and the
broker serves the same guide to agents at `GET /`.

The diagram sources are in [`docs/diagrams/`](docs/diagrams) (Mermaid).

## Quick start

From npm, without cloning, run these commands in order. They require Node 20+,
Docker Engine and Compose 2.20 or later. See the [npm package](https://www.npmjs.com/package/heurchain).

```text
npx heurchain init
npx heurchain up
npx heurchain tenant --name "My team" --email me@example.com
```

To build from a clone, follow the [Docker Compose install](docs/install.md#docker-compose)
and [create a tenant](docs/install.md#create-a-tenant-and-api-key) for your shell.
The first start downloads images and the embedding model. The broker listens on
`127.0.0.1` by default.

For either path, [store a synthetic decision and recall the same record](docs/install.md#try-it).
Wait for readiness, confirm the write receipt, and compare the record ID on a fresh
search before connecting an agent. The [self-host smoke test](.github/workflows/selfhost-smoke.yml)
checks the same flow, including MCP and recall after a full stack restart.

## Connect your agents

**Claude Code or Codex CLI: one command.** [`tools/setup`](docs/agent-setup.md) verifies the
broker and your key, adds the MCP entry, installs the [memory skill](skills/heurchain-memory/SKILL.md)
and, on qualified client builds, turns on automatic recall (a prompt-submit hook that searches
before prompts that depend on earlier work). It also handles repeat runs, upgrades and
uninstall, without touching other MCP servers or hooks:

```sh
HEURCHAIN_API_KEY=... node tools/setup/cli.mjs connect --client claude --tenant <tenant_id> --apply
```

See [docs/agent-setup.md](docs/agent-setup.md) for the qualified versions and how to
review the hook. You can call the store and search tools directly with the hook off.

**MCP (Streamable HTTP)** at `http://<host>:8080/mcp/sse`, with the same two headers. For
Claude Code (Bash, with `KEY` and `TENANT` from [the REST check](docs/install.md#try-it)):

```sh
claude mcp add --transport http heurchain http://127.0.0.1:8080/mcp/sse \
  --header "X-HeurChain-API-Key: $KEY" --header "X-HeurChain-Tenant: $TENANT"
```

Any MCP client that supports Streamable HTTP and custom headers works the same way. The
server exposes 24 tools (`heurchain_store`, `heurchain_search`, `heurchain_session_start`,
`heurchain_manifest` and more). Legacy SSE (`GET /mcp/sse` plus `POST /mcp/message`) is also
available.

**HTTP.** `POST /store`, `POST /search`, `POST /query`, `POST /session/start`,
`GET /agent/{agent_id}`, `GET /memory/manifest` and the rest. `GET /openapi.json` on your
broker describes every endpoint.

**Let the agent set itself up.** Paste this to any agent:

```
Read the setup instructions in the heurchain npm package (README.md and
AGENT_CONFIG.json, https://www.npmjs.com/package/heurchain), then connect to the
HeurChain broker at http://<your-host>:8080 using the API key and tenant ID I give
you. Follow the session protocol in AGENT_CONFIG.json.
```

## Running it for real

[docs/install.md](docs/install.md) covers:

- TLS with a reverse proxy, for agents on other machines
- A native Linux install with systemd instead of Docker
- Backups: the Redis volume, the SQLite file and the vault directory (Qdrant can be rebuilt
  from Redis by re-embedding)
- Upgrading from the 1.x Node edition

Configuration lives in [`deploy/broker.yaml`](deploy/broker.yaml) (strict: an unknown key
stops startup with an error naming it) and secrets in `deploy/.env`. The comments in
`broker.yaml` show how to switch from Ollama to an OpenAI-compatible embedder.

## Repository layout

| Path | Contents |
|---|---|
| [`broker/`](broker) | The Go memory broker, version 0.6.1-oss.1 |
| [`broker/schema.sql`](broker/schema.sql) | SQLite baseline schema, embedded in the broker and applied on first start |
| [`deploy/`](deploy) | Dockerfile, Compose stack, example config, systemd unit |
| [`docs/`](docs) | Installation and operation; diagram sources |
| [`tools/setup/`](tools/setup) | Local agent setup: MCP entry, skill, automatic recall hook ([docs](docs/agent-setup.md)) |
| [`skills/`](skills) | The `heurchain-memory` agent skill |
| [`npm/`](npm) | Source of the `heurchain` npm package: the CLI and agent configuration; `npm pack` bundles the broker, `deploy/`, `tools/setup`, skills and docs from this repository |

## Build and test

```sh
cd broker
go build ./...
go test ./...      # uses an in-memory Redis; no services needed
```

Go 1.23 or newer. The broker uses pure-Go SQLite, so no C toolchain is required.

The agent setup tool has offline tests: `node --test tools/setup/setup.test.mjs` (Node 20+).

## Tips for better recall

- Put absolute dates in stored text ("on 2026-11-01", not "last Friday"). Relative dates lose
  their meaning once the memory is recalled later.
- For questions that need several facts together, ask `/search` for more results (`limit`)
  and let the agent combine them.

## Upgrading from 1.x

Releases up to 1.4.2 were a Node.js service deployed with Ansible, published on npm as the
`heurchain` package. The Go broker replaces them; the old code remains in this repository's
git history (last commit `0e0f6e1`). The data model changed, and 0.6.1 does not read 1.x
data in place: run the new broker alongside the old one and re-store what you want to keep.
See [docs/install.md](docs/install.md#upgrading-from-the-1x-node-edition).

## License

[MIT](LICENSE). Third-party Go modules keep their own licenses; see `broker/go.mod`.
