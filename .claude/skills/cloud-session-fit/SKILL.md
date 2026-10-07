---
name: cloud-session-fit
description: Assess where Claude Code cloud sessions (Claude Code on the web, claude.ai/code, `claude --cloud`, routines, auto-fix PRs) can be used in a repository or across a whole portfolio of repos, and produce a ranked adoption plan with blockers, fixes, and ready-to-commit starter config. Use this whenever someone asks where/how to use Claude Code on the web or cloud sessions in their projects, which repos could run Claude in the cloud, whether a repo is "cloud-ready", what routines or scheduled/background agents to set up, whether to turn on auto-fix for PRs, or how to get value from a Claude Code web/cloud announcement, even if they only paste a docs link and say "where could we use this".
---

# Cloud session fit

Figure out **where cloud sessions earn their keep** in the user's real work, and where they don't, and turn that into a short, ranked plan the user can act on this week.

Cloud sessions are fresh Ubuntu VMs that clone a GitHub repo and can't see the user's machine or LAN. So the core question for every workload in a repo is: *can it run, be verified, and be delivered from a VM with only GitHub plus allowlisted internet?* The answer is usually "yes for dev/test/review/maintenance, no for LAN deploys and hardware-bound work", and the value is in being specific about which is which.

## Inputs
- **Single repo**: the current working directory unless the user names a path or repo.
- **Portfolio**: "our projects", "all my repos", or a GitHub owner or list. Start with triage (step 1b), then deep-assess the top few.

Read `references/cloud-capabilities.md` before judging fit. It's a dated snapshot of limits, the network allowlist, and features. If a recommendation hinges on a specific limit or domain, re-check the live docs first: use the `read_documentation` tool if the session has one, otherwise WebFetch the URLs at the top of that file. The product moves fast, and a wrong limit undermines the whole report.

## Workflow

### 1a. Scan (single repo)
```bash
python3 <skill-dir>/scripts/scan_repo.py <repo-path>          # human-readable
python3 <skill-dir>/scripts/scan_repo.py <repo-path> --json   # for further processing
```
The scanner is fast, read-only, stdlib-only. It reports stacks, tests, CI, containers, LAN/private-IP references, `host.docker.internal`, hosts outside the Trusted allowlist, hardware coupling, secret-like env vars, `.gitignore` rules that hide `.claude/` from the cloud, and **candidate** plays.

### 1b. Triage (portfolio)
Get the repo list from whatever is available: the `list_repos` tool (claude-code-remote), the GitHub MCP tools, or `gh repo list <owner> --limit 200 --json nameWithOwner,pushedAt,isFork,isArchived,visibility,primaryLanguage`. Save it to a file and run:
```bash
python3 <skill-dir>/scripts/portfolio_triage.py repos.json
```
It tiers repos by recent activity and drops forks and archived repos. Deep-assess tier-1 repos (up to about 5 per pass; offer to continue with the rest). Each deep assessment needs a local clone. In a cloud session, attach repos with `add_repo` only for the ones you'll actually scan. Then run `scan_repo.py` on several paths at once for a comparison table.

### 2. Verify the signals by reading code
The scanner pattern-matches, so treat its output as leads. For each warning, open the referenced files and decide:
- Is the LAN IP or `host.docker.internal` in **runtime code the tests exercise**, in **deploy config**, or just in **docs/examples**? Only the first blocks cloud testing, and the usual fix is an env-var override or a stub.
- Is a "hardware" hit a hard requirement (CUDA kernels, a Proxmox API the tests call) or an optional integration that can be mocked?
- Do the "untrusted hosts" get called at test time, or only in production?
- Is there really no test suite (check Makefiles, scripts, CI configs, notebooks)?
- Are files duplicated or templated in ways that drift (compare them; `diff` is cheap)?

Then split the repo into **workloads** and classify each one:

| Workload | Examples | Typical verdict |
|---|---|---|
| Develop and unit-test | edit code, run pytest/vitest | cloud-ready once deps install |
| Integration test | compose stack, DBs | cloud with setup (Docker/Postgres/Redis are on the image) |
| Review and maintain | PR review, deps, docs drift | cloud via auto-fix and routines |
| Build and package | docker build, wheels | cloud unless >16 GB RAM, GPU, or a non-x86 target |
| Publish and deploy | npm publish, ansible to LAN, kubectl | usually local, self-hosted, or GitHub Actions |
| Runtime integrations | local LLM, NAS, hypervisor APIs | local-only; mock them for cloud tests |

### 3. Prove it (only when already inside a cloud session)
If `CLAUDE_CODE_REMOTE=true`, you're standing in the target environment, so cheap empirical checks beat inference. Keep it to roughly 10 minutes and don't modify tracked files:
- `check-tools`, then try the dependency install the hook would run (into a venv or scratch copy, not the tracked tree).
- Run the test command if there is one. If there isn't, start the services **natively** and hit their health endpoints. Python, Node, Redis and Postgres are already on the VM, and this is usually faster and more reliable than Docker.
- For compose stacks, `docker compose config` is free. Pulls and builds have known snags (see "Docker in the VM" in the capability sheet): anonymous Docker Hub pulls can hit `429` from shared egress (`mirror.gcr.io/library/<image>` works), and `RUN pip/npm install` inside a build can't reach the session proxy. Don't wire the proxy or its CA into containers yourself. Record it as a setup item for the user instead (prebuilt images on GHCR, or a native-services test path).
- `curl -sI https://<host>` for any external host the tests need. `403` with `x-deny-reason: host_not_allowed` means it needs Custom network access.
- Stop every process you started and remove the scratch files when you're done.

Report what actually happened (timings, pass/fail) alongside what you inferred. Measured facts carry more weight with the user than heuristics.

### 4. Pick the plays
Use `references/adoption-playbook.md`. It maps signals to plays and has starter templates. Choose at most about 6 plays per repo, ranked by **value to this user ÷ setup effort**, and put the foundation fixes (commit `.claude/` config, SessionStart hook, environment spec, test harness) first when they're missing, because every other play depends on them. Say plainly which workloads should stay local and what to use instead (Remote Control, self-hosted environment, Desktop local tasks, GitHub Actions).

Be concrete about this user's code. "Add a smoke test that runs `docker compose up` and hits `/health` on the broker (`docker/heurchain/main.py`)" is actionable; "consider adding tests" is not.

Flag security trade-offs when they come up: unauthenticated services, secrets in environment variables (readable by every user of the environment), and auto-fix on repos where PR comments trigger deploys.

### 5. Report
Use this structure. Keep it skimmable: tables and short bullets, no preamble.

```markdown
# Cloud session fit: <repo or portfolio>
_Assessed <date> against Claude Code cloud docs snapshot <date>_

## Verdict
<ready | ready with setup | partial | not a fit> — one or two sentences on why.

## Workloads
| Workload | Cloud? | Why / what's needed |

## Blockers and fixes
| Issue (file:line) | Impact in cloud | Fix |

## Adoption plays (ranked)
| # | Play | Cloud feature | Trigger / how to start | Effort | Why here |

## Starter config
- Environment: name, network level (+ allowed hosts), env var names (never values), setup script
- `.claude/settings.json` SessionStart hook (or a pointer to it if you wrote it)
- Routine prompts for the top routine plays

## First three moves
1. ...
2. ...
3. ...

## Stays local
<workloads + recommended alternative>
```

For a portfolio, lead with a table (`repo | tier | verdict | top play | blocker`), then one short section per deep-assessed repo.

Deliver the report in chat. If the user wants it saved or shared, write it where they ask (default `docs/cloud-session-fit/<repo>.md`). If they'll pass it to others, publish it with the Artifact tool if available.

### 6. Offer to implement
End by offering to make the foundation changes (un-ignore `.claude/`, add `CLAUDE.md`, add the SessionStart hook, write the test-harness task) on a branch. Don't edit the repo during the assessment itself, because the user asked for analysis first. The `session-start-hook` skill, if available, can build and validate the hook.

## Pitfalls
- Don't recommend cloud sessions for anything that must reach `192.168.x`/`10.x`/Tailscale hosts. They can't, whatever the network level.
- Don't propose putting real secrets in environment variables; they're visible to anyone using the environment.
- `.claude/` or `*.sh` in `.gitignore` silently keeps skills and hooks out of every cloud session, so always check.
- Multi-repo sessions and Project threads don't load repo hooks. Use the environment setup script for those.
- Routines act as the user (their GitHub identity, their connectors). Suggest trimming connectors and protecting branches.
- A green routine run only means no infra error. Tell users to read the transcript.
