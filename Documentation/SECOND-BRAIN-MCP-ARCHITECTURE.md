# Second-Brain MCP Architecture

Deployed 2026-02-16 on CT 203 (mcp-test201) at 192.168.1.203, hosted on pver430 (MediNAS).

---

## Overview

The Second-Brain MCP stack provides a centralized API gateway for AI-assisted infrastructure management. It exposes tool endpoints for semantic caching, note management, and vault operations via a Redis-backed Express.js server behind an Nginx reverse proxy, with Prometheus/Grafana monitoring.

**Design goal:** Reduce token costs on repeated queries by 70-95% via Redis semantic caching, while providing a single MCP endpoint for Claude Desktop, VS Code, and other AI clients.

---

## Service Topology

```
  Clients (Claude Desktop, VS Code, Cursor)
            |
            v
  +-----------------------+
  | nginx-gateway (:80)   |  <-- LXC-safe: master_process off
  | Reverse proxy         |      Docker DNS resolver 127.0.0.11
  +-----------+-----------+
              |
              v
  +-----------------------+
  | obsidian-mcp (:3010)  |  <-- Express.js + Redis client
  | Node.js 20-slim       |      /health, /api/tools, /api/cache
  +-----------+-----------+
              |
              v
  +-----------------------+
  | redis-cache (:6379)   |  <-- Appendonly, 256MB LRU
  | Redis 7-alpine        |      Semantic cache + session store
  +-----------------------+

  Sidecar monitoring (same Docker network):
  +-----------------------+     +-----------------------+
  | prometheus (:9090)    | --> | grafana (:3001)       |
  | Scrapes MCP + Nginx   |     | Dashboards + alerts   |
  +-----------------------+     +-----------------------+
```

---

## Container Details

| Container | Image | Host Port | Health Check | Volumes | Restart |
|-----------|-------|-----------|-------------|---------|---------|
| redis-cache | redis:7-alpine | 6379 | `redis-cli ping` | redis-data (named) | unless-stopped |
| obsidian-mcp | node:20-slim | 3010 | Node.js HTTP GET /health | /opt/second-brain-mcp/app:/app | unless-stopped |
| nginx-gateway | nginx:alpine | 80 | - | nginx.conf (bind mount) | unless-stopped |
| prometheus | prom/prometheus:latest | 9090 | `/-/healthy` | prometheus.yml (bind mount) | unless-stopped |
| grafana | grafana/grafana:latest | 3001 | `/api/health` | provisioning dir (bind mount) | unless-stopped |

**Note:** Port 3000 is occupied by pre-existing `cc-edit` container. Grafana maps to host port 3001.

---

## API Endpoints

All endpoints available directly at `:3010` or via Nginx gateway at `:80`.

| Method | Path | Description | Response |
|--------|------|-------------|----------|
| GET | `/health` | Service health + Redis status | `{"status":"healthy","redis":true,"uptime":N}` |
| GET | `/api/tools` | List available MCP tools | `{"tools":["search_vault","read_note",...]}` |
| POST | `/api/cache` | Store key/value with optional TTL | `{"cached":true}` |
| GET | `/api/cache/:key` | Retrieve cached value | `{"hit":true,"data":{...}}` or `{"hit":false}` |
| GET | `/api/stats` | Redis server statistics | `{"redis_info":"..."}` |

**Cache POST body:**
```json
{"key": "query:hash", "value": {"result": "..."}, "ttl": 3600}
```

---

## Caching Architecture

```
  AI Client Query
       |
       v
  Check Redis cache (key = query hash)
       |
  +----+----+
  |         |
  HIT       MISS
  |         |
  Return    Process query
  cached    Store result in Redis
  result    Return fresh result
```

**Redis configuration:**
- Persistence: appendonly (AOF) for durability
- Max memory: 256MB with allkeys-LRU eviction
- TTL support: per-key expiration via `setEx`

**Expected savings:** 70-95% token reduction on repeated/similar queries across all connected clients.

---

## Monitoring

### Prometheus Scrape Targets

| Job | Target | Path | Interval |
|-----|--------|------|----------|
| mcp-server | 192.168.1.203:3010 | /health | 15s |
| nginx | 192.168.1.203:80 | / | 15s |
| prometheus | localhost:9090 | /metrics | 15s |

### Grafana Access

- URL: http://192.168.1.203:3001
- Login: admin / SecurePass123!
- Datasource: Prometheus at http://192.168.1.203:9090 (auto-provisioned)

---

## File Layout on CT 203

```
/opt/second-brain-mcp/
  docker-compose.yml      # Redis + MCP + Nginx orchestration
  nginx.conf              # Reverse proxy config
  app/
    server.js             # Express MCP server
    package.json          # Node.js dependencies
    node_modules/         # Installed by container on startup

/opt/monitoring/
  prometheus/
    prometheus.yml        # Scrape configuration
  grafana/
    provisioning/
      datasources/
        prometheus.yml    # Auto-configured datasource
```

---

## Ansible Deployment

### Prerequisites

- **Control node:** WSL (Fedora 40) with Ansible 2.16+
- **Packages:** `openssh-clients`, `sshpass` (for password-based SSH to CT 203)
- **Target:** CT 203 at 192.168.1.203 with Docker + docker-compose installed

### Playbook Location

Canonical source: `C:\Users\NM2\Documents\DevProjects\SysDev\SysAdmin\ansible\`
WSL working copy: `/home/user/second-brain-ansible/`

### Execution

```bash
# From WSL
cd /home/user/second-brain-ansible
ansible-playbook playbook.yml              # Full deploy
ansible-playbook playbook.yml --tags docker     # Docker stack only
ansible-playbook playbook.yml --tags monitoring # Monitoring only
ansible-playbook playbook.yml --check           # Dry run
```

### Role Structure

```
roles/
  docker-stack/
    tasks/main.yml          # Deploy app files, pull images, start stack
    templates/
      docker-compose.yml.j2 # Redis + MCP + Nginx
      nginx.conf.j2         # LXC-safe reverse proxy
      server.js.j2          # Express MCP server
      package.json.j2       # Node.js dependencies
  monitoring/
    tasks/main.yml          # Prometheus + Grafana containers
    templates/
      prometheus.yml.j2     # Scrape configuration
      grafana-datasource.yml.j2
```

---

## Networking Notes

### LXC Constraints

CT 203 runs inside an LXC container on Proxmox. This imposes restrictions:

- **Nginx:** `master_process off` + `worker_processes 1` required. The `socketpair()` syscall fails in LXC for worker spawning.
- **Docker DNS:** Nginx uses `resolver 127.0.0.11` (Docker's embedded DNS) to resolve container names like `obsidian-mcp`.
- **docker-compose:** Only the standalone binary (`/usr/local/bin/docker-compose` v2.24.5) is available, not the `docker compose` CLI plugin.

### Port Map

| Port | Service | Protocol |
|------|---------|----------|
| 80 | Nginx gateway | HTTP |
| 3000 | cc-edit (pre-existing) | HTTP |
| 3001 | Grafana | HTTP |
| 3010 | MCP server | HTTP/JSON |
| 6379 | Redis | Redis protocol |
| 9090 | Prometheus | HTTP |

---

## Maintenance & Troubleshooting

### Quick Health Check

```bash
ssh root@192.168.1.203 'curl -s http://localhost:3010/health'
ssh root@192.168.1.203 'docker exec redis-cache redis-cli ping'
ssh root@192.168.1.203 'curl -s http://localhost:9090/-/healthy'
ssh root@192.168.1.203 'curl -s http://localhost:3001/api/health'
```

### View Logs

```bash
ssh root@192.168.1.203 'docker logs obsidian-mcp --tail 20'
ssh root@192.168.1.203 'docker logs redis-cache --tail 20'
ssh root@192.168.1.203 'docker logs nginx-gateway --tail 20'
ssh root@192.168.1.203 'docker logs prometheus --tail 20'
ssh root@192.168.1.203 'docker logs grafana --tail 20'
```

### Restart Stack

```bash
# Full restart
ssh root@192.168.1.203 'cd /opt/second-brain-mcp && docker-compose restart'

# Individual service
ssh root@192.168.1.203 'docker restart obsidian-mcp'

# Full teardown and rebuild
ssh root@192.168.1.203 'cd /opt/second-brain-mcp && docker-compose down && docker-compose up -d'
```

### Redis Cache Operations

```bash
# Check cache size
ssh root@192.168.1.203 'docker exec redis-cache redis-cli dbsize'

# Flush cache
ssh root@192.168.1.203 'docker exec redis-cache redis-cli flushall'

# Memory usage
ssh root@192.168.1.203 'docker exec redis-cache redis-cli info memory | grep used_memory_human'
```
