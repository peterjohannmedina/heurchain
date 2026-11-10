# HeurChain

**One memory system for every AI agent you run, on your own infrastructure.**

HeurChain is a persistent memory broker for AI agents. Claude Code, Codex, Gemini, local
models and any HTTP or MCP client share one store: they write facts, decisions and session
summaries, and recall them later with hybrid search (BM25 + dense vectors, fused with RRF).

> **2.0.0 is a documentation and agent-configuration package.** Since 2.0.0, HeurChain is a
> self-hosted Go broker (version **0.6.1**) run with Docker Compose or systemd. This package
> no longer contains a server. It carries the setup guide and the machine-readable files
> agents use to wire themselves to your broker.
>
> The 1.x Node.js server (`server.js`) is deprecated. Its data model differs from the broker;
> see [Upgrading from 1.x](#upgrading-from-1x).

Source, install guide and research: **https://github.com/peterjohannmedina/heurchain**

## What's in this package

| File | For |
|---|---|
| `AGENT_CONFIG.json` | Machine-readable manifest: auth headers, MCP and HTTP connection, session protocol, every HTTP operation and MCP tool of broker 0.6.1 |
| `AGENT_ONBOARDING.md` | The broker's own agent guide (the same text it serves at `GET /`) |
| `README.md` | This file |

```js
const config = require('heurchain/AGENT_CONFIG.json');
console.log(config.authentication.headers);
```

## Run the broker

Requirements: Docker Engine with Compose v2.

```sh
git clone https://github.com/peterjohannmedina/heurchain.git
cd heurchain
cp deploy/.env.example deploy/.env      # set ADMIN_USER and a long random ADMIN_PASS
docker compose -f deploy/compose.yaml up -d --build
docker compose -f deploy/compose.yaml exec ollama ollama pull nomic-embed-text
curl -s http://127.0.0.1:8080/health
```

The stack runs the broker, Redis, Qdrant and Ollama, published on `127.0.0.1` only. The
full guide (native systemd install, TLS, backups) is
[docs/install.md](https://github.com/peterjohannmedina/heurchain/blob/master/docs/install.md).

Create a tenant and API key (the key is shown once):

```sh
curl -s -u "$ADMIN_USER:$ADMIN_PASS" -X POST http://127.0.0.1:8080/admin/tenants \
  -H 'Content-Type: application/json' -d '{"name":"My team","email":"me@example.com"}'
```

## Wire your agent

Every request sends two headers: `X-HeurChain-API-Key: <api_key>` and
`X-HeurChain-Tenant: <tenant_id>`.

**MCP (Streamable HTTP)**, for example Claude Code:

```sh
claude mcp add --transport http heurchain http://127.0.0.1:8080/mcp/sse \
  --header "X-HeurChain-API-Key: $KEY" --header "X-HeurChain-Tenant: $TENANT"
```

**HTTP**: `POST /store`, `POST /search`, `POST /query`, `POST /session/start`, and so on.
`GET /openapi.json` on your broker describes every endpoint.

**Let the agent set itself up.** Paste this to any agent:

```
Read the setup instructions in the heurchain npm package (README.md and
AGENT_CONFIG.json, https://www.npmjs.com/package/heurchain), then connect to the
HeurChain broker at http://<your-host>:8080 using the API key and tenant ID I give
you. Follow the session protocol in AGENT_CONFIG.json: check the manifest and start a
session at startup, store and search during work, end the session with a summary.
```

## Session protocol (short form)

1. `GET /memory/manifest`: see what is already stored before asking the user.
2. `POST /session/start` with your `agent_id`; pass the returned `session_id` as
   `metadata.session_id` on every store.
3. `GET /agent/{agent_id}`: recall your recent memories.
4. During work: `POST /store` for facts and decisions, `POST /search` before asking.
5. At the end: `POST /session/end`, then store a concise summary.

## Upgrading from 1.x

1.x was a Node.js server with its own Redis key layout (`doc:*`, `memory:*`, Obsidian/Ori
vault tiers). Broker 0.6.1 does not read 1.x data in place. Run the new broker alongside
the old one, with separate Redis instances, and re-store what you want to keep through
`POST /store`. Keep the 1.x deployment until you have checked the results. The 1.x versions
stay installable as `heurchain@1`.

## License

MIT
