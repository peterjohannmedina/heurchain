# Adoption playbook

Each play pairs the **signals** that make it worth doing with the **cloud feature** it uses and a **starter artifact** you can adapt. Rank plays by value to the user divided by setup effort. Only recommend a play after you've confirmed its signals in the code, not just because the scanner listed it.

## Contents
- A. Foundation (do these first; they unlock everything else)
  - A1 Commit Claude config so it reaches the cloud
  - A2 SessionStart hook for dependencies
  - A3 Environment: network level, setup script, variables
  - A4 Test harness + CI bootstrap
- B. Interactive cloud work
  - B1 Parallel backlog fan-out with `claude --cloud`
  - B2 Plan locally, execute in cloud
  - B3 Ephemeral integration stack (docker compose in the VM)
  - B4 Mobile kick-off and triage
  - B5 Multi-repo Projects
- C. PR automation
  - C1 Auto-fix PRs
  - C2 PR-opened review routine
  - C3 Merged-PR port/sync routine
- D. Routines for maintenance
  - D1 Dependency updates
  - D2 Docs / changelog drift
  - D3 Release verification
  - D4 Code-drift between duplicated sources
  - D5 One-off future cleanup
- E. Event-driven ops
  - E1 Alert triage via API trigger
  - E2 Deploy verification via API trigger
- F. Where the cloud is the wrong tool
- G. Starter templates

---

## A. Foundation

### A1 Commit Claude config so it reaches the cloud
**Signals:** `.claude/` or `*.sh` in `.gitignore`; no `CLAUDE.md`; MCP servers added only at user scope.
**Do:** un-ignore `.claude/skills/`, `.claude/agents/`, `.claude/commands/` and `.claude/settings.json`, keep `.claude/settings.local.json` ignored, and add a `CLAUDE.md` with build/test/run commands and conventions. If `*.sh` is ignored for a good reason, write hook scripts in Python or add a narrow `!scripts/claude/*.sh` exception.
```gitignore
.claude/*
!.claude/settings.json
!.claude/skills/
!.claude/agents/
!.claude/commands/
```

### A2 SessionStart hook for dependencies
**Signals:** any manifest (package.json, requirements.txt, pyproject, go.mod...) and no `SessionStart` hook.
**Do:** a repo hook that installs deps only when `CLAUDE_CODE_REMOTE=true` and is idempotent, so a resume is cheap. The `session-start-hook` skill, if available, generates and verifies one. See template G1.

### A3 Environment: network, setup script, variables
**Signals:** hosts outside Trusted, toolchains not pre-installed, large Docker images, secret-like env vars.
**Do:** propose a named environment per repo family (e.g. `homelab-stack`, `python-ml`). Specify:
- Network level: Trusted unless the scanner found untrusted hosts the *tests* need. Use Custom with an explicit list rather than Full.
- Setup script: apt packages, extra toolchains, `docker compose pull`/`build` so images are cached. Keep it under 5 minutes.
- Variables: non-secret config only. For secrets, use API credentials (Pro/Max) or mocks.
See template G2.

### A4 Test harness + CI bootstrap
**Signals:** zero test files and no CI workflow. This is the single biggest unlock: without tests a cloud session can't verify its work, and auto-fix has nothing to react to.
**Do:** a first cloud task: "Add a smoke-test suite (pytest/vitest/go test) that runs offline with services mocked or started in-VM, plus a GitHub Actions workflow running it on PRs." Keep it small: health endpoints, one round-trip per core feature.

## B. Interactive cloud work

### B1 Parallel backlog fan-out
**Signals:** tests exist; open issues or TODO/FIXME clusters; independent modules.
**Do:** one `claude --cloud "<task>"` per independent item, then review the diffs on web or phone and create PRs. Good tasks are well-scoped with a verifiable outcome. Bad tasks need LAN access, real secrets, or design judgment you haven't written down.

### B2 Plan locally, execute in cloud
**Signals:** multi-step refactors or migrations; the user wants their laptop free.
**Do:** `claude --permission-mode plan` locally, commit `docs/plans/<name>.md`, then `claude --cloud "Execute docs/plans/<name>.md; open a PR"`.

### B3 Ephemeral integration stack in the VM
**Signals:** `docker-compose*.yml` whose images come from Docker Hub/GHCR or are built locally; services like Redis/Postgres.
**Do:** first ask whether the services can just run **natively** on the VM (Python/Node/Redis/Postgres are pre-installed). That's usually the fastest path to a test loop. If containers are needed: pull from `mirror.gcr.io/library/...` or GHCR (anonymous Docker Hub pulls can 429), use prebuilt images rather than builds that `RUN pip/npm install` (build containers can't reach the session proxy), pre-pull in the setup script so they're cached, and start with `docker compose up -d` from a SessionStart hook or the prompt. The tests then hit `localhost`. Replace LAN-only dependencies (local LLMs, NAS paths, Proxmox APIs) with stubs or env-var overrides for cloud runs.

### B4 Mobile kick-off and triage
**Signals:** a solo maintainer with many repos, or work that arrives away from the desk.
**Do:** start or steer sessions from the Claude app's Code tab; queue follow-ups with `claude -p "..." --cloud <id>`.

### B5 Multi-repo Projects
**Signals:** coordinated changes across repos (an SDK and its plugin, a server and its client, a fork and upstream).
**Do:** a Project that drives parallel sessions per repo. Note that multi-repo sessions don't load repo hooks, so put setup in the environment's setup script.

## C. PR automation

### C1 Auto-fix PRs
**Signals:** GitHub remote + CI workflows (+ the Claude GitHub App installable).
**Do:** install the app, then `/autofix-pr` on PR branches, or toggle Auto-fix in the session's CI bar. **Warn** if `issue_comment`-triggered workflows, Atlantis, or deploy-on-comment exist, because Claude's replies can trigger them.

### C2 PR-opened review routine
**Signals:** a repo with conventions that generic review misses (protocol contracts, migration rules, security-sensitive paths).
**Do:** a GitHub trigger on `pull_request.opened` with the filter `is_draft = false`. The prompt is the repo's checklist; it leaves inline comments plus a summary. See template G3.

### C3 Merged-PR port/sync routine
**Signals:** parallel implementations that must stay in step (two SDKs, a template and its rendered copy, a fork that tracks upstream).
**Do:** trigger on `pull_request.closed` + `is_merged = true`, port the change, open a matching PR.

## D. Maintenance routines

### D1 Dependency updates
**Signals:** manifests with loose pins or stale lockfiles.
**Do:** weekly (e.g. Mon 07:07): bump within semver ranges, run tests, and open a PR only if they're green. Otherwise write up what broke.

### D2 Docs / changelog drift
**Signals:** README/API docs/CHANGELOG plus an active API surface.
**Do:** weekly: compare PRs merged since the last run against the docs and open a docs PR.

### D3 Release verification
**Signals:** published packages (npm/PyPI), version fields in several files.
**Do:** a GitHub `release.published` trigger: check that the version strings agree across manifests, that a CHANGELOG entry exists, and that the packaged files list is complete. Open a fix PR or comment. Publishing itself stays in Actions, because registry hosts never get API credentials.

### D4 Code drift between duplicated sources
**Signals:** the same file vendored in two places (Docker build context vs. Ansible role template vs. package).
**Do:** short term, a weekly routine or CI check that diffs the pairs and opens a reconciliation PR. Long term, a cloud task that collapses them into one source of truth.

### D5 One-off future cleanup
**Signals:** feature flags, temporary shims, "remove after X" comments.
**Do:** `/schedule in 2 weeks, open a PR removing <flag>`.

## E. Event-driven ops

### E1 Alert triage via API trigger
**Signals:** Prometheus/Alertmanager, Grafana, Sentry, Datadog, or PagerDuty config in the repo.
**Do:** an Alertmanager `webhook_configs` entry (or Grafana contact point) POSTs to the routine's `/fire` with a bearer token. The prompt says: "Investigate the alert in the routine-fire-payload block; correlate with recent commits; open a draft PR or write a diagnosis." This works from a LAN because it's outbound only. The routine can't query LAN metrics, so put enough context in the payload. See template G4.

### E2 Deploy verification via API trigger
**Signals:** a CD pipeline or deploy script.
**Do:** the pipeline fires the routine after a deploy; it checks the changelog and diffs, runs smoke tests against *public* endpoints, and posts go/no-go.

## F. Where the cloud is the wrong tool
State these plainly in the report and suggest the alternative:
- Talking to LAN hosts, homelab APIs (Proxmox, Ceph, NAS), or local LLM servers: use **Remote Control** on a LAN machine, a **self-hosted environment**, or a **Desktop local scheduled task**.
- GPU training/inference, more than 16 GB RAM, more than 30 GB of data: use your own hardware with Remote Control.
- Publishing and deploying with real secrets: use **GitHub Actions** with repo secrets. The cloud session prepares the PR.
- macOS/iOS/Windows-native builds and GUI apps.
- Exposing an unauthenticated internal service to the internet *just* so a cloud session can reach it. Recommend auth first, then a connector or Custom allowlist.

## G. Starter templates

### G1 SessionStart hook (Python so `*.sh` ignores don't drop it)
`.claude/settings.json`:
```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "python3 \"$CLAUDE_PROJECT_DIR\"/.claude/hooks/cloud_setup.py", "timeout": 600 } ] }
    ]
  }
}
```
`.claude/hooks/cloud_setup.py`:
```python
#!/usr/bin/env python3
import os, subprocess, sys
if os.environ.get("CLAUDE_CODE_REMOTE") != "true":
    sys.exit(0)  # local sessions: do nothing
root = os.environ.get("CLAUDE_PROJECT_DIR", ".")
def sh(cmd, cwd=root):
    print("+", cmd, flush=True)
    subprocess.run(cmd, shell=True, cwd=cwd, check=False)
# Idempotent installs: cheap on resume.
sh("pip install -q -r requirements.txt")          # adapt per manifest
sh("npm ci --silent", cwd=os.path.join(root, "app"))
```

### G2 Environment spec (what to type into the claude.ai/code environment dialog)
```text
Name: <repo-family>
Network access: Trusted   (or Custom: <hosts>, + "include default list")
Environment variables (non-secret only):
  APP_ENV=test
  SERVICE_URL=http://localhost:8080
Setup script:
  #!/bin/bash
  set -e
  apt-get update -qq && apt-get install -y -qq <pkgs> &
  (cd /path/to/repo && docker compose -f <compose.yml> pull && docker compose -f <compose.yml> build) &
  wait
```
(The setup script can't assume the repo path is the working directory. Have it `cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"` or install only system-level tools there.)

### G3 PR review routine prompt
```text
You are reviewing PR #<from event> in <owner/repo>. Apply this repo's checklist:
1. <protocol/API contract rule>
2. <files that must stay in sync>
3. <security-sensitive paths and what to look for>
Run the test suite. Leave inline comments only for concrete, verifiable problems,
then one summary comment with: verdict, risks, and anything a human must decide.
Do not push commits.
```

### G4 Alert triage routine + Alertmanager receiver
Routine prompt:
```text
Investigate the alert described in the routine-fire-payload block. Identify the
affected service in this repo, read recent commits touching it, and either open a
draft PR with a fix (tests passing) or write a diagnosis comment on a new issue.
Link back to this session ($CLAUDE_CODE_REMOTE_SESSION_ID).
```
The `/fire` endpoint wants `{"text": "..."}` plus the `anthropic-beta` and `anthropic-version` headers. Alertmanager's native webhook body is a different shape, so put a small relay (a 20-line service, n8n, or Node-RED) on the LAN between them:
```yaml
# alertmanager.yml
receivers:
  - name: claude-triage
    webhook_configs:
      - url: http://<relay-host>:9099/alert   # relay reshapes the body and adds the headers
route:
  routes:
    - matchers: [ severity="critical" ]
      receiver: claude-triage
      repeat_interval: 4h          # stay well under 30 fires/hour/routine
```
Relay call:
```bash
curl -X POST https://api.anthropic.com/v1/claude_code/routines/$TRIG_ID/fire \
  -H "Authorization: Bearer $ROUTINE_TOKEN" \
  -H "anthropic-beta: experimental-cc-routine-2026-04-01" \
  -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
  -d "$(jq -n --arg t "$ALERT_SUMMARY" '{text: $t}')"
```
