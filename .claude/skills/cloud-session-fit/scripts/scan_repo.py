#!/usr/bin/env python3
"""Scan a git checkout for signals that decide how well it fits Claude Code cloud sessions.

Stdlib only, read-only. It reports facts and candidate plays; the agent running the
cloud-session-fit skill verifies them against the code before recommending anything.

Usage:
    python3 scan_repo.py [PATH ...] [--json]

With several paths it prints one compact row per repo (portfolio view).
"""

import ipaddress
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

SKIP_DIRS = {
    ".git", "node_modules", ".venv", "venv", "env", "__pycache__", "dist", "build",
    "target", ".next", ".nuxt", "vendor", ".tox", ".mypy_cache", ".pytest_cache",
    "site-packages", ".gradle", ".idea", ".terraform", "coverage",
    ".claude",  # read separately; skipping keeps this skill from flagging its own patterns
}
TEXT_EXTS = {
    ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".go", ".rs", ".rb", ".php",
    ".java", ".kt", ".cs", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".sh", ".ps1",
    ".yml", ".yaml", ".json", ".toml", ".ini", ".cfg", ".conf", ".env", ".example",
    ".j2", ".tf", ".tfvars", ".md", ".txt", ".dockerfile", ".service", ".sql", ".lua",
}
TEXT_NAMES = {"Dockerfile", "Makefile", "Procfile", "Jenkinsfile", ".env.example", "nginx.conf"}
MAX_FILE_BYTES = 1_000_000
MAX_FILES = 6000

# Toolchains pre-installed on the Anthropic-hosted image (snapshot; see references/cloud-capabilities.md).
PREINSTALLED = {"python", "node", "ruby", "php", "java", "go", "rust", "c/c++", "docker", "postgres", "redis"}

# Subset of the default "Trusted" allowlist, as suffix patterns. Hosts outside need Custom/Full network access.
TRUSTED_SUFFIXES = [
    "anthropic.com", "claude.com", "claude.ai", "github.com", "githubusercontent.com", "gitlab.com",
    "bitbucket.org", "docker.io", "docker.com", "ghcr.io", "gcr.io", "quay.io", "mcr.microsoft.com",
    "googleapis.com", "google.com", "azure.com", "microsoft.com", "microsoftonline.com", "amazonaws.com",
    "api.aws", "oracle.com", "java.com", "java.net", "r2.cloudflarestorage.com", "npmjs.org", "npmjs.com",
    "yarnpkg.com", "jsr.io", "pypi.org", "pythonhosted.org", "python.org", "rubygems.org", "ruby-lang.org",
    "crates.io", "rust-lang.org", "rustup.rs", "golang.org", "go.dev", "maven.org", "apache.org",
    "gradle.org", "gradle-dn.com", "packagist.org", "nuget.org", "pub.dev", "hex.pm", "cpan.org",
    "metacpan.org", "cocoapods.org", "haskell.org", "swift.org", "ubuntu.com", "launchpad.net",
    "nixos.org", "k8s.io", "hashicorp.com", "anaconda.com", "anaconda.org", "continuum.io",
    "eclipse.org", "nodejs.org", "apple.com", "android.com", "stainless.com", "prisma.sh",
    "datadoghq.com", "datadoghq.eu", "honeycomb.io", "sourceforge.net", "packagecloud.io",
    "gstatic.com", "json-schema.org", "schemastore.org", "modelcontextprotocol.io", "dot.net",
    "visualstudio.com",
]
# Hosts that show up in docs/examples and say nothing about runtime reachability.
IGNORE_HOSTS = {
    "example.com", "example.org", "localhost", "img.shields.io", "shields.io", "mermaid.ink",
    "opensource.org", "creativecommons.org", "schema.org", "w3.org", "www.w3.org", "mintcdn.com",
}

URL_RE = re.compile(r"\b(?:https?|wss?|redis|postgres(?:ql)?|mongodb|amqp|mysql)://([A-Za-z0-9._-]+)(?::(\d+))?")
IPV4_RE = re.compile(r"(?<![\d.])((?:\d{1,3}\.){3}\d{1,3})(?![\d.])")
LAN_NAME_RE = re.compile(r"\b[A-Za-z0-9-]+\.(?:local|lan|home\.arpa|internal|ts\.net)(?![.\w-])")
DOCKER_HOST_RE = re.compile(r"host\.docker\.internal")
ENV_REF_RE = re.compile(
    r"(?:os\.environ(?:\.get)?\(\s*[\"']|os\.getenv\(\s*[\"']|process\.env\.|process\.env\[\s*[\"']|"
    r"ENV\[\s*[\"']|getenv\(\s*[\"']|\$\{)([A-Z][A-Z0-9_]{2,})"
)
SECRETISH_RE = re.compile(r"(SECRET|PASSWORD|PASSWD|CREDENTIAL|PRIVATE_KEY)|(_KEY|TOKEN|_AUTH)$")
HARDWARE_PATTERNS = {
    "gpu/cuda": re.compile(r"\b(cuda|nvidia-smi|nvidia/cuda|--gpus|rocm|torch\.cuda|CUDA_VISIBLE_DEVICES)\b", re.I),
    "local-llm": re.compile(r"\b(ollama|llama\.cpp|llama-server|vllm|lm ?studio|gguf)\b", re.I),
    "hypervisor/storage": re.compile(r"\b(proxmox|pvesh|pveproxy|qm |pct |ceph|zfs|zpool)\b", re.I),
    "host-devices": re.compile(r"(/dev/(?:dri|video|snd|ttyUSB|ttyACM|nvidia)|--device[= ])"),
    "systemd/host-services": re.compile(r"\b(systemctl|\.service\b|journalctl)"),
    "remote-ssh-deploy": re.compile(r"\b(ansible_host|ssh -|scp |rsync .*:|ansible-playbook)\b"),
    "desktop/gui": re.compile(r"\b(electron|pyqt|tkinter|xcodebuild|winforms|wpf)\b", re.I),
}
MONITORING_RE = re.compile(r"\b(prometheus|alertmanager|grafana|sentry|datadog|pagerduty|opsgenie)\b", re.I)


def run(cmd, cwd):
    try:
        out = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=30)
        return out.stdout.strip() if out.returncode == 0 else ""
    except Exception:
        return ""


def is_private_ip(text):
    try:
        ip = ipaddress.ip_address(text)
    except ValueError:
        return None
    if ip.is_loopback:
        return "loopback"
    if ip in ipaddress.ip_network("100.64.0.0/10"):
        return "cgnat/tailscale"
    if ip.is_private and not ip.is_unspecified:
        # Docker bridge gateways (172.17-19.0.1) are container plumbing, not LAN dependencies.
        if str(ip).startswith(("172.17.", "172.18.", "172.19.")):
            return "docker-bridge"
        return "lan"
    return None


def host_trusted(host):
    host = host.lower().rstrip(".")
    return any(host == s or host.endswith("." + s) for s in TRUSTED_SUFFIXES)


def walk(root):
    count = 0
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            p = Path(dirpath) / name
            count += 1
            if count > MAX_FILES:
                return
            yield p


def is_text_candidate(p):
    return p.suffix.lower() in TEXT_EXTS or p.name in TEXT_NAMES or p.name.startswith((".env", "Dockerfile"))


def read_text(p):
    try:
        if p.stat().st_size > MAX_FILE_BYTES:
            return ""
        return p.read_text(encoding="utf-8", errors="ignore")
    except Exception:
        return ""


def git_ignored(root, rel):
    try:
        r = subprocess.run(["git", "check-ignore", "-q", "--no-index", rel], cwd=root, timeout=10)
        return r.returncode == 0
    except Exception:
        return False


def scan(root):
    root = Path(root).resolve()
    rel = lambda p: str(p.relative_to(root))
    r = {
        "path": str(root),
        "name": root.name,
        "git": {},
        "stacks": [],
        "not_preinstalled": [],
        "manifests": [],
        "tests": {"dirs": [], "file_count": 0, "commands": []},
        "ci": [],
        "containers": {"dockerfiles": [], "compose": [], "images": []},
        "infra": [],
        "claude": {},
        "network": {"lan_refs": {}, "loopback_refs": 0, "docker_host_refs": [], "lan_names": [],
                     "external_hosts_untrusted": {}, "external_hosts_trusted": []},
        "hardware": {},
        "monitoring": [],
        "env_vars": {"declared": [], "secretish": []},
        "risk_files": [],
        "docs": [],
        "todo_count": 0,
        "large_files": [],
        "gitignore_blocks": [],
    }

    # --- git facts
    remotes = run(["git", "remote", "-v"], root)
    urls = sorted({line.split()[1] for line in remotes.splitlines() if len(line.split()) > 1})
    host = "none"
    if any("github.com" in u for u in urls):
        host = "github"
    elif any("gitlab" in u for u in urls):
        host = "gitlab"
    elif urls:
        host = "other"
    last = run(["git", "log", "-1", "--format=%cI"], root)
    recent = run(["git", "rev-list", "--count", "--since=30.days", "HEAD"], root)
    tracked = run(["git", "ls-files"], root).splitlines()
    r["git"] = {
        "remotes": urls,
        "host": host,
        "branch": run(["git", "rev-parse", "--abbrev-ref", "HEAD"], root),
        "last_commit": last,
        "days_since_commit": (datetime.now(timezone.utc) - datetime.fromisoformat(last)).days if last else None,
        "commits_30d": int(recent) if recent.isdigit() else None,
        "tracked_files": len(tracked),
        "lfs": ".gitattributes" in tracked and "filter=lfs" in read_text(root / ".gitattributes"),
    }

    # --- claude config that reaches cloud sessions only when committed
    settings = root / ".claude" / "settings.json"
    hooks = {}
    if settings.exists():
        try:
            hooks = json.loads(read_text(settings)).get("hooks", {})
        except Exception:
            hooks = {"_error": "unparseable"}
    mcp = {}
    mcp_path = root / ".mcp.json"
    if mcp_path.exists():
        try:
            for name, cfg in json.loads(read_text(mcp_path)).get("mcpServers", {}).items():
                mcp[name] = cfg.get("url") or cfg.get("command", "")
        except Exception:
            mcp = {"_error": "unparseable"}
    r["claude"] = {
        "CLAUDE.md": (root / "CLAUDE.md").exists() or (root / ".claude" / "CLAUDE.md").exists(),
        "settings.json": settings.exists(),
        "session_start_hook": "SessionStart" in hooks,
        "hook_events": sorted(k for k in hooks if not k.startswith("_")),
        "skills": sorted(p.name for p in (root / ".claude" / "skills").glob("*") if p.is_dir()),
        "agents": sorted(p.stem for p in (root / ".claude" / "agents").glob("*.md")),
        "commands": sorted(p.stem for p in (root / ".claude" / "commands").glob("*.md")),
        "mcp_servers": mcp,
    }
    for probe, why in [
        (".claude/settings.json", ".claude/settings.json (hooks, permissions) is gitignored, so cloud sessions never see it"),
        (".claude/skills/x/SKILL.md", ".claude/skills/ is gitignored, so project skills never reach cloud sessions"),
        (".mcp.json", ".mcp.json is gitignored, so project MCP servers never reach cloud sessions"),
        ("CLAUDE.md", "CLAUDE.md is gitignored"),
        ("scripts/setup.sh", "*.sh is gitignored, so a committed hook/setup shell script would be dropped"),
    ]:
        if git_ignored(root, probe):
            r["gitignore_blocks"].append(why)

    # --- file walk
    stacks, not_pre, manifests = set(), set(), []
    test_files = 0
    test_dirs = set()
    env_declared, env_secretish = set(), set()
    lan_refs, lan_names, untrusted, trusted = {}, set(), {}, set()
    loopback = 0
    docker_host = set()
    hardware = {k: set() for k in HARDWARE_PATTERNS}
    monitoring = set()
    todo = 0

    for p in walk(root):
        name, rp = p.name, rel(p)
        low = name.lower()
        parts = set(Path(rp).parts[:-1])

        # manifests -> stacks
        if name == "package.json" and "node_modules" not in parts:
            stacks.add("node"); manifests.append(rp)
            try:
                pkg = json.loads(read_text(p))
                for s in ("test", "lint", "build", "typecheck", "check"):
                    cmd = pkg.get("scripts", {}).get(s)
                    if cmd and "no test specified" not in cmd:
                        r["tests"]["commands"].append(f"{rp}: npm run {s}  ({cmd})")
                if "engines" in pkg:
                    r.setdefault("node_engines", {})[rp] = pkg["engines"]
            except Exception:
                pass
        elif name in ("pyproject.toml", "setup.py", "setup.cfg", "Pipfile") or re.fullmatch(r"requirements.*\.txt", name):
            stacks.add("python"); manifests.append(rp)
        elif name == "go.mod":
            stacks.add("go"); manifests.append(rp)
        elif name == "Cargo.toml":
            stacks.add("rust"); manifests.append(rp)
        elif name == "Gemfile":
            stacks.add("ruby"); manifests.append(rp)
        elif name == "composer.json":
            stacks.add("php"); manifests.append(rp)
        elif name in ("pom.xml", "build.gradle", "build.gradle.kts"):
            stacks.add("java"); manifests.append(rp)
        elif low.endswith((".csproj", ".sln", ".fsproj")):
            stacks.add("dotnet"); not_pre.add("dotnet (install in setup script)"); manifests.append(rp)
        elif name == "pubspec.yaml":
            stacks.add("dart/flutter"); not_pre.add("dart/flutter SDK"); manifests.append(rp)
        elif name == "mix.exs":
            stacks.add("elixir"); not_pre.add("elixir/erlang"); manifests.append(rp)
        elif name == "Package.swift":
            stacks.add("swift"); not_pre.add("swift toolchain (Linux only; no Xcode)"); manifests.append(rp)
        elif name in ("deno.json", "deno.jsonc"):
            stacks.add("deno"); not_pre.add("deno"); manifests.append(rp)
        elif name in ("CMakeLists.txt",) or (name == "Makefile" and len(Path(rp).parts) == 1):
            stacks.add("c/c++" if name == "CMakeLists.txt" else "make"); manifests.append(rp)
        elif name == "pixi.toml" or name == "environment.yml":
            stacks.add("conda"); not_pre.add("conda/mamba"); manifests.append(rp)

        # containers / infra
        if name.startswith("Dockerfile") or low.endswith(".dockerfile"):
            r["containers"]["dockerfiles"].append(rp)
            for m in re.finditer(r"^\s*FROM\s+(\S+)", read_text(p), re.M | re.I):
                r["containers"]["images"].append(m.group(1))
        if re.fullmatch(r"(docker-)?compose.*\.ya?ml(\.j2)?", low):
            r["containers"]["compose"].append(rp)
            for m in re.finditer(r"^\s*image:\s*[\"']?([^\s\"'{]+)", read_text(p), re.M):
                r["containers"]["images"].append(m.group(1))
        if name in ("playbook.yml", "site.yml", "ansible.cfg"):
            r["infra"].append(f"ansible ({rp})")
        if low.endswith(".tf"):
            r["infra"].append(f"terraform ({rp})")
        if name in ("Chart.yaml", "kustomization.yaml"):
            r["infra"].append(f"kubernetes ({rp})")

        # CI
        if rp.startswith(".github/workflows/") and low.endswith((".yml", ".yaml")):
            r["ci"].append(rp)
        elif name in (".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", ".travis.yml") or rp.startswith(".circleci/"):
            r["ci"].append(rp)

        # tests
        if parts & {"tests", "test", "__tests__", "spec", "e2e"}:
            test_dirs.update(d for d in Path(rp).parts[:-1] if d in {"tests", "test", "__tests__", "spec", "e2e"})
        if re.search(r"(^test_.*\.py$|_test\.(py|go)$|\.(spec|test)\.[cm]?[jt]sx?$|Test\.java$|_spec\.rb$)", name):
            test_files += 1

        # docs
        if len(Path(rp).parts) == 1 and re.match(r"(README|CHANGELOG|CONTRIBUTING|ARCHITECTURE)", name, re.I):
            r["docs"].append(rp)
        elif Path(rp).parts[0] == "docs" and len(r["docs"]) < 40:
            r["docs"].append(rp)

        # risky files committed in the tree
        if re.fullmatch(r"(\.env|.*\.pem|id_rsa|id_ed25519|.*\.tfvars|.*\.key|\.vault_pass|secrets?\.ya?ml)", name) \
                and rp in set(tracked):
            r["risk_files"].append(rp)
        if name == ".env.example" or name.endswith(".env.example") or name == "env.example":
            for m in re.finditer(r"^\s*([A-Z][A-Z0-9_]+)\s*=", read_text(p), re.M):
                env_declared.add(m.group(1))

        try:
            size = p.stat().st_size
        except Exception:
            size = 0
        if size > 50_000_000:
            r["large_files"].append(f"{rp} ({size // 1_000_000} MB)")

        if not is_text_candidate(p):
            continue
        text = read_text(p)
        if not text:
            continue
        is_doc = low.endswith(".md")

        todo += len(re.findall(r"\b(TODO|FIXME|XXX|HACK)\b", text)) if not is_doc else 0

        for m in IPV4_RE.finditer(text):
            kind = is_private_ip(m.group(1))
            if kind == "lan" or kind == "cgnat/tailscale":
                lan_refs.setdefault(m.group(1), set()).add(rp)
            elif kind == "loopback":
                loopback += 1
        if DOCKER_HOST_RE.search(text):
            docker_host.add(rp)
        for m in LAN_NAME_RE.finditer(text):
            if not m.group(0).endswith(("docker.internal",)):
                lan_names.add(m.group(0))
        for m in URL_RE.finditer(text):
            h = m.group(1).lower().strip(".")
            if h in IGNORE_HOSTS or IPV4_RE.fullmatch(h) or "." not in h or h.endswith("docker.internal"):
                continue
            if "<" in h or "{" in h or h.startswith(("your", "my-", "x.")):
                continue
            if host_trusted(h):
                trusted.add(h)
            elif not is_doc:
                untrusted.setdefault(h, set()).add(rp)
        if not is_doc:
            for m in ENV_REF_RE.finditer(text):
                var = m.group(1)
                env_declared.add(var)
            for k, pat in HARDWARE_PATTERNS.items():
                if pat.search(text):
                    hardware[k].add(rp)
        if not is_doc and MONITORING_RE.search(text):
            monitoring.update(x.lower() for x in MONITORING_RE.findall(text))

    for v in env_declared:
        if SECRETISH_RE.search(v):
            env_secretish.add(v)

    r["stacks"] = sorted(stacks)
    r["not_preinstalled"] = sorted(not_pre)
    r["manifests"] = manifests[:40]
    r["tests"]["dirs"] = sorted(test_dirs)
    r["tests"]["file_count"] = test_files
    r["network"]["lan_refs"] = {ip: sorted(files)[:8] for ip, files in sorted(lan_refs.items())}
    r["network"]["loopback_refs"] = loopback
    r["network"]["docker_host_refs"] = sorted(docker_host)[:15]
    r["network"]["lan_names"] = sorted(lan_names)[:20]
    r["network"]["external_hosts_untrusted"] = {h: sorted(f)[:5] for h, f in sorted(untrusted.items())}
    r["network"]["external_hosts_trusted"] = sorted(trusted)[:40]
    r["hardware"] = {k: sorted(v)[:8] for k, v in hardware.items() if v}
    r["monitoring"] = sorted(monitoring)
    r["env_vars"]["declared"] = sorted(env_declared)[:80]
    r["env_vars"]["secretish"] = sorted(env_secretish)
    r["containers"]["images"] = sorted({i for i in r["containers"]["images"] if not i.endswith(":")})
    r["todo_count"] = todo

    r["assessment"] = assess(r)
    return r


def assess(r):
    """Turn raw signals into blockers, warnings and candidate plays. Heuristic: verify before trusting."""
    blockers, warnings, plays = [], [], []
    g, net, c = r["git"], r["network"], r["claude"]

    if g["host"] == "none":
        blockers.append("No git remote: cloud sessions need a GitHub repo (or `CCR_FORCE_BUNDLE=1 claude --cloud` uploads a bundle, but results can't be pushed back).")
    elif g["host"] != "github":
        blockers.append(f"Remote is {g['host']}: cloning and PRs need GitHub; a bundle upload works but can't push back.")
    for why in r["gitignore_blocks"]:
        warnings.append(f"gitignore: {why}.")
    if r["not_preinstalled"]:
        warnings.append("Toolchains not on the base image (add to the environment setup script): " + ", ".join(r["not_preinstalled"]))
    if net["lan_refs"]:
        warnings.append(f"{len(net['lan_refs'])} private/LAN address(es) referenced ({', '.join(list(net['lan_refs'])[:5])}). "
                        "Cloud VMs can't reach your LAN: anything that must talk to these runs locally (Remote Control) or on a self-hosted environment.")
    if net["docker_host_refs"]:
        warnings.append("host.docker.internal used: in the cloud VM it resolves to the VM, not your workstation; services expected there (e.g. Ollama) won't exist unless started in-session.")
    if net["lan_names"]:
        warnings.append("LAN/tailnet hostnames referenced: " + ", ".join(net["lan_names"][:6]))
    if net["external_hosts_untrusted"]:
        warnings.append("Hosts outside the default Trusted allowlist (need Custom network access or a connector): "
                        + ", ".join(list(net["external_hosts_untrusted"])[:12]))
    for k in r["hardware"]:
        if k in ("gpu/cuda", "local-llm", "hypervisor/storage", "host-devices", "desktop/gui"):
            warnings.append(f"Hardware/host coupling ({k}) in: {', '.join(r['hardware'][k][:4])}. VM is 4 vCPU / 16 GB / 30 GB disk, no GPU.")
    if r["env_vars"]["secretish"]:
        warnings.append("Secret-like env vars referenced (" + ", ".join(r["env_vars"]["secretish"][:10]) + "). "
                        "Environment variables are readable by anyone using the environment; on Pro/Max use API credentials, else use test doubles.")
    if r["risk_files"]:
        blockers.append("Secret-looking files are tracked in git: " + ", ".join(r["risk_files"][:6]))
    if r["large_files"] or g["lfs"]:
        warnings.append("Large files / Git LFS present: clone time and 30 GB disk budget matter.")

    has_tests = r["tests"]["file_count"] > 0 or r["tests"]["commands"]
    has_ci = bool(r["ci"])
    gh = g["host"] == "github"

    if not has_tests:
        plays.append(("Bootstrap a test harness + CI in a cloud session", "high",
                      "No tests detected. A cloud session can write smoke/unit tests and a GitHub Actions workflow without touching your machine; this unlocks every other play."))
    else:
        plays.append(("Parallel `claude --cloud` tasks for backlog items", "high",
                      f"Tests exist ({r['tests']['file_count']} files) so Claude can verify its own changes in the VM."))
    if gh and has_ci:
        plays.append(("Auto-fix PRs (CI failures + review comments)", "high",
                      f"{len(r['ci'])} CI workflow(s) found; install the Claude GitHub App and turn on Auto-fix or run /autofix-pr."))
    if gh:
        plays.append(("Routine on `pull_request.opened`: repo-specific review checklist", "medium",
                      "GitHub-triggered routine applying this repo's own conventions."))
    if r["containers"]["compose"]:
        plays.append(("Ephemeral integration stack in the VM (`docker compose up` via SessionStart hook)", "high",
                      f"Compose file(s): {', '.join(r['containers']['compose'][:3])}. Docker works in the VM, but Docker Hub may 429 (use mirror.gcr.io) and in-build package installs can't reach the proxy; running the services natively is often simpler."))
    if r["manifests"]:
        plays.append(("Weekly dependency-update routine (opens a PR after tests pass)", "medium",
                      f"{len(r['manifests'])} dependency manifest(s)."))
    if r["docs"]:
        plays.append(("Docs/changelog drift routine (weekly or on release)", "medium",
                      f"Docs found: {', '.join(r['docs'][:4])}."))
    if r["monitoring"]:
        plays.append(("API-triggered alert-triage routine (monitoring POSTs to the routine /fire endpoint)", "medium",
                      f"Monitoring stack referenced: {', '.join(r['monitoring'][:4])}. Outbound from LAN to the routine works even though the cloud can't reach in."))
    if net["lan_refs"] or r["hardware"].get("remote-ssh-deploy") or r["hardware"].get("hypervisor/storage"):
        plays.append(("Keep deploy/ops local: Remote Control or a self-hosted environment", "info",
                      "LAN-bound deploy or infra steps can't run on Anthropic-hosted VMs; author the change in the cloud, apply it from your network."))
    if r["todo_count"] >= 10:
        plays.append(("Fan out TODO/FIXME cleanup as parallel cloud sessions", "low",
                      f"{r['todo_count']} TODO/FIXME markers in code."))
    if not c["session_start_hook"]:
        plays.append(("Add a SessionStart hook (guarded by CLAUDE_CODE_REMOTE) that installs deps", "high",
                      "Makes every cloud session start test-ready; the session-start-hook skill can generate it."))
    if not c["CLAUDE.md"]:
        plays.append(("Add a CLAUDE.md (build/test commands, conventions)", "medium",
                      "Cloud sessions start cold; CLAUDE.md is the cheapest context they get."))

    # Coarse verdict
    hard = len(blockers)
    lan_heavy = bool(net["lan_refs"]) and len(net["lan_refs"]) >= 1
    hw = any(k in r["hardware"] for k in ("gpu/cuda", "hypervisor/storage", "host-devices", "desktop/gui"))
    if hard:
        verdict = "blocked"
    elif not has_tests and (lan_heavy or hw):
        verdict = "partial"
    elif lan_heavy or hw or r["not_preinstalled"] or net["external_hosts_untrusted"]:
        verdict = "ready-with-setup"
    else:
        verdict = "ready"
    rank = {"high": 0, "medium": 1, "low": 2, "info": 3}
    plays.sort(key=lambda p: rank[p[1]])
    return {"verdict": verdict, "blockers": blockers, "warnings": warnings,
            "candidate_plays": [{"play": p, "value": v, "why": w} for p, v, w in plays]}


def print_report(r):
    a = r["assessment"]
    g = r["git"]
    print(f"# {r['name']}  —  verdict: {a['verdict']}")
    print(f"remote: {', '.join(g['remotes']) or 'none'}  | branch {g['branch']} | last commit {g['last_commit'][:10] if g['last_commit'] else '?'}"
          f" ({g['days_since_commit']}d ago, {g['commits_30d']} commits/30d) | {g['tracked_files']} tracked files")
    print(f"stacks: {', '.join(r['stacks']) or '-'}   not preinstalled: {', '.join(r['not_preinstalled']) or '-'}")
    print(f"tests: {r['tests']['file_count']} files, dirs {r['tests']['dirs'] or '-'}; commands: {r['tests']['commands'] or '-'}")
    print(f"ci: {r['ci'] or '-'}")
    print(f"containers: dockerfiles {r['containers']['dockerfiles'] or '-'}; compose {r['containers']['compose'] or '-'}; images {r['containers']['images'] or '-'}")
    print(f"infra: {r['infra'] or '-'}   monitoring: {r['monitoring'] or '-'}")
    print(f"claude: {json.dumps(r['claude'])}")
    print(f"network: LAN {json.dumps(r['network']['lan_refs'])}")
    print(f"         host.docker.internal in {r['network']['docker_host_refs'] or '-'}; loopback refs {r['network']['loopback_refs']}")
    print(f"         untrusted hosts {json.dumps(r['network']['external_hosts_untrusted'])}")
    print(f"hardware: {json.dumps(r['hardware'])}")
    print(f"env vars (secret-like): {r['env_vars']['secretish'] or '-'}")
    print(f"docs: {r['docs'][:8]}   TODO/FIXME: {r['todo_count']}   large files: {r['large_files'] or '-'}")
    print("\n## Blockers")
    for b in a["blockers"] or ["none"]:
        print(f"- {b}")
    print("\n## Warnings")
    for w in a["warnings"] or ["none"]:
        print(f"- {w}")
    print("\n## Candidate plays (verify before recommending)")
    for p in a["candidate_plays"]:
        print(f"- [{p['value']}] {p['play']} — {p['why']}")


def main(argv):
    as_json = "--json" in argv
    paths = [a for a in argv if not a.startswith("--")] or ["."]
    results = [scan(p) for p in paths]
    if as_json:
        print(json.dumps(results if len(results) > 1 else results[0], indent=2, default=list))
    elif len(results) == 1:
        print_report(results[0])
    else:
        print("| repo | verdict | stacks | tests | CI | compose | LAN refs | top play |")
        print("|---|---|---|---|---|---|---|---|")
        for r in results:
            a = r["assessment"]
            top = a["candidate_plays"][0]["play"] if a["candidate_plays"] else "-"
            print(f"| {r['name']} | {a['verdict']} | {', '.join(r['stacks'])} | {r['tests']['file_count']} | "
                  f"{len(r['ci'])} | {'yes' if r['containers']['compose'] else 'no'} | {len(r['network']['lan_refs'])} | {top} |")


if __name__ == "__main__":
    main(sys.argv[1:])
