# Ansible Local Dev Log

Running notes on what changed and why, committed alongside each local change.
Edit this file with each commit — one entry per commit, newest at top.

---

## 15db9da — feat(ansible): add system-redis role — manage Redis bind and protected-mode

**Date:** 2026-05-01  
**Context:** The Redis config changes from `147ed2d` (bind IPs, protected-mode) were applied manually on CT 203 and not managed by Ansible. A fresh `ansible-playbook` run would deploy the Docker stack pointing at system Redis but leave Redis misconfigured if the host was ever reprovisioned. This commit closes that gap.

### New role: `roles/system-redis`

**`tasks/main.yml`**
| Task | Module | What it does |
|------|--------|-------------|
| Ensure redis-server is installed | `ansible.builtin.package` | Idempotent install |
| Configure Redis bind addresses | `ansible.builtin.lineinfile` | Replaces `^bind ` line with loopback + `{{ redis_compose_gw }}` + `{{ redis_docker0_gw }}` |
| Disable Redis protected mode | `ansible.builtin.lineinfile` | Sets `protected-mode no` |
| Ensure redis-server running + enabled | `ansible.builtin.service` | Starts and enables on boot |
| Verify Redis on loopback | `ansible.builtin.command` | `redis-cli ping` — fails playbook if not PONG |
| Verify Redis on compose gateway | `ansible.builtin.command` | `redis-cli -h {{ redis_compose_gw }} ping` — confirms containers can reach it |

**`handlers/main.yml`**: `Restart redis-server` — triggers on either `lineinfile` config change.

### `playbook.yml` changes

- `system-redis` role inserted **before** `docker-stack` with `tags: [docker, system-redis]`
  — running `--tags docker` automatically applies Redis config before deploying the stack
- Fixed stale `redis_port` in deployment summary → replaced with human-readable line noting system Redis is shared with HeurChain

### `inventory.yml` changes

Added two explicit gateway vars (with inline comments explaining both are needed):
```yaml
redis_docker0_gw: "172.17.0.1"   # docker0 bridge; what host-gateway resolves to inside containers
redis_compose_gw: "172.19.0.1"   # second-brain-mcp_default network gateway
```

### Idempotency check

`ansible-playbook --tags system-redis --check` → 5 ok, 0 changed on the live host.
The two verify tasks skip in check mode (expected — command tasks don't have a check-mode implementation).

---

## 147ed2d — feat(ansible): wire second-brain MCP to shared system Redis (HeurChain)

**Date:** 2026-05-01  
**Context:** Second-brain MCP and HeurChain broker were both running on CT 203 but using separate Redis instances — the MCP had its own Docker sidecar (17 keys), HeurChain used the system Redis (463 keys, the full document store). `cache_get`/`cache_set` calls through the MCP were blind to everything HeurChain had stored.

### What changed

**Dropped the Docker Redis sidecar entirely.**  
Removed the `redis` service, `redis-data` volume, and `depends_on` redis condition from `docker-compose.yml.j2`. The MCP now connects to the system Redis via `host.docker.internal`.

**Added `extra_hosts: host.docker.internal:host-gateway`** to the `obsidian-mcp` service so the container can resolve the host via Docker's built-in `host-gateway` special value.

**Changed `REDIS_URL`** from `redis://redis:6379` (Docker sidecar) to `redis://host.docker.internal:6379` (system Redis). Host-gateway resolved to `172.17.0.1` (the default `docker0` bridge IP).

**System Redis config changes** (applied directly on CT 203, not via Ansible — document here for reproducibility):

| Setting | Before | After | Reason |
|---------|--------|-------|--------|
| `bind` | `127.0.0.1 -::1` | `127.0.0.1 -::1 172.19.0.1 172.17.0.1` | Allow connections from compose network gateway and docker0 bridge |
| `protected-mode` | `yes` | `no` | Protected mode blocks all non-loopback connections when no password is set |

> **Note:** `172.19.0.1` is the `second-brain-mcp_default` compose network gateway. `172.17.0.1` is the default `docker0` bridge (what `host-gateway` resolves to). Both were needed because `host-gateway` did not resolve to the compose network gateway as expected.

**Replaced Redis health check task** — `docker exec redis-cache redis-cli ping` → `redis-cli ping` directly on the host (no container needed).

### Verification

```
docker exec obsidian-mcp node -e "... r.keys('*') ..."
→ total: 463
```

MCP container now sees all 463 system Redis keys, including all HeurChain `doc:*` entries, `tags:*`, `namespaces:*`, etc. `cache_get` and `obsidian_search_notes` operate on the unified keyspace.

### State after this commit

- `obsidian-mcp` → system Redis (`127.0.0.1:6379` via `host.docker.internal`)
- HeurChain broker → same system Redis (unchanged)
- Docker Redis sidecar: removed. `obsidian-mcp-enhanced_redis-data` volume still on disk (backup).
- Both services also share `/opt/obsidian-vault` on the filesystem.

---

## e98cb85 — fix(ansible): CT 203 deploy fixes — compose binary, AppArmor, Redis port

**Date:** 2026-05-01  
**Context:** First live deploy run against CT 203 (192.168.1.203) after the version-update commit.

### What failed and why

| Failure | Root cause | Fix applied |
|---------|-----------|-------------|
| `docker compose pull` → "unknown command" | Docker 28.2.2 installed on CT 203 but without the compose *plugin*. The standalone binary is at `/usr/local/bin/docker-compose` (v2.24.5) | Changed task commands to `docker-compose` (hyphen) |
| All containers failed to start → AppArmor error | Docker in a Proxmox LXC container can't load the `docker-default` AppArmor profile — it lacks the policy-admin privilege | Added `security_opt: [apparmor:unconfined]` to all three compose services |
| Redis container unhealthy → "Can't handle RDB format version 12" | Old `second-brain-mcp_redis-data` volume was written by Redis 7.4 (format v12). New pinned version 7.2 only supports up to format v11 | Deleted stale volume. Redis 7.2 created a fresh one. Old data migrated separately (see below) |
| `obsidian-mcp` port 3010 already allocated | The prior `obsidian-mcp-enhanced` stack was still running on port 3010 | Ran `docker-compose down` in `/opt/obsidian-mcp-enhanced/` to retire the old deployment |
| Redis `0.0.0.0:6379` already in use | System Redis runs on CT 203 at `127.0.0.1:6379` (used by HeurChain memory broker). Compose tried to bind same host port | Removed `ports:` block from the redis service — it only needs to be reachable within the compose network |

### Other issues resolved during deploy

- **CT 203 had no internet**: Default route was via `192.168.1.233` (pver430 head node, which was offline). Fixed with `ip route replace default via 192.168.1.1`. The `/etc/network/interfaces` already has `gateway 192.168.1.1` and `post-up` rule but Proxmox overrides it on container start. Needs monitoring.
- **Ansible SSH auth**: `ansible_ssh_pass: "4677"` in inventory was wrong. CT 203 root password is `1234` but password auth is disabled — key auth only. Copied `~/.ssh/id_ed25519` from Windows into WSL `~/.ssh/` and switched inventory to `ansible_ssh_private_key_file`.
- **WSL had no SSH key**: Windows key at `C:\Users\NM2\.ssh\id_ed25519` works fine; WSL has no private key by default. `cp /mnt/c/Users/NM2/.ssh/id_ed25519 ~/.ssh/id_ed25519 && chmod 600` fixed it.

### Redis data migration (post-deploy)

The `obsidian-mcp-enhanced_redis-data` volume was NOT deleted by `docker-compose down` (volumes are preserved unless `down -v`). It contained a `dump.rdb` from the previous deployment.

Mounted it in a temporary `redis-inspect` container (redis:7-alpine, which can read format v12), then used a Python loop to copy all 17 keys into the live `redis-cache` container:

- 16 string keys → `SET key value`
- 1 set key (`knowledge:index`) → `SADD key member1 member2`

All keys successfully migrated. Old volume `obsidian-mcp-enhanced_redis-data` retained on disk as a backup.

### State after this commit

- Stack running on CT 203: `redis-cache` (7.2-alpine), `obsidian-mcp` (node:22-slim), `nginx-gateway` (1.27-alpine)
- MCP health: `{"status":"healthy","redis":true,"obsidian":true,"uptime":11s}`
- Vault: `/opt/obsidian-vault` — intact, all data from before deploy
- Redis: 17 keys migrated from previous deployment
- Old stack `obsidian-mcp-enhanced` is down and decommissioned

---

## 4b132b8 — chore(ansible): version update — pin images, FQCN modules, docker compose v2

**Date:** 2026-05-01  
**Context:** Opus audit of the full playbook followed by a batch of edits. Previous state had unpinned images (`latest`, `alpine`), non-FQCN Ansible module names, and was written for docker-compose v1 (removed in Docker 28+).

### Files changed and why

**`ansible.cfg`**  
Added `interpreter_python = auto_silent` (suppresses deprecation warnings about Python discovery) and `forks = 5` (enables parallel execution across hosts).

**`inventory.yml`**  
- Pinned all image versions: `redis:7.2-alpine`, `nginx:1.27-alpine`, `prom/prometheus:v2.53.0`, `grafana/grafana:11.1.0`, `node:22-slim`
- Added `node_version` and `mcp_network_name` variables (previously hardcoded in templates)
- Added comment flagging plaintext credentials — production should use ansible-vault

**`playbook.yml`**  
- `gather_facts: yes` → `gather_facts: true` (YAML boolean canonical form)
- `ping:`, `debug:` → `ansible.builtin.ping:`, `ansible.builtin.debug:` (FQCN)

**`roles/docker-stack/tasks/main.yml`**  
- All modules → FQCN (`ansible.builtin.*`)
- `shell: docker-compose pull/up` → `ansible.builtin.command: docker compose pull/up` (compose v2 syntax, no shell needed)
- `shell: docker restart/exec` → `ansible.builtin.command:` (no shell features needed)

**`roles/docker-stack/templates/docker-compose.yml.j2`**  
- Removed `version: "3.8"` (obsolete, warns in compose v2)
- Added `name: "{{ mcp_network_name }}"` at top (sets the compose project name; monitoring containers join `<name>_default` network)
- `node:20-slim` → `node:{{ node_version }}` (was hardcoded)
- Quoted volume paths (unquoted Jinja2 paths can cause YAML parse issues)
- `npm install --production` → `npm install --omit=dev --silent` (`--production` is deprecated in npm 7+)

**`roles/docker-stack/templates/package.json.j2`**  
Bumped all deps to current releases:
- express `^4.18.2` → `^5.1.0`
- redis `^4.6.12` → `^5.0.0`
- `@modelcontextprotocol/sdk` `^1.12.0` → `^1.17.0`
- zod `^3.23.0` → `^3.25.0`
- Added `"private": true` and `"engines": {"node": ">=22"}`

**`roles/docker-stack/templates/server.js.j2`**  
- `console.log("Redis error:", err)` → `console.error(...)` (errors go to stderr, not stdout)
- Moved `app.listen(port, ...)` inside the Redis `connect()` async IIFE — server no longer accepts connections before Redis is ready
- `process.env.MCP_PORT || port` → `parseInt(process.env.MCP_PORT, 10) || port` (explicit base-10 parse)

**`roles/docker-stack/templates/nginx.conf.j2`**  
- `worker_connections 512` → `1024`
- Added `error_log /dev/stderr;` and `access_log /dev/stdout;` (logs visible via `docker logs`)
- Added `client_max_body_size 10m;`

**`roles/monitoring/tasks/main.yml`**  
- All modules → FQCN
- `shell:` kept for tasks using pipes / `||` / redirects (`ansible.builtin.shell:`); `command:` used elsewhere
- Hardcoded `second-brain-mcp_default` network name → `{{ mcp_network_name }}_default`

**`roles/monitoring/templates/prometheus.yml.j2`**  
- Removed `metrics_path: "/health"` — `/health` returns JSON, not Prometheus text format; leaving it causes scrape parse errors
- Changed targets from `ct_ip:port` to Docker service names (`obsidian-mcp:3010`, `nginx-gateway:80`) — Prometheus runs on the compose network and can resolve service names

**`roles/monitoring/templates/grafana-datasource.yml.j2`**  
- `url: http://{{ ct_ip }}:{{ prometheus_port }}` → `url: http://prometheus:9090` — same reason as above: use the service name on the compose network

**`requirements.yml`** (new file)  
Added `community.docker >= 4.0.0` collection requirement for future use of `community.docker.docker_compose_v2` module.

**`.gitignore`**  
Added `*.retry`, `vars/secrets.yml`, `.vault_pass`.

---

## 893f12d — feat(second-brain): add Obsidian vault as long-term MD storage

**Date:** Pre-session (prior work)  
**Context:** Original commit establishing the second-brain MCP stack. Added Obsidian vault integration as a durable markdown store alongside the Redis ephemeral cache. No detailed notes from this session.
