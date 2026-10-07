# Cloud session fit: heurchain
_Assessed 2026-10-07 in a Claude Code cloud session, against the cloud docs snapshot of 2026-10-07, using the `cloud-session-fit` skill (`.claude/skills/cloud-session-fit`)._

## Verdict
**Ready with setup for development, testing and repo maintenance. Deploys and homelab integrations stay local.**
The scanner's heuristic said *partial* (no tests, LAN references). A live run in the cloud VM upgraded that: the broker and MCP server both run natively in the VM with no setup script. The remaining work is adding tests, CI and committed Claude config, not fighting the environment.

## What was measured in the VM
| Check | Result |
|---|---|
| Redis 7 (pre-installed) | `service redis-server start` → `PONG` |
| Broker deps (`docker/heurchain/requirements.txt`) | `pip install` in 4 s |
| Broker (`uvicorn main:app`) | `/health` ok (Redis connected, Obsidian vault ok, Ori vault "unavailable", which is expected without `ori.config.yaml`); `/session/start` → `/agent/store persist=true` → `/search` returned BM25-ranked hits |
| MCP server deps (`docker/mcp-app`) | `npm install --omit=dev` in 5 s |
| MCP server (`node server.js`, `COMPRESSOR_PROVIDER=none`) | `/health` healthy, `/api/tools` lists **29** tools |
| Docker daemon | starts, compose available |
| Docker Hub pulls | **429 Too Many Requests** (anonymous pulls from shared egress) |
| `mirror.gcr.io/library/{python,node,redis,nginx}` | pulled fine |
| `docker build` with `RUN pip/npm install` | build containers can't reach the session proxy or trust its CA. Not pursued, so test natively instead |

## Workloads
| Workload | Cloud? | Why / what's needed |
|---|---|---|
| Develop + unit-test broker (FastAPI) and MCP server (Node) | ✅ Yes | Proven above. Python 3, Node 22 and Redis 7 are on the base image |
| Integration test of the full compose stack | ⚠️ With setup | Use mirror.gcr.io or prebuilt GHCR images. Native services are simpler for tests |
| Consolidation worker / compressor (Ollama) | ❌ Mock in cloud | `OLLAMA_URL` defaults to `192.168.1.242` (`docker/heurchain/consolidation_worker.py:30`) and compose points at `host.docker.internal`. No GPU, and `ollama.com` isn't on the Trusted list. Set `COMPRESSOR_PROVIDER=none` or stub `/api/generate` |
| Homelab MCP tools (Proxmox, Ceph, Grafana, Prometheus, SearXNG) | ❌ Local | They call `192.168.1.x` APIs |
| Ansible deploy to CT 203 (`inventory.yml.example`, `192.168.1.203`) | ❌ Local | SSH to the LAN. Author the change in the cloud, apply it locally (or Remote Control) |
| `npm publish` | GitHub Actions | `registry.npmjs.org` never receives cloud API credentials. Use an Actions secret |
| Docs / README / `AGENT_CONFIG.json` upkeep | ✅ Yes | Text only |
| PR review + auto-fix | ⚠️ After CI exists | There's no `.github/workflows/` yet |

## Blockers and fixes
| Issue | Impact in cloud | Fix |
|---|---|---|
| `.gitignore` ignored all of `.claude/` | Skills, hooks and settings never reached cloud sessions | **Fixed in this PR**: shared `.claude/settings.json`, `skills/`, `agents/` and `commands/` are now committed; `settings.local.json` stays ignored |
| `.gitignore` ignores `*.sh` | Shell hook/setup scripts would be silently dropped | Write hooks in Python (template in the skill) or add a narrow `!` exception |
| No tests, no CI | Cloud sessions can't verify changes; auto-fix has nothing to react to | Play 1 below |
| No `CLAUDE.md` | Every session starts cold | Add build/test/run commands and the session protocol |
| `.gitignore` lacks `node_modules/` and `.venv/` | A cloud session that installs deps in-tree makes noisy diffs | Add both before adding a SessionStart hook |
| Broker `:3012` has no auth (`docker/heurchain/main.py`); MCP auth is optional (`docker/mcp-app/server.js:54`) | HeurChain memory can't safely be exposed to cloud sessions or routines | Strategic play 7: auth first, then HTTPS on 443 and a claude.ai connector |
| `docker/mcp-app/server.js` and `roles/docker-stack/templates/server.js.j2` differ by 209 lines | Ansible and Docker/npm ship different MCP servers | Play 4 |
| Versions disagree: `package.json` 1.4.2, `McpServer` "1.4.0" (`server.js:607`), startup log "v1.4.1" (`server.js:1363`); README says 26 tools, server exposes 29 | Users and agents get wrong metadata | Play 5 |
| `@anthropic-ai/sdk` is imported (`server.js:316`) but isn't in `package.json` | `COMPRESSOR_PROVIDER=anthropic` fails silently (caught, returns `null`) | Add the dependency, or drop the provider |
| This checkout is `peterjohannmedina/heurchain` (last push 151 days ago); `synchronic1/heurchain` was pushed today | Fixes may land on the stale copy | Confirm the canonical repo and run the skill there |

## Adoption plays (ranked)
| # | Play | Cloud feature | How to start | Effort | Why here |
|---|---|---|---|---|---|
| 1 | Test harness + CI | `claude --cloud` task | `claude --cloud "Add pytest smoke tests for docker/heurchain/main.py (session start/store/search/recall against a real local Redis) and a node smoke test for docker/mcp-app/server.js (/health, /api/tools); add .github/workflows/ci.yml with a redis service; COMPRESSOR_PROVIDER=none"` | S–M | Unlocks every other play. All the pieces were proven to run in the VM |
| 2 | SessionStart hook + env spec | Repo hook + environment | Hook: start Redis, venv + `pip install -r docker/heurchain/requirements.txt`, `npm ci` in `docker/mcp-app`, guarded by `CLAUDE_CODE_REMOTE` | S | Makes every cloud session test-ready in about 10 s |
| 3 | Auto-fix PRs | Claude GitHub App + Auto-fix | Install the app on the canonical repo; `/autofix-pr` on PR branches | XS | Free once play 1 lands. Solo maintainer, steerable from phone |
| 4 | Source-drift guard | CI check, then a cloud refactor | CI step: `diff` the `docker/heurchain/*` ↔ `roles/heurchain/files/*` pairs; then a cloud task to render `server.js.j2` from `server.js` (one source of truth) | S | A 209-line drift already exists |
| 5 | Release verification | Routine, GitHub `release.published` | Prompt below | S | Three different version strings and a stale tool count today |
| 6 | Alert triage | Routine, API trigger | Alertmanager (from the `monitoring` role) → small LAN relay → `/fire` | M | Monitoring already exists. Outbound calls from the LAN work, but the routine can't query LAN Grafana, so put context in the payload |
| 7 | HeurChain as a claude.ai connector | Connector shared by cloud sessions and routines | Add per-tenant auth (V2 PRD §7), expose over HTTPS :443 (e.g. a tunnel), register as a custom connector | L | Gives cloud agents the same memory as local agents; connector traffic bypasses the allowlist |

## Starter config
**Environment `heurchain-dev`** (claude.ai/code → environment settings)
```text
Network access: Trusted
Environment variables (non-secret):
  COMPRESSOR_PROVIDER=none
  REDIS_HOST=127.0.0.1
  HEURCHAIN_URL=http://127.0.0.1:3012
  OBSIDIAN_VAULT_PATH=/tmp/heurchain/obsidian-vault
  ORI_VAULT_PATH=/tmp/heurchain/ori-vault
Setup script: none needed (all toolchains pre-installed).
  Optional for compose tests: docker pull mirror.gcr.io/library/redis:7.2-alpine (etc.)
```
**SessionStart hook**: use template G1 in `.claude/skills/cloud-session-fit/references/adoption-playbook.md` with these steps: `service redis-server start`; `python3 -m venv .venv && .venv/bin/pip install -q -r docker/heurchain/requirements.txt`; `npm ci --prefix docker/mcp-app` (commit a lockfile first).

**Routine prompt: release verification** (trigger: `release.published`)
```text
A release was just published. Verify version consistency across docker/mcp-app/package.json,
the McpServer version and startup log in docker/mcp-app/server.js, AGENT_CONFIG.json and
CHANGELOG.md; verify the tool count in README.md matches the tools registered in server.js;
verify roles/ copies match docker/ copies. If anything disagrees, open a PR fixing it on a
claude/ branch and summarize. Do not publish to npm.
```

## First three moves
1. Merge this PR so the `cloud-session-fit` skill and the `.gitignore` fix reach every cloud session of this repo.
2. Run play 1 as a cloud task (prompt above) and review the PR from the web or phone.
3. Install the Claude GitHub App on the canonical heurchain repo and turn on Auto-fix for that PR.

## Stays local
- **Ansible deploys, Ollama consolidation, homelab MCP tools**: run from a LAN machine. Use Remote Control to steer from your phone, or a Desktop *local* scheduled task for recurring jobs.
- **npm publish**: GitHub Actions with an `NPM_TOKEN` secret, triggered by the release that play 5 verifies.

## Portfolio: where to look next
Triage of the 40 repos visible to this account (`scripts/portfolio_triage.py`, metadata only). Forks (llama.cpp, mission-control-board, two openclaw) and the archived `medina` are skipped.

| Tier | Repos |
|---|---|
| 1 — active (≤45 days) | synchronic1/heurchain, mixpost-instance, ModelRouter, RecruiterAgent, elastic-inference-fabric; peterjohannmedina/claude-model-router, codex-model-routing; synchronic1/TokenRanger |
| 2 — warm (≤180 days) | Food-Bank-Capacity, IRB, RealTimeAVFusion, HarnessRanger, harness-ranger-demo, Compossure, rehabit, plane-heurchain, catlogic, heurchain-benchmarks, peterjohannmedina/heurchain, hermes-harness-ranger |
| 3 — cold | 15 repos: only if revived |

Next step: run this skill on the tier-1 repos. They haven't been scanned yet, so no fit claims are made for them here. Check GPU and local-LLM coupling first in elastic-inference-fabric and ModelRouter.
