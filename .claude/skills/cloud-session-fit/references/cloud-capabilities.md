# Claude Code cloud sessions: capability sheet

Snapshot taken 2026-10-07 from:
- https://code.claude.com/docs/en/claude-code-on-the-web
- https://code.claude.com/docs/en/cloud-environments
- https://code.claude.com/docs/en/routines

The product changes often. When a recommendation depends on a specific limit, domain or flag, re-check the live page first: use the `read_documentation` tool if the session has it, or WebFetch the URL above.

## Contents
1. What a cloud session is
2. Ways to start and move sessions
3. The VM: tools, resources, time limits
4. Network access
5. Configuration that reaches the cloud (and what doesn't)
6. Setup scripts vs. SessionStart hooks
7. Secrets and credentials
8. GitHub integration and auto-fix
9. Routines (scheduled / API / GitHub-triggered)
10. Hard limitations
11. Alternatives when the cloud can't do it

## 1. What a cloud session is
A Claude Code session running on an Anthropic-managed VM (or your org's self-hosted environment). It keeps running when your laptop closes; you steer it from browser (claude.ai/code), the Claude mobile app's Code tab, the Desktop app (choose **Cloud**), or the terminal. Available on Pro, Max, Team, and Enterprise (premium or Chat + Claude Code seats). No separate compute charge; usage shares your account's rate limits, so parallel sessions use them faster.

## 2. Ways to start and move sessions
- `claude --cloud "task"`: new cloud session for the current repo. It clones the GitHub remote at your current branch, so push local commits first. Each call is an independent session, so you can run several tasks in parallel. `--remote` is a deprecated alias.
- No remote, or the Claude GitHub App isn't installed: the CLI uploads a git bundle instead (<100 MB, untracked files excluded, `.env`/`*.pem`/`id_rsa`/`*.tfvars` uncommitted changes left behind). Force with `CCR_FORCE_BUNDLE=1`. A bundle can push back only if your GitHub connection has push access, and GitLab/Bitbucket remotes can't be pushed back at all.
- `claude -p "msg" --cloud <session-id>`: queue a follow-up into a running session from any machine, CI script included. `--output-format json` gives `{ok, session_id, url}`.
- `claude --teleport [id]`, `/teleport`, `/tp`, or `/tasks` then `t`: pull a cloud session and its branch into your terminal. Needs a clean git state, the same repo (not a fork), a pushed branch, and the same account. It's one-way: the CLI can't push a local session to the cloud (the Desktop app can, from **Open in**).
- Plan locally, execute in cloud: `claude --permission-mode plan`, commit the plan, then `claude --cloud "Execute docs/plan.md"`.
- **Projects**: one conversation where Claude coordinates several parallel cloud sessions across repos. It needs the Claude GitHub App on each repo.
- **Remote Control** (`/remote-control`): steer a *local* session from phone or web. This is different from cloud sessions.
- **Ultrareview**: deep multi-agent code review in a cloud sandbox.
- **Claude Tag**: org-managed @Claude in Slack on the same infrastructure, using org-level environments only.

## 3. The VM
- Ubuntu 24.04, x86_64. Use x86_64 Linux wheels and binaries.
- Approximately 4 vCPU, 16 GB RAM, 30 GB disk. Memory-heavy builds or tests may get killed. There's no GPU.
- Pre-installed: Python 3.x (pip, poetry, uv, black, mypy, pytest, ruff); Node 20/21/22 (22 on PATH; npm, yarn, pnpm, bun, eslint, prettier, chromedriver); Ruby 3.1-3.3; PHP 8.3 + Composer; OpenJDK 21 + Maven/Gradle; Go; Rust; GCC/Clang/cmake/ninja/conan; **Docker + docker compose**; **PostgreSQL 16 and Redis 7.0** (installed, not running: `service postgresql start`, `service redis-server start`); git, gh, jq, yq, ripgrep, tmux. Run `check-tools` inside a session for versions.
- Not pre-installed (install them in a setup script): .NET, Dart/Flutter, Elixir, Swift, Deno, conda, CUDA, anything GUI or macOS/Xcode.
- Time limits: a foreground command times out at 2 min by default and can be raised to 10 min. After the timeout it moves to the background for up to 30 more minutes. Raise the limits with the `BASH_DEFAULT_TIMEOUT_MS` / `BASH_MAX_TIMEOUT_MS` env vars. SessionStart `command` hooks get 600 s unless `timeout` is set (`async: true` hooks aren't timed). A setup script that runs longer than about 5 minutes isn't cached.
- The VM is reclaimed after inactivity (including while waiting on an MCP approval). Reopening restores the conversation but not running processes or subagents.

## 4. Network access
Each environment has a level:
| Level | Allows |
|---|---|
| None | No outbound through the session network |
| **Trusted** (Default env) | Default allowlist: package registries, GitHub/GitLab/Bitbucket, container registries (Docker Hub, ghcr.io, gcr, quay, mcr), cloud provider APIs (`*.googleapis.com`, `*.amazonaws.com`, Azure), Ubuntu apt, HashiCorp, k8s, Anaconda, Datadog/Honeycomb intake, `*.modelcontextprotocol.io` |
| Full | Any domain |
| Custom | Your list (one domain per line, `*.` wildcard), optionally plus the defaults |

These always work regardless of level: GitHub (through its own proxy), MCP connectors added on claude.ai (they go through Anthropic's servers), hosts listed on API credentials, and the Anthropic API.

**Notably NOT on the Trusted list**: `huggingface.co`, `ollama.com`, `openai.com`, `openrouter.ai`, most SaaS APIs, and anything on a private network. Hosts outside the list fail with `403` and `x-deny-reason: host_not_allowed`.

**Agent proxy restrictions** (from the in-session proxy notes): it doesn't carry gRPC/HTTP-2-only APIs, WebSocket upgrades, client mTLS, certificate-pinned clients, **HTTPS on ports other than 443**, or raw-TCP databases. A service you want cloud sessions to reach has to be HTTPS on 443 (or be added as a claude.ai connector).

**Docker in the VM** (observed 2026-10-07): `dockerd` starts and compose works, but anonymous pulls from Docker Hub returned `429 Too Many Requests` from the shared egress. Pulling the same images from `mirror.gcr.io/library/<image>` worked, since `*.gcr.io` is on the Trusted list. Processes *inside* containers (including `RUN pip install` / `npm install` build steps) can't reach the session proxy and don't trust its CA. So prefer running services natively on the VM for tests, or pull prebuilt images (GHCR, built by GitHub Actions). Pre-pull in the setup script so the cache keeps them.

**Never reachable from Anthropic-hosted VMs**: your LAN (`192.168.x`, `10.x`, `172.16-31.x`), Tailscale/CGNAT `100.64/10`, `*.local`, and your workstation. Inside the VM, `host.docker.internal` is the VM itself.

## 5. Configuration that reaches cloud sessions
Only what's **committed** to the repo reaches the session (single-repo sessions):
- Yes: `CLAUDE.md`, `.claude/settings.json` (hooks, permissions), `.claude/rules/`, `.claude/skills/`, `.claude/agents/`, `.claude/commands/`, `.mcp.json`, and skills you enabled on claude.ai.
- No: anything in `~/.claude` (user CLAUDE.md, user skills, user hooks, user MCP servers added with `claude mcp add` at local/user scope), plugins listed in `enabledPlugins`, `.claude/settings.local.json`, MDM-managed settings, interactive SSO (AWS SSO etc.).
- Multi-repo sessions (and Project threads) don't load any repo's `.claude/settings.json` or `.mcp.json`, so use the environment setup script there.
- **Check `.gitignore`**: ignoring `.claude/` or `*.sh` silently keeps skills, hooks and scripts out of every cloud session.
- To change a setting, set an env var on the environment, or commit it to `.claude/settings.json`. `/config` in the browser just opens the settings page.

## 6. Setup scripts vs. SessionStart hooks
| | Setup script | SessionStart hook |
|---|---|---|
| Configured in | Environment dialog on claude.ai/code (or org-shared env) | Repo `.claude/settings.json` |
| Runs | Before Claude launches; skipped when the env cache exists | After launch, every start **and resume** |
| Where | Cloud only | Local and cloud (guard with `[ "$CLAUDE_CODE_REMOTE" = "true" ]`) |
| Best for | Toolchains, apt packages, `docker compose pull/build` (images get cached) | `npm ci`, `pip install -r`, `uv sync`, starting services |

The cache is a filesystem snapshot (packages, images, files), so running processes don't survive it. It rebuilds when the script or allowed hosts change, or after about 7 days. Setup scripts run as root.

## 7. Secrets and credentials
- Environment variables (`.env` format) are **readable by anyone who uses the environment**, so don't put real secrets there.
- **API credentials** (Pro/Max only, not Team/Enterprise yet): the key stays outside the VM, and the agent proxy attaches it to requests for the hosts you list. It never attaches to `api.anthropic.com`, `registry.npmjs.org`, `pypi.org`, `files.pythonhosted.org`, `index.crates.io`, `proxy.golang.org`, `jsr.io`, or to setup-script requests. That means `npm publish` and similar can't use them, so keep publishing in GitHub Actions secrets.
- GitHub: credentials stay server-side. `GH_TOKEN`/`GITHUB_TOKEN` read as `proxy-injected`, and `gh` works through the proxy. The proxy blocks tag pushes and branch deletions, allows only a pinned set of GraphQL operations (so no Projects v2), and only reaches repos attached to the session.
- Prefer test doubles and mocks for third-party services in cloud runs.

## 8. GitHub integration and auto-fix
- Connect with the **Claude GitHub App** (needed for auto-fix, routines' GitHub triggers, and Projects) or with `/web-setup`, which sends your local `gh` token for cloning only.
- **Auto-fix**: Claude watches a PR, fixes CI failures and addresses review comments, and asks you when things are ambiguous. Turn it on from the CI bar in a session, with `/autofix-pr` from the terminal on the PR branch, or by pasting a PR URL with "watch and fix". It doesn't react to merge conflicts (there's no webhook), so ask it to rebase. Review replies post under your GitHub user. Be careful in repos where a PR comment triggers deploys (Atlantis, `issue_comment` workflows).
- Diffs show in the session (`+N -M`) with inline comments; Claude creates PRs from the session.
- Commits carry a `Claude-Session:` trailer. `$CLAUDE_CODE_REMOTE_SESSION_ID` gives the session link for reports.

## 9. Routines
Saved prompt + repos + environment + connectors, run autonomously as cloud sessions (research preview). Create them at claude.ai/code/routines or with `/schedule` from a local CLI (not from inside a cloud session).
- **Schedule**: hourly, daily, weekdays, weekly, a custom cron (minimum 1 hour, via `/schedule update`), or one-off ("in 2 weeks, open a cleanup PR"). Pick a few minutes past the hour (e.g. 9:07) to avoid delays.
- **API**: `POST https://api.anthropic.com/v1/claude_code/routines/<trig_id>/fire` with `Authorization: Bearer <token>`, `anthropic-beta: experimental-cc-routine-2026-04-01`, `anthropic-version: 2023-06-01`, and body `{"text": "..."}`. The text arrives as untrusted `<routine-fire-payload>`, so the prompt must explicitly say to act on it. Monitoring, CD and scripts can call it from anywhere with outbound HTTPS, including your LAN.
- **GitHub**: `pull_request.*` (opened, closed/merged, labeled, synchronize...) and `release.*`. Filters: author, title, body, base/head branch, labels, is_draft, is_merged, with equals/contains/regex (the regex must match the whole value). Needs the Claude GitHub App.
- Runs push to `claude/*` branches by default. Protect branches with rulesets. Connectors are all included by default and can write without asking, so trim them. Runs act as you (your GitHub user, your Slack).
- Limits: 100 scheduled runs per hour per account; Run now + API fires are capped at 30 per hour per routine; GitHub events are capped per routine and per account. Runs use your subscription usage.
- A green run status only means "no infra error". Read the transcript to see what actually happened.

## 10. Hard limitations
- Pushing and PRs require GitHub (GitHub Enterprise Server works on Team/Enterprise).
- An org IP allowlist breaks Anthropic-hosted sessions unless support exempts them.
- Zero Data Retention / HIPAA orgs can't use cloud sessions.
- No interactive browser auth, no GPU, no LAN, no macOS.
- Commands that only work in the terminal (`/plugin`, `/resume`, `/clear`) aren't available. `/model`, `/effort` and similar take arguments.

## 11. When the cloud can't do it
- **Remote Control**: run Claude Code on your own machine (LAN, GPU, homelab access) and steer it from phone or web.
- **Self-hosted environment** (Team/Enterprise): cloud-session UX on runners you operate inside your network, using your own git credentials and egress.
- **Desktop scheduled tasks** (choose Local in Desktop routines): cron-like runs on your machine with local file and network access.
- **GitHub Actions** (`anthropics/claude-code-action`): CI-native runs with repo secrets, good for publish and deploy steps.
- **Split the work**: author and test the change in the cloud, then apply or deploy it from inside the network (a PR merged, then a local or self-hosted runner deploys).
