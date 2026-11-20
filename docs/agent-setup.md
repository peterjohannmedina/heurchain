# Connecting agents: MCP, skill and automatic recall (self-hosted)

`tools/setup/cli.mjs` sets up an agent on this machine against your own broker, in
one pass:

1. **MCP entry** for the client, so the model can call `heurchain_search`,
   `heurchain_store` and the other broker tools.
2. **The `heurchain-memory` skill** ([skills/heurchain-memory/SKILL.md](../skills/heurchain-memory/SKILL.md)),
   standing guidance on when to search and what to save.
3. **Automatic recall**, a `UserPromptSubmit` hook that searches the broker before
   prompts that look like they depend on earlier work. It adds up to three labelled
   excerpts to that prompt's first model request.

It needs Node 20 or later and nothing else (no `npm install`). It talks only to the
broker URL you give it. There is no hosted account, browser sign-in or OAuth.

## Supported clients

| Client | MCP entry | Skill | Automatic recall |
|---|---|---|---|
| Claude Code CLI 2.1.329, Windows | `claude mcp add-json --scope user heurchain-local` (HTTP, key from a headers helper) | `~/.claude/skills/heurchain-memory/` | **On by default**: qualified on this broker build |
| Codex CLI 0.170.0, Windows | `codex mcp add heurchain_local` (local stdio bridge) | `~/.agents/skills/heurchain-memory/` | **On by default** once you trust it in `/hooks`: qualified on this broker build |
| Claude Code CLI / Codex CLI, any other version or on macOS, Linux, WSL | Same | Same | **Off by default, unverified.** Opt in with `--recall-hook` |
| Codex app, Claude Desktop, Copilot in VS Code, Gemini CLI / Antigravity, Muse and other MCP clients | Not handled by setup. Configure Streamable HTTP manually ([below](#other-mcp-clients)) | Copy the skill file manually | **Unsupported**: no hook contract was qualified against this broker |

"Qualified" means the exact client build passed `tools/setup/native-check.mjs`
against a local 0.6.1-oss.1 broker ([results](#qualification)). Hook delivery has
changed between client releases before, so other builds stay opt-in until they pass
the same check. The list lives in `QUALIFIED` in `tools/setup/clients.mjs`.

## One local flow

Start the broker and create a tenant and key ([README quick start](../README.md#quick-start)).
Then, from the repository root:

```sh
export HEURCHAIN_API_KEY=...        # the api_key from POST /admin/tenants (PowerShell: $env:HEURCHAIN_API_KEY = '...')
node tools/setup/cli.mjs connect --client claude --tenant <tenant_id>            # dry run: shows the plan
node tools/setup/cli.mjs connect --client claude --tenant <tenant_id> --apply
```

Use `--client codex` for Codex CLI, and `--url https://memory.example.internal` when
the broker is not at `http://127.0.0.1:8080`. Plain `http` is accepted only for
loopback; use the TLS proxy from [install.md](install.md#exposing-it-beyond-localhost)
otherwise. `--key-stdin` reads the key from one line on stdin instead of the
environment.

Before it changes anything, `connect` verifies the broker: `GET /health`, then MCP
`initialize` and `tools/list` with your key and tenant, requiring `heurchain_search`
and `heurchain_store`. A wrong key or tenant stops it with nothing changed. These
checks do not write anything.

Then restart the client and review what was added:

- **Claude Code:** `/mcp` lists `heurchain-local`; `/hooks` lists *HeurChain memory recall*.
- **Codex CLI:** `/mcp` lists `heurchain_local`. Open `/hooks` and **trust**
  *HeurChain memory recall*. Codex does not run a new or changed hook until you do,
  and setup never writes trust state for you.

### Where things go

| What | Where |
|---|---|
| API key | Windows: user environment variable `HEURCHAIN_LOCAL_<CLIENT>_KEY` (HKCU\Environment). macOS/Linux: `~/.heurchain/credentials.env`, mode 0600. Never in a client config, the state file or setup output |
| Bridge / headers helper, hook runtime, secret-free state | `~/.heurchain/local/<client>/` (`heurchain-mcp-bridge.mjs`, `heurchain-recall-hook.mjs`, `client.json`) |
| Hook registration | Claude Code: `~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR`). Codex: `$CODEX_HOME/hooks.json` |
| MCP entry | Written by the client's own `mcp add` command |

Self-hosted files live under `~/.heurchain/local/`, apart from a hosted HeurChain
setup in `~/.heurchain/<client>/`. The two never replace each other's hook. Both
use the same skill path. Setup replaces an existing `heurchain-memory` skill there
(with a backup) and refuses to overwrite any other file.

### Repeat setup, upgrade and uninstall

- **Repeat:** running `connect --apply` again with the same inputs changes nothing.
  The key can be left out on a repeat run; the stored one is reused.
- **Upgrade:** after `git pull`, run `connect --apply` again. Changed runtimes and a
  changed hook command are replaced in place. Our hook entry is never duplicated,
  and a changed MCP entry is removed and re-added. Codex asks you to trust the
  updated hook again.
- **Uninstall:** `node tools/setup/cli.mjs uninstall --client claude --apply` removes
  the MCP entry, the hook registration, the copied files and the stored key
  (`--keep-key` keeps it). The skill is removed only if it is unchanged. Then revoke
  the key on the broker (`DELETE /admin/tenants/{id}/keys/{key_id}`) if it is no
  longer used.
- `node tools/setup/cli.mjs status --client claude` shows what is installed, and
  whether a key is stored (by name only).

Every file change is a dry run without `--apply`. Each applied change works like
this:
- It takes a lock and computes the merged file first.
- It **validates** the merge before anything is replaced. The result must parse.
  Every setting and hook setup does not own must be unchanged. The HeurChain entry
  must appear exactly once, or not at all after removal.
- It keeps a private backup and receipt next to the file.
- It swaps the new file in with an atomic rename, and refuses if the file changed
  meanwhile.

`node tools/setup/cli.mjs rollback --file FILE --backup BACKUP` restores a backup.
Unrelated hooks (including other `UserPromptSubmit` hooks) and settings are kept as
they are, and a file with nothing to change is left byte-identical. MCP entries are
added and removed only through the client's own `mcp add` / `mcp remove` command,
by name, so other MCP servers stay in place. After adding, setup reads the entry
back through the client and stops if it is not exactly what was requested.

## Automatic recall

What it does, which `connect` also prints as `recall.disclosure`: before a prompt
that looks like it depends on earlier work, the hook sends up to 300 characters of
the prompt (obvious secrets removed) to `heurchain_search` on your broker. It then
adds up to three matching excerpts to that prompt, labelled
`<heurchain-recall initiator="hook">`. Self-contained prompts (sorting,
arithmetic, values supplied in the prompt, slash commands) make no request at all.
A timeout, outage or missing key lets the prompt through unchanged: the hook always
exits 0 and never blocks a prompt.

**Turning it off**, from narrowest to widest:

| Switch | Effect |
|---|---|
| `HEURCHAIN_RECALL_HOOK=off` in the client's environment | No lookups for that client process and its sessions |
| `"enabled": false` in `~/.heurchain/local/<client>/client.json` | No lookups, hook stays registered |
| `connect --no-recall-hook --apply` | Unregisters an existing HeurChain recall hook from this setup and removes its runtime. MCP and skill stay |
| `uninstall --apply` | Removes everything setup added |

**Credential.** The 0.6.1-oss.1 broker has no read-only API keys. A key can read and
write in its tenant, and `POST /admin/tenants` issues one key per tenant. Recall
therefore uses the same tenant key as the client's MCP entry. The hook only ever
sends `tools/call heurchain_search`. A separate read-only recall credential is
**not available** on this edition.

**Relevance.** The gate is a fixed keyword rule (rollout, release, migration,
incident, "we agreed", "last time", …). It misses some prompts that need memory and
fetches for some that do not. Lexical hits pass without a similarity floor, so an
unrelated record that shares common words can be injected. Excerpts are labelled as
untrusted evidence, and the skill tells the model to ignore unrelated ones. The
model can always call `heurchain_search` itself.

Capture or compaction hooks are **not** part of this setup. Saving stays a model
decision guided by the skill.

## Other MCP clients

Any client that supports Streamable HTTP with custom headers can connect directly:
URL `http://127.0.0.1:8080/mcp/sse`, headers `X-HeurChain-API-Key` and
`X-HeurChain-Tenant`, supplied through the client's own secret mechanism. Setup does
not write configuration for these clients, and automatic recall is not offered for
them.

## Qualification

`tools/setup/native-check.mjs` is the check behind the qualified list. Run it
**only against a disposable broker**: it stores, then forgets, one synthetic record.

```sh
HEURCHAIN_API_KEY=... node tools/setup/native-check.mjs --url http://127.0.0.1:8080 \
  --tenant <tenant_id> --output ./native-check-out [--client claude|codex]
```

Each case does the following:
- Installs into a disposable profile with the real `connect --apply`.
- Runs the installed client against a loopback model stand-in, which records each
  model request; no vendor model call is made.
- Sends the bridge and hook through a loopback proxy in front of the real broker,
  which counts hook lookups.

On Windows, Codex cases briefly store the synthetic key as the
`HEURCHAIN_LOCAL_CODEX_KEY` user variable, because Codex does not pass environment
variables to stdio MCP servers. They refuse to run if that variable already exists,
and remove it afterwards.

Results on 2026-11-20 (Windows 11 Pro 10.0.26200 x64, Node 24.18.1, broker
0.6.1-oss.1 from this repository's Compose file; receipt:
[native-receipts.json](qualification/agent-setup-20261120/native-receipts.json)):

| Client | Case | Hook lookups | Fact in first model request | HeurChain tools in first request | Answer used the fact | Result |
|---|---|---:|---|---:|---|---|
| Claude Code 2.1.329 | Prompt that depends on a stored decision | 1 | yes | 24 | yes | pass |
| Claude Code 2.1.329 | "Alphabetize these words…" | 0 | no | 24 | no | pass |
| Claude Code 2.1.329 | Broker answers the hook with 503 | 1 | no | 24 | no | pass |
| Claude Code 2.1.329 | `connect --no-recall-hook` after a default install | 0 | no | 24 | no | pass |
| Claude Code 2.1.329 | `HEURCHAIN_RECALL_HOOK=off` | 0 | no | 24 | no | pass |
| Claude Code 2.1.329 | Older runtime + stale hook command, then `connect` again | 1 | yes | 24 | yes | pass |
| Claude Code 2.1.329 | Model calls `heurchain_search` over the MCP entry | 0 | no | 24 | yes | pass |
| Codex CLI 0.170.0 | Prompt that depends on a stored decision | 1 | yes | 24 | yes | pass |
| Codex CLI 0.170.0 | "Alphabetize these words…" | 0 | no | 24 | no | pass |
| Codex CLI 0.170.0 | Broker answers the hook with 503 | 1 | no | 24 | no | pass |
| Codex CLI 0.170.0 | Hook installed but not trusted in `/hooks` | 0 | no | 24 | no | pass |
| Codex CLI 0.170.0 | `connect --no-recall-hook` after a default install | 0 | no | 24 | no | pass |

Every case also checked that unrelated settings and hooks in the profile were kept.
Claude Code additionally listed `heurchain-local … Connected` in `claude mcp list`
(headers helper path). In the upgrade case the runtime was replaced, the hook was
registered exactly once, and the stale command was gone. Codex reached the broker
through the stdio bridge (`initialize`, `tools/list`) with the key read from the
Windows user environment. A separate disposable run showed that an unrelated Claude
Code MCP server survives `connect`, a repeat `connect` (no changes, stored key
reused) and `uninstall`.

Not verified: macOS, Linux and WSL execution, the Codex app, any other client
build, and a real model provider. The stand-in shows what reaches the first model
request, not how a model uses it.
