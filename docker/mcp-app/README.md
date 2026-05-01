# heurchain-mcp

MCP (Model Context Protocol) server for **HeurChain** — tiered agent memory with BM25 search, session lifecycle, and infrastructure knowledge tools.

HeurChain gives any AI agent persistent memory across sessions: a working tier (Ori vault, markdown files) for in-progress scratch space and a longterm tier (Redis + Obsidian vault mirror) for durable knowledge. The MCP server exposes 26 tools over SSE transport so Claude Code and any other MCP-compatible client can read and write that memory natively.

---

## Quick start (Docker standalone)

```bash
# Requires Docker Compose v2
git clone <repo>
cd docker/

cp .env.example .env
# edit .env — set vault paths, optional Grafana/Ollama URLs

docker compose -f docker-compose.standalone.yml up -d --build

curl http://localhost:3012/health   # HeurChain broker
curl http://localhost:3010/health   # MCP server
curl http://localhost/              # nginx gateway (SSE entry point)
```

Services started: `heurchain-redis` → `heurchain-broker` → `heurchain-mcp` → `heurchain-nginx`

---

## Wiring to Claude Code

Add to your `.mcp.json`:

```json
{
  "mcpServers": {
    "heurchain": {
      "type": "sse",
      "url": "http://<your-host>/sse"
    }
  }
}
```

Enable it in `.claude/settings.local.json`:

```json
{
  "enabledMcpjsonServers": ["heurchain"]
}
```

Then restart Claude Code. Confirm with `claude mcp list` — you should see `heurchain: ✓ Connected`.

---

## Agent onboarding

`AGENT_CONFIG.json` (bundled with this package) is a machine-readable self-configuration manifest. An agent reads it once at session start to understand:

- Which interface to use (MCP SSE vs HTTP REST)
- The session startup/shutdown protocol (start → recall → work → persist → end)
- Key naming schema and tier routing rules
- Behavioral guidance (when to search, when to persist, how to avoid key collisions)
- Full quick-reference for all API endpoints

Point any agent at the installed file:

```
node_modules/heurchain-mcp/AGENT_CONFIG.json
```

Or fetch it from the running MCP server's host if you have HTTP access to the broker.

---

## MCP tools

| Category | Tools |
|---|---|
| Cache | `cache_set`, `cache_get`, `cache_delete`, `redis_stats` |
| Vault | `obsidian_write_note`, `obsidian_read_note`, `obsidian_delete_note`, `obsidian_list_notes` |
| Search | `heurchain_search` (BM25 ranked, preferred), `obsidian_search_notes` (fallback) |
| Monitoring | `prometheus_query`, `prometheus_get_targets`, `prometheus_get_alerts` |
| Grafana | `grafana_get_health`, `grafana_list_dashboards` |
| Infrastructure | `proxmox_get_cluster_status`, `proxmox_list_nodes`, `proxmox_find_vm` |
| Ceph | `ceph_get_health_status`, `ceph_list_osd_notes` |
| Network | `network_search_docs`, `network_get_runbook` |
| User context | `user_context_add_entry`, `user_context_get_history`, `user_context_search_history` |
| Health | `health_check` |

---

## Broker API (HTTP clients)

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Service health — Redis, Ori vault, Obsidian vault |
| `POST` | `/store` | Store memory with auto tier routing |
| `GET` | `/get?key=&tier=` | Get memory by key |
| `GET` | `/search?q=&limit=&tier=` | BM25 ranked search |
| `POST` | `/session/start` | Start agent session → returns `session_id` |
| `POST` | `/session/end` | End session with summary |
| `GET` | `/agent/{name}/recall` | Full context of most recent session |
| `POST` | `/agent/store` | Store with automatic agent+session namespacing |

Broker default port: **3012**. All endpoints require no authentication.

---

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `REDIS_HOST` | `localhost` | Redis hostname |
| `REDIS_PORT` | `6379` | Redis port |
| `MCP_PORT` | `3010` | MCP server listen port |
| `HEURCHAIN_URL` | `http://host.docker.internal:3012` | Broker URL from MCP container |
| `OBSIDIAN_VAULT_PATH` | `/opt/obsidian-vault` | Markdown vault root |
| `GRAFANA_USER` / `GRAFANA_PASSWORD` | `admin` / `admin` | Grafana auth for monitoring tools |
| `OLLAMA_URL` | `http://host.docker.internal:11434` | Ollama for consolidation worker |

---

## License

MIT
