import os
import re
import math
import time
import json
import glob
import uuid
import subprocess
from fastapi import FastAPI, Response, Path, Query, HTTPException
from pydantic import BaseModel
import redis

# Configuration — all paths and connection params are overridable via env vars
# so the same image works across bare-metal and Docker deployments.
REDIS_HOST = os.environ.get("REDIS_HOST", "localhost")
REDIS_PORT = int(os.environ.get("REDIS_PORT", "6379"))
OBSIDIAN_VAULT_PATH = os.environ.get("OBSIDIAN_VAULT_PATH", "/opt/obsidian-vault/")
MEMORY_BROKER_PORT = int(os.environ.get("MEMORY_BROKER_PORT", "3012"))
ORI_VAULT_PATH = os.environ.get("ORI_VAULT_PATH", "/mnt/pvet630/openclaw/ori-vault/")
ORI_NOTES_PATH = os.path.join(ORI_VAULT_PATH, "notes")
ORI_SELF_PATH = os.path.join(ORI_VAULT_PATH, "self")
ORI_CONFIG_PATH = os.path.join(ORI_VAULT_PATH, "ori.config.yaml")
RESEED_SCRIPT = "/opt/memory-broker/reseed.py"

redis_client = redis.StrictRedis(host=REDIS_HOST, port=REDIS_PORT, db=0, decode_responses=True)
reseed_redis = redis.StrictRedis(host=REDIS_HOST, port=REDIS_PORT, db=0)

app = FastAPI(
    title="Memory Broker Service",
    description="Unified interface for agent memory operations with Redis, Obsidian sync, and Ori vault tiered storage.",
    version="2.0.0",
)

# ---------------------------------------------------------------------------
# Helpers — Obsidian sync (existing)
# ---------------------------------------------------------------------------

def _key_to_path(key: str) -> str:
    relative_path = key.replace(":", "/")
    return os.path.join(OBSIDIAN_VAULT_PATH, f"{relative_path}.md")


def _save_to_obsidian(key: str, content: str):
    file_path = _key_to_path(key)
    os.makedirs(os.path.dirname(file_path), exist_ok=True)
    with open(file_path, "w", encoding="utf-8") as f:
        f.write(content)


# ---------------------------------------------------------------------------
# Helpers — Ori vault (working tier)
# ---------------------------------------------------------------------------

def _key_to_ori_filename(key: str) -> str:
    """Convert a memory key to a safe filename for the Ori vault."""
    return key.replace(":", "_").replace("/", "_") + ".md"


def _save_to_ori_vault(
    key: str,
    content: str,
    tags: list[str] | None = None,
    source: str | None = None,
):
    """Write a markdown note to the Ori vault notes directory."""
    os.makedirs(ORI_NOTES_PATH, exist_ok=True)
    filename = _key_to_ori_filename(key)
    filepath = os.path.join(ORI_NOTES_PATH, filename)
    # Build frontmatter — `updated_at` is rewritten on every save (so it's
    # actually a last-write timestamp, used by /search and /get to choose
    # between tier copies on dedup).
    now = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
    frontmatter_lines = ["---", f"key: {key}", f"updated_at: {now}"]
    if tags:
        frontmatter_lines.append(f"tags: [{', '.join(tags)}]")
    if source:
        frontmatter_lines.append(f"source: {source}")
    frontmatter_lines.append("---")
    full_content = "\n".join(frontmatter_lines) + "\n\n" + content
    try:
        with open(filepath, "w", encoding="utf-8") as f:
            f.write(full_content)
    except Exception as e:
        print(f"Error writing to Ori vault for key '{key}': {e}")
        raise


def _parse_ori_frontmatter(raw: str) -> tuple[dict, str]:
    """Split an Ori vault file into (metadata, body).

    Frontmatter is YAML-ish but we parse it line-by-line to avoid pulling
    in PyYAML and to tolerate minor formatting drift.
    """
    if not raw.startswith("---"):
        return {}, raw
    parts = raw.split("---", 2)
    if len(parts) < 3:
        return {}, raw
    meta: dict = {}
    for line in parts[1].splitlines():
        line = line.strip()
        if not line or ":" not in line:
            continue
        k, _, v = line.partition(":")
        meta[k.strip()] = v.strip()
    # Older notes were written with `created:` instead of `updated_at:`.
    # Normalise so all callers see a consistent field name.
    if "updated_at" not in meta and "created" in meta:
        meta["updated_at"] = meta["created"]
    return meta, parts[2].strip()


def _read_from_ori_vault(key: str) -> str | None:
    """Read content from Ori vault by key. Returns None if not found."""
    filename = _key_to_ori_filename(key)
    filepath = os.path.join(ORI_NOTES_PATH, filename)
    if not os.path.exists(filepath):
        return None
    try:
        with open(filepath, "r", encoding="utf-8") as f:
            raw = f.read()
        _, body = _parse_ori_frontmatter(raw)
        return body
    except Exception:
        return None


def _read_ori_metadata(key: str) -> dict | None:
    """Return the parsed frontmatter dict for an Ori vault entry, or None."""
    filename = _key_to_ori_filename(key)
    filepath = os.path.join(ORI_NOTES_PATH, filename)
    if not os.path.exists(filepath):
        return None
    try:
        with open(filepath, "r", encoding="utf-8") as f:
            raw = f.read()
        meta, _ = _parse_ori_frontmatter(raw)
        return meta
    except Exception:
        return None


def _search_ori_vault(query: str, limit: int = 10) -> list[dict]:
    """Simple keyword search across Ori vault markdown files."""
    query_tokens = set(tokenize(query))
    if not query_tokens:
        return []
    results = []
    search_dirs = [ORI_NOTES_PATH, ORI_SELF_PATH]
    for search_dir in search_dirs:
        if not os.path.isdir(search_dir):
            continue
        for filepath in glob.glob(os.path.join(search_dir, "*.md")):
            try:
                with open(filepath, "r", encoding="utf-8") as f:
                    content = f.read()
            except Exception:
                continue
            content_tokens = set(tokenize(content))
            matched = query_tokens & content_tokens
            if not matched:
                continue
            score = len(matched) / len(query_tokens)
            meta, display_content = _parse_ori_frontmatter(content)
            # Key MUST come from frontmatter — filename stems collapse
            # underscores to colons which corrupts compound Hermes keys.
            key = meta.get("key")
            if not key:
                continue  # skip files with missing/corrupt frontmatter
            results.append({
                "key": key,
                "score": round(score, 4),
                "content": display_content,
                "tier": "working",
                "updated_at": meta.get("updated_at"),
            })
    results.sort(key=lambda x: x["score"], reverse=True)
    return results[:limit]


# ---------------------------------------------------------------------------
# Helpers — Tier classification
# ---------------------------------------------------------------------------

LONGTERM_PREFIXES = ("doc:", "knowledge:", "session:")
WORKING_PREFIXES = ("memory:",)


def _classify_tier(key: str, tier: str) -> str:
    """Resolve 'auto' tier to 'working' or 'longterm' based on key prefix."""
    if tier == "auto":
        if key.startswith(WORKING_PREFIXES):
            return "working"
        if key.startswith(LONGTERM_PREFIXES):
            return "longterm"
        return "longterm"  # default
    return tier


# ---------------------------------------------------------------------------
# Helpers — Agent session namespacing
# ---------------------------------------------------------------------------

def _make_session_id(agent_name: str) -> str:
    ts = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    return f"{agent_name}-{ts}-{uuid.uuid4().hex[:8]}"


def _agent_session_key(agent_name: str, session_id: str, key: str) -> str:
    return f"memory:agent:{agent_name}:{session_id}:{key}"


def _agent_persistent_key(agent_name: str, key: str) -> str:
    return f"doc:agent:{agent_name}:{key}"


# ---------------------------------------------------------------------------
# Helpers — Graph reseed trigger
# ---------------------------------------------------------------------------

def _trigger_reseed():
    """Non-blocking trigger of graph reseed via Redis pub/sub."""
    try:
        reseed_redis.publish('__rmh_reseed__', 'reseed')
    except Exception:
        pass  # non-fatal


# ---------------------------------------------------------------------------
# BM25 search index (existing, unchanged)
# ---------------------------------------------------------------------------

inverted_index = {}
document_lengths = {}
key_tokens = {}  # per-key unique-token set, so re-index can subtract old contribution cleanly
avg_doc_length = 0
num_documents = 0
search_cache = {}
SEARCH_CACHE_TTL = 60
SEARCH_CACHE_MAX = 500


def tokenize(text: str) -> list[str]:
    return re.findall(r'\b\w+\b', text.lower())


def update_index(key: str, content: str):
    """(Re-)index a document for BM25.

    On re-index of an existing key, the prior contribution is removed first;
    otherwise inverted_index[token][key] would accumulate across writes and
    BM25 frequencies would inflate monotonically.
    """
    global avg_doc_length, num_documents
    if key in key_tokens:
        for token in key_tokens[key]:
            posting = inverted_index.get(token)
            if posting and key in posting:
                del posting[key]
                if not posting:
                    del inverted_index[token]
    tokens = tokenize(content)
    key_tokens[key] = set(tokens)
    document_lengths[key] = len(tokens)
    for token in tokens:
        if token not in inverted_index:
            inverted_index[token] = {}
        inverted_index[token][key] = inverted_index[token].get(key, 0) + 1
    num_documents = len(document_lengths)
    avg_doc_length = sum(document_lengths.values()) / num_documents if num_documents > 0 else 0


def remove_from_index(key: str):
    """Drop a key from the BM25 index entirely."""
    global avg_doc_length, num_documents
    if key not in key_tokens:
        return
    for token in key_tokens[key]:
        posting = inverted_index.get(token)
        if posting and key in posting:
            del posting[key]
            if not posting:
                del inverted_index[token]
    del key_tokens[key]
    document_lengths.pop(key, None)
    num_documents = len(document_lengths)
    avg_doc_length = sum(document_lengths.values()) / num_documents if num_documents > 0 else 0


def bm25_score(query_tokens: list[str], doc_key: str, k1=1.5, b=0.75) -> float:
    score = 0.0
    doc_len = document_lengths.get(doc_key, 0)
    if num_documents == 0 or doc_len == 0:
        return 0.0
    for token in query_tokens:
        if token in inverted_index and doc_key in inverted_index[token]:
            freq = inverted_index[token][doc_key]
            n_q = len(inverted_index[token])
            idf = math.log((num_documents - n_q + 0.5) / (n_q + 0.5) + 1)
            numerator = freq * (k1 + 1)
            denominator = freq + k1 * (1 - b + b * (doc_len / avg_doc_length))
            score += idf * (numerator / denominator)
    return score


def _invalidate_search_cache():
    """Drop all cached search results — call after any /store or /promote."""
    search_cache.clear()


def search_documents(query: str, limit: int = 10) -> list[dict]:
    cache_key = f"{query}-{limit}"
    cached = search_cache.get(cache_key)
    if cached and time.time() - cached["timestamp"] < SEARCH_CACHE_TTL:
        # Cache stores (key, score) only — re-fetch content + meta from Redis
        # so callers get current content and a valid updated_at for dedup.
        results = []
        for entry in cached["entries"]:
            content = redis_client.get(entry["key"])
            if content is not None:
                meta = redis_client.hgetall(f"meta:{entry['key']}") or {}
                results.append({
                    "key": entry["key"],
                    "score": entry["score"],
                    "content": content,
                    "tier": "longterm",
                    "updated_at": meta.get("updated_at"),
                })
        return results
    query_tokens = tokenize(query)
    scores = {}
    seen_content = {}  # key → content, captured during indexing pass to avoid double-fetch
    _META_PREFIXES = ("meta:", "tags:", "namespaces:")
    for key in redis_client.scan_iter(match="*", count=500):
        # Skip internal metadata keys — they are hashes/sets, not content.
        if key.startswith(_META_PREFIXES):
            continue
        if key not in document_lengths and redis_client.type(key) == 'string':
            content = redis_client.get(key)
            if content:
                update_index(key, content)
                seen_content[key] = content
    for doc_key in document_lengths.keys():
        scores[doc_key] = bm25_score(query_tokens, doc_key) if query_tokens else 0.0
    sorted_entries = sorted(
        [(key, score) for key, score in scores.items() if score > 0],
        key=lambda x: x[1],
        reverse=True,
    )[:limit]
    results = []
    for key, score in sorted_entries:
        content = seen_content.get(key)
        if content is None:
            content = redis_client.get(key)
        if content is None:
            continue  # key was deleted between index and read
        meta = redis_client.hgetall(f"meta:{key}") or {}
        results.append({
            "key": key,
            "score": score,
            "content": content,
            "tier": "longterm",
            "updated_at": meta.get("updated_at"),
        })
    # Cache only (key, score) — never freeze content
    if len(search_cache) >= SEARCH_CACHE_MAX:
        # Bounded eviction: drop oldest entry by timestamp
        oldest = min(search_cache.keys(), key=lambda k: search_cache[k]["timestamp"])
        del search_cache[oldest]
    search_cache[cache_key] = {
        "entries": [{"key": k, "score": s} for k, s in sorted_entries],
        "timestamp": time.time(),
    }
    return results


# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------

class StoreRequest(BaseModel):
    key: str
    content: str
    tags: list[str] = []
    namespace: str | None = None
    tier: str = "auto"  # "working" | "longterm" | "auto" | "both"
    source: str | None = None  # provenance — written to meta:{key} hash


class SessionStartRequest(BaseModel):
    agent_name: str
    metadata: dict = {}


class SessionEndRequest(BaseModel):
    session_id: str
    summary: str | None = None


class AgentStoreRequest(BaseModel):
    agent_name: str
    session_id: str | None = None
    key: str
    content: str
    tier: str = "auto"
    tags: list[str] = []
    persist: bool = False  # also write to doc:agent:{name}:{key} longterm tier


# ---------------------------------------------------------------------------
# Startup
# ---------------------------------------------------------------------------

@app.on_event("startup")
async def startup_event():
    os.makedirs(OBSIDIAN_VAULT_PATH, exist_ok=True)
    # Seed ori-vault/self/ files into Redis so they are searchable via broker
    if os.path.isdir(ORI_SELF_PATH):
        for fname in os.listdir(ORI_SELF_PATH):
            if fname.endswith(".md"):
                fpath = os.path.join(ORI_SELF_PATH, fname)
                rkey = "self:" + fname.replace(".md", "")
                try:
                    with open(fpath) as fh:
                        file_content = fh.read()
                    if not redis_client.exists(rkey):
                        redis_client.set(rkey, file_content)
                        update_index(rkey, file_content)
                        print(f"Seeded {rkey} from ori-vault/self/")
                except Exception as e:
                    print(f"Failed to seed {fname}: {e}")
    # Index existing Redis keys (use SCAN, not KEYS, to avoid blocking the
    # Redis server on large keyspaces)
    for key in redis_client.scan_iter(match="*", count=500):
        if redis_client.type(key) == 'string':
            content = redis_client.get(key)
            if content:
                update_index(key, content)


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@app.get("/health")
async def health_check():
    """Enhanced health check — reports status of all backends."""
    health = {"status": "ok", "backends": {}}

    # Redis (single ping, reused for both new + legacy fields)
    redis_ok = False
    try:
        redis_client.ping()
        redis_ok = True
        health["backends"]["redis"] = {"status": "connected"}
    except Exception as e:
        health["backends"]["redis"] = {"status": "error", "detail": str(e)}
        health["status"] = "degraded"

    # Ori vault
    if os.path.exists(ORI_CONFIG_PATH):
        health["backends"]["ori_vault"] = {"status": "connected", "path": ORI_VAULT_PATH}
    else:
        health["backends"]["ori_vault"] = {"status": "unavailable", "detail": "ori.config.yaml not found"}

    # Obsidian vault
    if os.path.isdir(OBSIDIAN_VAULT_PATH):
        health["backends"]["obsidian"] = {"status": "connected", "path": OBSIDIAN_VAULT_PATH}
    else:
        health["backends"]["obsidian"] = {"status": "unavailable"}

    # Legacy top-level field (preserved for older clients)
    health["redis"] = "connected" if redis_ok else "disconnected"

    return health


@app.post("/store")
async def store_memory(request: StoreRequest):
    """Store memory with tier routing."""
    resolved_tier = _classify_tier(request.key, request.tier)

    # Force longterm for doc:/knowledge: prefixes regardless of explicit tier
    if request.key.startswith(LONGTERM_PREFIXES) and resolved_tier == "working":
        resolved_tier = "both"

    wrote_to = []

    now = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())

    try:
        if resolved_tier in ("longterm", "both"):
            # Write to Redis (existing behavior)
            redis_client.set(request.key, request.content)
            if request.tags:
                redis_client.sadd(f"tags:{request.key}", *request.tags)
            if request.namespace:
                redis_client.sadd(f"namespaces:{request.namespace}", request.key)
            # Provenance + last-write timestamp for tier-merge dedup
            meta = {"updated_at": now}
            if request.source:
                meta["source"] = request.source
            redis_client.hset(f"meta:{request.key}", mapping=meta)
            _save_to_obsidian(request.key, request.content)
            update_index(request.key, request.content)
            wrote_to.append("longterm")

        if resolved_tier in ("working", "both"):
            # Write to Ori vault
            _save_to_ori_vault(
                request.key,
                request.content,
                request.tags or None,
                source=request.source,
            )
            wrote_to.append("working")

        # Cache holds (key, score) but search_documents merges with live
        # content from Redis on read. We still drop the cache so newly
        # written keys can appear (or disappear) in subsequent results.
        _invalidate_search_cache()

        # Trigger graph reseed (non-blocking)
        _trigger_reseed()

        return {
            "status": "success",
            "key": request.key,
            "tier": resolved_tier,
            "wrote_to": wrote_to,
        }
    except Exception as e:
        print(f"[/store] key={request.key!r} error: {e}")
        raise HTTPException(status_code=500, detail="Failed to store memory")


@app.post("/promote")
async def promote_memory(key: str = Query(...), new_key: str | None = Query(None)):
    """Promote a working memory entry to long-term (Redis KB)."""
    # Try to read from Ori vault
    content = _read_from_ori_vault(key)
    if content is None:
        raise HTTPException(status_code=404, detail=f"Key '{key}' not found in working tier (Ori vault)")

    target_key = new_key or key

    try:
        now = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
        # Write to Redis (longterm)
        redis_client.set(target_key, content)
        redis_client.hset(f"meta:{target_key}", mapping={
            "updated_at": now,
            "source": f"promoted_from:{key}",
        })
        _save_to_obsidian(target_key, content)
        update_index(target_key, content)

        # Tag the original as promoted
        redis_client.sadd(f"tags:{target_key}", "promoted_from_working")

        # Also update the Ori vault note with a promoted tag
        _save_to_ori_vault(key, content, tags=["promoted_to_longterm"])

        _invalidate_search_cache()
        _trigger_reseed()

        return {
            "status": "promoted",
            "from_key": key,
            "to_key": target_key,
            "tier": "longterm",
        }
    except Exception as e:
        print(f"[/promote] key={key!r} error: {e}")
        raise HTTPException(status_code=500, detail="Failed to promote memory")


@app.get("/get")
async def get_memory(key: str = Query(...), tier: str = Query("all")):
    """Get memory by key.

    tier="longterm" → Redis only.
    tier="working"  → Ori vault only.
    tier="all"      → both checked; if a key exists in both, the most recently
                      written copy is returned (compared via meta:{key}.updated_at
                      vs. ori-vault frontmatter updated_at). Falls back to
                      working if timestamps are unavailable on either side, since
                      the working copy is the editable surface post-promote.
    """
    longterm_entry = None
    working_entry = None

    if tier in ("longterm", "all"):
        content = redis_client.get(key)
        if content is not None:
            tags = redis_client.smembers(f"tags:{key}")
            meta = redis_client.hgetall(f"meta:{key}") or {}
            longterm_entry = {
                "key": key,
                "content": content,
                "tags": list(tags),
                "source": meta.get("source"),
                "updated_at": meta.get("updated_at"),
                "tier": "longterm",
            }

    if tier in ("working", "all"):
        content = _read_from_ori_vault(key)
        if content is not None:
            meta = _read_ori_metadata(key) or {}
            working_entry = {
                "key": key,
                "content": content,
                "tags": [],
                "source": meta.get("source"),
                "updated_at": meta.get("updated_at"),
                "tier": "working",
            }

    if longterm_entry and working_entry:
        # Both exist — pick the newer. ISO-8601 strings sort lexicographically.
        lt_ts = longterm_entry.get("updated_at") or ""
        wk_ts = working_entry.get("updated_at") or ""
        return longterm_entry if lt_ts > wk_ts else working_entry
    if longterm_entry:
        return longterm_entry
    if working_entry:
        return working_entry

    raise HTTPException(status_code=404, detail="Memory not found")


@app.get("/keys")
async def list_keys(prefix: str = Query("")):
    if not prefix:
        return []
    keys = list(redis_client.scan_iter(match=f"{prefix}*", count=500))
    return sorted(keys)


@app.get("/search")
async def search_memory(
    q: str = Query(...),
    limit: int = Query(10, ge=1, le=100),
    tier: str = Query("all"),
):
    """Search memory across tiers."""
    if not q:
        return []

    results = []

    if tier in ("longterm", "all"):
        redis_results = search_documents(q, limit)
        results.extend(redis_results)

    if tier in ("working", "all"):
        ori_results = _search_ori_vault(q, limit)
        results.extend(ori_results)

    # Tier-aware dedup: a key may legitimately appear in both tiers (after
    # /promote). BM25 scores (~1-8) and Ori-vault scores (0-1) are NOT on
    # the same scale, so a raw global sort would always rank longterm over
    # working and the older copy could mask a newer post-promote vault edit.
    # Strategy: collapse (key, tier) duplicates first; if a key appears in
    # both tiers, keep ONLY the newer one by updated_at.
    by_key_tier: dict[tuple[str, str], dict] = {}
    for r in results:
        composite = (r["key"], r.get("tier", "?"))
        existing = by_key_tier.get(composite)
        if existing is None or r.get("score", 0) > existing.get("score", 0):
            by_key_tier[composite] = r

    by_key: dict[str, dict] = {}
    for (key, _tier), entry in by_key_tier.items():
        prior = by_key.get(key)
        if prior is None:
            by_key[key] = entry
            continue
        # Cross-tier collision — pick the newer copy. Falls back to score
        # comparison only if neither side has a usable timestamp.
        prior_ts = prior.get("updated_at") or ""
        entry_ts = entry.get("updated_at") or ""
        if entry_ts and prior_ts:
            by_key[key] = entry if entry_ts > prior_ts else prior
        elif entry_ts and not prior_ts:
            by_key[key] = entry
        # else: keep prior (either both empty, or only prior has a timestamp)

    merged = sorted(by_key.values(), key=lambda x: x.get("score", 0), reverse=True)
    return merged[:limit]


# ---------------------------------------------------------------------------
# Agent session endpoints
# ---------------------------------------------------------------------------

@app.post("/session/start")
async def session_start(request: SessionStartRequest):
    """Create a new agent session and return its session_id."""
    session_id = _make_session_id(request.agent_name)
    now = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
    meta = {
        "agent_name": request.agent_name,
        "session_id": session_id,
        "started_at": now,
        "status": "active",
        **request.metadata,
    }
    redis_client.set(f"session:{session_id}", json.dumps(meta))
    redis_client.sadd(f"agent:{request.agent_name}:sessions", session_id)
    return {"session_id": session_id, "started_at": now}


@app.post("/session/end")
async def session_end(request: SessionEndRequest):
    """Mark a session as ended. Optionally attach a summary."""
    raw = redis_client.get(f"session:{request.session_id}")
    if raw is None:
        raise HTTPException(status_code=404, detail=f"Session '{request.session_id}' not found")
    meta = json.loads(raw)
    now = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
    meta["ended_at"] = now
    meta["status"] = "ended"
    if request.summary:
        meta["summary"] = request.summary
    redis_client.set(f"session:{request.session_id}", json.dumps(meta))
    return {"status": "ended", "session_id": request.session_id, "ended_at": now}


@app.get("/session/{session_id}")
async def session_get(session_id: str = Path(...)):
    """Return session metadata."""
    raw = redis_client.get(f"session:{session_id}")
    if raw is None:
        raise HTTPException(status_code=404, detail=f"Session '{session_id}' not found")
    return json.loads(raw)


@app.get("/session/{session_id}/context")
async def session_context(session_id: str = Path(...)):
    """Return all memory stored under this session (working + persistent longterm)."""
    raw = redis_client.get(f"session:{session_id}")
    if raw is None:
        raise HTTPException(status_code=404, detail=f"Session '{session_id}' not found")
    meta = json.loads(raw)
    agent_name = meta.get("agent_name", "")
    prefix = f"memory:agent:{agent_name}:{session_id}:"
    working_keys = []
    if os.path.isdir(ORI_NOTES_PATH):
        for fname in os.listdir(ORI_NOTES_PATH):
            if not fname.endswith(".md"):
                continue
            fpath = os.path.join(ORI_NOTES_PATH, fname)
            try:
                with open(fpath, "r", encoding="utf-8") as f:
                    raw_content = f.read()
                file_meta, body = _parse_ori_frontmatter(raw_content)
                key = file_meta.get("key", "")
                if key.startswith(prefix):
                    working_keys.append({
                        "key": key,
                        "content": body,
                        "tier": "working",
                        "updated_at": file_meta.get("updated_at"),
                    })
            except Exception:
                continue
    longterm_prefix = f"doc:agent:{agent_name}:"
    longterm_keys = []
    for k in redis_client.scan_iter(match=f"{longterm_prefix}*", count=500):
        content = redis_client.get(k)
        if content is not None:
            longterm_keys.append({"key": k, "content": content, "tier": "longterm"})
    return {
        "session_id": session_id,
        "agent_name": agent_name,
        "session_meta": meta,
        "working": working_keys,
        "longterm": longterm_keys,
    }


@app.get("/agent/{name}/sessions")
async def agent_sessions(name: str = Path(...)):
    """List all sessions for a named agent, newest first."""
    sessions_set = redis_client.smembers(f"agent:{name}:sessions")
    sessions = []
    for sid in sessions_set:
        raw = redis_client.get(f"session:{sid}")
        if raw:
            sessions.append(json.loads(raw))
    sessions.sort(key=lambda s: s.get("started_at", ""), reverse=True)
    return {"agent_name": name, "sessions": sessions}


@app.get("/agent/{name}/recall")
async def agent_recall(
    name: str = Path(...),
    session_id: str | None = Query(None),
):
    """Return full context for the most recent (or specified) session — for session restore."""
    if session_id:
        target_session_id = session_id
    else:
        sessions_set = redis_client.smembers(f"agent:{name}:sessions")
        if not sessions_set:
            return {"agent_name": name, "session_id": None, "working": [], "longterm": []}
        best_sid, best_ts = None, ""
        for sid in sessions_set:
            raw = redis_client.get(f"session:{sid}")
            if raw:
                ts = json.loads(raw).get("started_at", "")
                if ts > best_ts:
                    best_ts, best_sid = ts, sid
        if not best_sid:
            return {"agent_name": name, "session_id": None, "working": [], "longterm": []}
        target_session_id = best_sid
    return await session_context(target_session_id)


@app.post("/agent/store")
async def agent_store(request: AgentStoreRequest):
    """Store memory with automatic agent+session namespacing.

    - session_id provided → writes to working tier as memory:agent:{name}:{session}:{key}
    - persist=True → also writes to longterm as doc:agent:{name}:{key}
    """
    session_result = None
    if request.session_id:
        namespaced_key = _agent_session_key(request.agent_name, request.session_id, request.key)
        session_result = await store_memory(StoreRequest(
            key=namespaced_key,
            content=request.content,
            tags=request.tags,
            tier="working",
            source=f"agent:{request.agent_name}",
        ))

    persist_result = None
    if request.persist:
        persistent_key = _agent_persistent_key(request.agent_name, request.key)
        persist_result = await store_memory(StoreRequest(
            key=persistent_key,
            content=request.content,
            tags=request.tags,
            tier="longterm",
            source=f"agent:{request.agent_name}",
        ))

    return {
        "status": "stored",
        "agent_name": request.agent_name,
        "session_id": request.session_id,
        "session_key": _agent_session_key(request.agent_name, request.session_id, request.key) if request.session_id else None,
        "persistent_key": _agent_persistent_key(request.agent_name, request.key) if request.persist else None,
        "session_write": session_result,
        "persist_write": persist_result,
    }


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=MEMORY_BROKER_PORT)
