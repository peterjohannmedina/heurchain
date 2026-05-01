#!/usr/bin/env python3
"""
HeurChain Consolidation Worker

Scans the Ori vault (working tier) for notes that have aged past a threshold,
calls a compressor LLM to generate a discriminative cue, promotes the full
content to long-term (Redis), and leaves the cue in working memory.

Designed to run as a nightly cron job on CT 203 (mcp-test201).

Selected compressor: llama3.2:3b @ ClawBaby Ollama
See MODEL_BENCHMARK.md for the full model selection rationale.
"""

import os
import re
import sys
import time
import json
import glob
from datetime import datetime, timedelta, timezone
from pathlib import Path

import httpx

# ---------------------------------------------------------------------------
# Configuration — override via env vars
# ---------------------------------------------------------------------------

OLLAMA_URL = os.environ.get("OLLAMA_URL", "http://192.168.1.242:11434")
MODEL = os.environ.get("COMPRESSOR_MODEL", "llama3.2:3b")
BROKER_URL = os.environ.get("BROKER_URL", "http://localhost:3012")

ORI_VAULT_PATH = os.environ.get("ORI_VAULT_PATH", "/mnt/pvet630/openclaw/ori-vault/")
ORI_NOTES_PATH = os.path.join(ORI_VAULT_PATH, "notes")

# Notes older than this are eligible for consolidation
AGE_DAYS = int(os.environ.get("CONSOLIDATE_AGE_DAYS", "7"))

# Ollama generation options
TEMPERATURE = float(os.environ.get("COMPRESSOR_TEMP", "0.1"))
NUM_PREDICT = int(os.environ.get("COMPRESSOR_MAX_TOKENS", "20"))
OLLAMA_TIMEOUT = int(os.environ.get("OLLAMA_TIMEOUT", "120"))

# Prompt template for extractive compression
PROMPT_TEMPLATE = """Compress the following note into 5-15 tokens that would uniquely retrieve it.
Prioritize: product codes, proper nouns, specific numbers, technical terms.
Avoid: generic descriptions, function words, obvious categories, framing text.
Output ONLY the compressed tokens, nothing else.

Note:
{content}

Compressed:"""

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

def log(msg: str):
    ts = datetime.now(timezone.utc).isoformat()
    print(f"[{ts}] {msg}", flush=True)


# ---------------------------------------------------------------------------
# Ori vault helpers
# ---------------------------------------------------------------------------

def _key_to_filename(key: str) -> str:
    return key.replace(":", "_").replace("/", "_") + ".md"


def _filename_to_key(filename: str) -> str | None:
    """Convert an Ori vault filename back to a memory key."""
    if not filename.endswith(".md"):
        return None
    stem = filename[:-3]
    # Heuristic: replace underscores with colons for common prefixes
    # This is best-effort; the frontmatter key field is authoritative.
    return stem.replace("_", ":", 1)  # only first underscore -> colon


def _parse_frontmatter(raw: str) -> tuple[dict, str]:
    """Split an Ori vault file into (metadata, body)."""
    if not raw.startswith("---"):
        return {}, raw
    parts = raw.split("---", 2)
    if len(parts) < 3:
        return {}, raw
    meta: dict = {}
    for line in parts[1].splitlines():
        line = line.strip()
        if ":" in line and not line.startswith("#"):
            k, v = line.split(":", 1)
            meta[k.strip()] = v.strip()
    return meta, parts[2].strip()


def _list_eligible_notes() -> list[tuple[str, Path, dict, str]]:
    """Return notes in Ori vault older than AGE_DAYS.

    Each tuple is (key, filepath, metadata, body).
    """
    cutoff = datetime.now(timezone.utc) - timedelta(days=AGE_DAYS)
    eligible: list[tuple[str, Path, dict, str]] = []

    notes_dir = Path(ORI_NOTES_PATH)
    if not notes_dir.exists():
        log(f"Ori notes directory does not exist: {notes_dir}")
        return eligible

    for filepath in notes_dir.glob("*.md"):
        try:
            raw = filepath.read_text(encoding="utf-8")
            meta, body = _parse_frontmatter(raw)
            key = meta.get("key") or _filename_to_key(filepath.name)
            if not key:
                continue

            updated_at_str = meta.get("updated_at", "")
            if updated_at_str:
                try:
                    # ISO format with or without Z
                    updated_at = datetime.fromisoformat(updated_at_str.replace("Z", "+00:00"))
                except ValueError:
                    updated_at = datetime.fromtimestamp(filepath.stat().st_mtime, tz=timezone.utc)
            else:
                updated_at = datetime.fromtimestamp(filepath.stat().st_mtime, tz=timezone.utc)

            if updated_at < cutoff:
                eligible.append((key, filepath, meta, body))
        except Exception as e:
            log(f"Skipping {filepath.name}: {e}")

    # Oldest first
    eligible.sort(key=lambda x: x[2].get("updated_at", ""))
    return eligible


# ---------------------------------------------------------------------------
# Compression
# ---------------------------------------------------------------------------

def compress(content: str) -> str:
    """Call Ollama to compress a note into a discriminative cue."""
    prompt = PROMPT_TEMPLATE.format(content=content[:3000])

    payload = {
        "model": MODEL,
        "prompt": prompt,
        "options": {
            "temperature": TEMPERATURE,
            "num_predict": NUM_PREDICT,
        },
        "stream": False,
    }

    with httpx.Client(timeout=OLLAMA_TIMEOUT) as client:
        resp = client.post(f"{OLLAMA_URL}/api/generate", json=payload)
        resp.raise_for_status()
        data = resp.json()

    cue = data.get("response", "").strip()
    # Defensive: strip common fluff
    cue = re.sub(r'^(Here["\']?s? the compressed version:?\s*)', '', cue, flags=re.IGNORECASE)
    cue = re.sub(r'^(Compressed:?\s*)', '', cue, flags=re.IGNORECASE)
    cue = re.sub(r'["\']$', '', cue)
    cue = cue.strip()
    return cue


# ---------------------------------------------------------------------------
# Broker API
# ---------------------------------------------------------------------------

def promote_to_longterm(key: str) -> bool:
    """Promote a working-tier note to longterm via the broker."""
    try:
        with httpx.Client(timeout=30) as client:
            resp = client.post(f"{BROKER_URL}/promote", params={"key": key})
            if resp.status_code == 200:
                log(f"  Promoted {key} to longterm")
                return True
            elif resp.status_code == 404:
                log(f"  Key {key} not found in working tier")
                return False
            else:
                log(f"  Promote failed for {key}: HTTP {resp.status_code} {resp.text}")
                return False
    except Exception as e:
        log(f"  Promote error for {key}: {e}")
        return False


def store_cue(key: str, cue: str, tags: list[str] | None = None) -> bool:
    """Store the compressed cue back to working memory."""
    payload = {
        "key": key,
        "content": cue,
        "tier": "working",
        "tags": tags or [],
        "source": "consolidation_worker",
    }
    try:
        with httpx.Client(timeout=30) as client:
            resp = client.post(f"{BROKER_URL}/store", json=payload)
            if resp.status_code == 200:
                log(f"  Stored cue for {key}")
                return True
            else:
                log(f"  Store cue failed for {key}: HTTP {resp.status_code} {resp.text}")
                return False
    except Exception as e:
        log(f"  Store cue error for {key}: {e}")
        return False


# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------

def run() -> int:
    log("=" * 60)
    log("HeurChain Consolidation Worker starting")
    log(f"Model: {MODEL} @ {OLLAMA_URL}")
    log(f"Broker: {BROKER_URL}")
    log(f"Age threshold: {AGE_DAYS} days")
    log(f"Ori vault: {ORI_NOTES_PATH}")
    log("=" * 60)

    eligible = _list_eligible_notes()
    log(f"Found {len(eligible)} note(s) eligible for consolidation")

    if not eligible:
        log("Nothing to do. Exiting.")
        return 0

    processed = 0
    failed = 0

    for key, filepath, meta, body in eligible:
        log(f"Processing {key} ...")
        try:
            # 1. Compress
            log(f"  Compressing ({len(body)} chars) ...")
            t0 = time.time()
            cue = compress(body)
            t1 = time.time()
            log(f"  Cue generated in {t1-t0:.1f}s: [{cue}]")

            if not cue:
                log(f"  Empty cue, skipping {key}")
                failed += 1
                continue

            # 2. Promote full content to longterm
            if not promote_to_longterm(key):
                failed += 1
                continue

            # 3. Store cue in working memory
            tags_str = meta.get("tags", "")
            tags = [t.strip() for t in tags_str.strip("[]").split(",") if t.strip()] if tags_str else None
            if not store_cue(key, cue, tags):
                failed += 1
                continue

            # 4. Remove original Ori vault file (now in Redis + cue in Ori)
            filepath.unlink()
            log(f"  Removed original {filepath.name}")

            processed += 1
            log(f"  DONE: {key}")

        except Exception as e:
            log(f"  FAILED: {key} — {e}")
            failed += 1

    log("=" * 60)
    log(f"Completed: {processed} processed, {failed} failed, {len(eligible)} total")
    log("=" * 60)
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(run())
