#!/usr/bin/env python3
"""Rank a list of repositories by how worthwhile a cloud-session assessment is right now.

Input (file path or stdin) is JSON in either shape:
  - claude-code-remote list_repos output: {"repos": [{"full_name", "pushed_at", "visibility", "fork", "archived"}, ...]}
  - gh CLI: gh repo list OWNER --limit 200 --json nameWithOwner,pushedAt,isFork,isArchived,visibility,primaryLanguage

Metadata only: it says where to look first, not whether a repo fits. Run scan_repo.py on a clone for that.

Usage:
    python3 portfolio_triage.py repos.json [--active-days 45] [--warm-days 180]
"""

import json
import sys
from datetime import datetime, timezone


def load(src):
    data = json.load(open(src) if src and src != "-" else sys.stdin)
    rows = data.get("repos", data) if isinstance(data, dict) else data
    out = []
    for r in rows:
        out.append({
            "name": r.get("full_name") or r.get("nameWithOwner") or r.get("name"),
            "pushed": r.get("pushed_at") or r.get("pushedAt"),
            "visibility": (r.get("visibility") or "").lower(),
            "fork": bool(r.get("fork") or r.get("isFork")),
            "archived": bool(r.get("archived") or r.get("isArchived")),
            "lang": (r.get("primaryLanguage") or {}).get("name", "") if isinstance(r.get("primaryLanguage"), dict) else (r.get("language") or ""),
        })
    return out


def main(argv):
    active, warm = 45, 180
    args = []
    i = 0
    while i < len(argv):
        if argv[i] == "--active-days":
            active = int(argv[i + 1]); i += 2
        elif argv[i] == "--warm-days":
            warm = int(argv[i + 1]); i += 2
        else:
            args.append(argv[i]); i += 1
    repos = load(args[0] if args else "-")
    now = datetime.now(timezone.utc)
    for r in repos:
        try:
            r["age"] = (now - datetime.fromisoformat(r["pushed"].replace("Z", "+00:00"))).days
        except Exception:
            r["age"] = 10_000
        if r["archived"]:
            r["tier"], r["note"] = "skip", "archived"
        elif r["fork"]:
            r["tier"], r["note"] = "skip", "fork: upstream owns CI/tests; assess only if you carry patches"
        elif r["age"] <= active:
            r["tier"], r["note"] = "1-active", "assess now: fast feedback on cloud tasks, auto-fix, routines"
        elif r["age"] <= warm:
            r["tier"], r["note"] = "2-warm", "good for maintenance routines (deps, docs drift) and backlog fan-out"
        else:
            r["tier"], r["note"] = "3-cold", "only if revived; a one-off cloud session can modernize/archive"
    repos.sort(key=lambda r: (r["tier"], r["age"]))
    print("| tier | repo | last push (days) | visibility | lang | note |")
    print("|---|---|---|---|---|---|")
    for r in repos:
        print(f"| {r['tier']} | {r['name']} | {r['age']} | {r['visibility']} | {r['lang']} | {r['note']} |")
    counts = {}
    for r in repos:
        counts[r["tier"]] = counts.get(r["tier"], 0) + 1
    print("\n" + ", ".join(f"{k}: {v}" for k, v in sorted(counts.items())))


if __name__ == "__main__":
    main(sys.argv[1:])
