# Installing HeurChain (self-hosted, broker 0.6.1-oss.1)

Two supported paths: Docker Compose (recommended) or a native Linux service.
Both run the same broker binary and need the same three services:

| Service | Role | Required |
|---|---|---|
| Redis 7 | Memories, BM25 index, sessions, cache | Yes |
| SQLite (built in) | Tenants, API keys, audit, schema version | Yes, no separate install |
| Embedder | Ollama (`nomic-embed-text`) or any OpenAI-compatible `/v1/embeddings` endpoint | Yes: `/store` fails without it |
| Qdrant | Dense vector search | Strongly recommended. Without it, stores succeed with an "indexing incomplete" warning and search falls back to BM25 only |

## Docker Compose

Requirements: Docker Engine with Compose v2, about 2 GB RAM, about 2 GB of disk for the images and model.

```sh
git clone https://github.com/peterjohannmedina/heurchain.git
cd heurchain
cp deploy/.env.example deploy/.env
chmod 600 deploy/.env
# Edit deploy/.env: set ADMIN_USER and a long random ADMIN_PASS.
docker compose -f deploy/compose.yaml up -d --build
docker compose -f deploy/compose.yaml exec ollama ollama pull nomic-embed-text
curl -s http://127.0.0.1:8080/health
```

On first start the broker creates its SQLite database from the built-in schema.

### Create a tenant and API key

A tenant is an isolated memory space (a person, a team, or a project). Each tenant has its own API keys.

```sh
curl -s -u "$ADMIN_USER:$ADMIN_PASS" -X POST http://127.0.0.1:8080/admin/tenants \
  -H 'Content-Type: application/json' \
  -d '{"name":"My team","email":"me@example.com"}'
```

The response contains `tenant_id` and `api_key`. The key is shown once; store it in your
secret manager. Every request sends the tenant in `X-HeurChain-Tenant: <tenant_id>` and the
key in `X-HeurChain-API-Key: <api_key>` (or as `Authorization: Bearer <api_key>`).

### Try it

```sh
KEY=...      # api_key from above
TENANT=...   # tenant_id from above
H=(-H "X-HeurChain-API-Key: $KEY" -H "X-HeurChain-Tenant: $TENANT" -H 'Content-Type: application/json')
curl -s http://127.0.0.1:8080/store "${H[@]}" \
  -d '{"agent_id":"me","text":"The staging database moved to port 5433 on 2026-11-01.","tags":["fact"]}'
curl -s http://127.0.0.1:8080/search "${H[@]}" -d '{"text":"staging database port"}'
```

`GET /openapi.json` describes every endpoint.

## Connecting agents over MCP

The broker serves MCP on the same port:

- Streamable HTTP (preferred, stateless): `http://<host>:8080/mcp/sse` with `POST`
- Legacy SSE: `GET /mcp/sse` plus `POST /mcp/message`

Claude Code example:

```sh
claude mcp add --transport http heurchain http://127.0.0.1:8080/mcp/sse \
  --header "X-HeurChain-API-Key: $KEY" --header "X-HeurChain-Tenant: $TENANT"
```

Restarting the broker drops legacy SSE sessions (they live in memory); Streamable HTTP
clients are unaffected.

## Exposing it beyond localhost

Compose publishes the broker on `127.0.0.1:8080` only. To reach it from other machines,
put a TLS reverse proxy (Caddy, nginx, Traefik) in front of it, and keep Redis, Qdrant and
Ollama on the private network. For example, a Caddyfile:

```
memory.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

## Native Linux service

1. Install Redis 7, Qdrant and an embedder (Ollama or an OpenAI-compatible server) on the host.
2. Build: `cd broker && CGO_ENABLED=0 go build -trimpath -o heurchain-broker .` (Go 1.23 or newer).
3. Install the binary to `/usr/local/bin/heurchain-broker`.
4. Copy `deploy/broker.yaml` to `/etc/heurchain/broker.yaml`. Change `redis.addr` to
   `127.0.0.1:6379`, `embed.ollama_url` to `http://127.0.0.1:11434`, and `server.bind` to
   `127.0.0.1`.
5. Create `/etc/heurchain/broker.env` (mode 0600) with `ADMIN_USER`, `ADMIN_PASS` and
   `QDRANT_URL=http://127.0.0.1:6333`.
6. Create a `heurchain` system user, install `deploy/systemd/heurchain-broker.service`, then
   `systemctl enable --now heurchain-broker`.

## Backups

Back up all three stores together, while the broker is stopped or quiesced:

- Redis: the AOF/RDB files in the `redis-data` volume (or `/var/lib/redis`).
- SQLite: `sqlite3 /var/lib/heurchain/broker.db ".backup broker-backup.db"`.
- Vault: the `/var/lib/heurchain/vault` directory.

Qdrant vectors can be rebuilt from Redis by re-embedding, but backing up the `qdrant-data`
volume avoids that cost.

## Upgrading from the 1.x Node edition

The 1.x Node/Ansible edition (last release 1.4.2) stored memories in its own Redis layout.
Broker 0.6.1 uses a different data model and **does not read 1.x data in place**. Run the
new stack alongside the old one, with separate Redis instances, and re-store the memories you
want to keep through `POST /store` (an export script against the 1.x API is the simplest
route). No automated 1.x migration tool ships in this release. Keep the 1.x deployment until
you have checked the results.
