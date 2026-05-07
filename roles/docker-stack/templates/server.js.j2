const express = require("express");
const { createClient } = require("redis");
const cors = require("cors");
const fs = require("fs").promises;
const path = require("path");
const { McpServer } = require("@modelcontextprotocol/sdk/server/mcp.js");
const { SSEServerTransport } = require("@modelcontextprotocol/sdk/server/sse.js");
const { z } = require("zod");

const app = express();
app.use(cors());

const redis = createClient({ url: process.env.REDIS_URL || "redis://localhost:6379" });
redis.on("error", (err) => console.error("Redis error:", err));

const OBSIDIAN_VAULT_PATH = path.resolve(process.env.OBSIDIAN_VAULT_PATH || "/opt/obsidian-vault");
const VAULT_ROOT = OBSIDIAN_VAULT_PATH + path.sep;

const BUFFER_CAPACITY        = parseInt(process.env.BUFFER_CAPACITY, 10)        || 50;
const BUFFER_FLUSH_MS        = parseInt(process.env.BUFFER_FLUSH_MS, 10)        || 5 * 60 * 1000; // 5 min
const SESSION_CONTEXT_TOKENS = parseInt(process.env.SESSION_CONTEXT_TOKENS, 10) || 400;
const PROC_KEY_RE            = /^[a-zA-Z0-9_\-\.]+$/; // safe proc key segment

// node-redis scanIterator yields pages (arrays) in some versions and individual
// keys in others. scanKeys normalizes to individual key strings in all cases.
async function* scanKeys(pattern, type) {
  const opts = { MATCH: pattern, COUNT: 200 };
  if (type) opts.TYPE = type;
  for await (const item of redis.scanIterator(opts)) {
    if (Array.isArray(item)) { for (const k of item) yield k; }
    else yield item;
  }
}

// ---------------------------------------------------------------------------
// Auth middleware — set MCP_AUTH_TOKEN env var to enforce bearer token auth.
// If unset, access is unrestricted (warn loudly).
// ---------------------------------------------------------------------------

const MCP_AUTH_TOKEN = process.env.MCP_AUTH_TOKEN || "";
if (!MCP_AUTH_TOKEN) {
  console.warn("⚠️  SECURITY WARNING: MCP_AUTH_TOKEN is not set. All endpoints are unauthenticated. Set this env var to a secret token.");
}

function requireAuth(req, res, next) {
  if (!MCP_AUTH_TOKEN) return next(); // auth disabled — allow (warned at startup)
  const auth = req.headers.authorization || "";
  if (!auth.startsWith("Bearer ") || auth.slice(7) !== MCP_AUTH_TOKEN) {
    return res.status(401).json({ error: "Unauthorized" });
  }
  next();
}

// ---------------------------------------------------------------------------
// Key validation & path resolution — #1 path traversal fix
// ---------------------------------------------------------------------------

const SAFE_KEY_SEGMENT = /^[a-zA-Z0-9_\-\.]+$/;

function validateKey(key) {
  if (typeof key !== "string" || key.length === 0 || key.length > 512)
    throw new Error("invalid key: must be a non-empty string ≤ 512 chars");
  if (/[\\/\x00]/.test(key))
    throw new Error("invalid key: contains illegal characters");
  const segments = key.split(":");
  for (const seg of segments) {
    if (seg === "" || seg === "." || seg === "..")
      throw new Error("invalid key: empty or dot segment");
    if (!SAFE_KEY_SEGMENT.test(seg))
      throw new Error(`invalid key: segment '${seg}' contains illegal characters`);
  }
}

function keyToObsidianPath(key) {
  validateKey(key);
  const relative = key.replace(/:/g, path.sep) + ".md";
  const resolved = path.resolve(OBSIDIAN_VAULT_PATH, relative);
  if (resolved !== OBSIDIAN_VAULT_PATH && !resolved.startsWith(VAULT_ROOT))
    throw new Error("invalid key: path escapes vault");
  return resolved;
}

// ---------------------------------------------------------------------------
// BM25 Index — fixes: drop tokens array (#13), O(N²) rebuild (#5),
//              pending-ops queue to survive rebuild (#4), tier validation (#15)
// ---------------------------------------------------------------------------

const STOPWORDS = new Set([
  "a","an","the","and","or","but","in","on","at","to","for","of","with",
  "by","from","as","is","was","are","were","be","been","being","have",
  "has","had","do","does","did","will","would","could","should","may",
  "might","shall","can","not","no","nor","so","yet","both","either",
  "than","then","when","where","which","who","that","this","these","those",
  "it","its","i","me","my","we","our","you","your","he","she","they","them",
  "his","her","their","what","how","all","each","any","more","also","just",
  "if","up","out","about","into","through","over","after","before","between"
]);

const VALID_TIERS = new Set(["all", "longterm", "working"]);

const NOISE_KEY_PATTERNS   = [/session-summary/, /agent-end/, /heartbeat/];
const NOISE_CONTENT_PATTERNS = [
  /^\d+ user turns,\s*\d+ total messages/i,
  /^openclaw session on [a-z0-9-]+:/i,
  /^session ended\./i
];

function isSessionNoise(key, content) {
  if (NOISE_KEY_PATTERNS.some(p => p.test(key))) return true;
  return NOISE_CONTENT_PATTERNS.some(p => p.test((content || "").trim()));
}

class BM25Index {
  constructor(k1 = 1.5, b = 0.75) {
    this.k1 = k1;
    this.b = b;
    // doc entry: { tf: Map<term,count>, dl: number, content: string, tier: string, updatedAt: string|null }
    this.docs = new Map();
    this.df   = new Map(); // term -> document frequency
    this.avgLen = 0;
    this.N = 0;
  }

  tokenize(text) {
    return (text || "")
      .toLowerCase()
      .replace(/[^a-z0-9\s]/g, " ")
      .split(/\s+/)
      .filter(t => t.length > 1 && !STOPWORDS.has(t));
  }

  // Single-doc add — used for incremental updates; calls _recalcStats each time
  add(key, content, tier, updatedAt) {
    if (this.docs.has(key)) this._removeFromIndex(key);
    const tokens = this.tokenize(content);
    const tf = new Map();
    for (const t of tokens) tf.set(t, (tf.get(t) || 0) + 1);
    this.docs.set(key, { tf, dl: tokens.length, content, tier, updatedAt });
    for (const term of tf.keys()) this.df.set(term, (this.df.get(term) || 0) + 1);
    this._recalcStats();
  }

  // Bulk add — O(N log N) instead of O(N²); calls _recalcStats once at end
  bulkAdd(entries) {
    for (const { key, content, tier, updatedAt } of entries) {
      if (this.docs.has(key)) this._removeFromIndex(key);
      const tokens = this.tokenize(content);
      const tf = new Map();
      for (const t of tokens) tf.set(t, (tf.get(t) || 0) + 1);
      this.docs.set(key, { tf, dl: tokens.length, content, tier, updatedAt });
      for (const term of tf.keys()) this.df.set(term, (this.df.get(term) || 0) + 1);
    }
    this._recalcStats();
  }

  remove(key) {
    if (!this.docs.has(key)) return;
    this._removeFromIndex(key);
    this._recalcStats();
  }

  _removeFromIndex(key) {
    const doc = this.docs.get(key);
    if (!doc) return;
    for (const term of doc.tf.keys()) {
      const c = this.df.get(term) || 0;
      if (c <= 1) this.df.delete(term);
      else this.df.set(term, c - 1);
    }
    this.docs.delete(key);
  }

  _recalcStats() {
    this.N = this.docs.size;
    if (this.N === 0) { this.avgLen = 0; return; }
    let total = 0;
    for (const doc of this.docs.values()) total += doc.dl;
    this.avgLen = total / this.N;
  }

  _scoreDoc(queryTokens, docKey) {
    const doc = this.docs.get(docKey);
    if (!doc) return 0;
    const lenNorm = this.avgLen > 0 ? doc.dl / this.avgLen : 1;
    let score = 0;
    for (const term of queryTokens) {
      const tf = doc.tf.get(term) || 0;
      if (tf === 0) continue;
      const df  = this.df.get(term) || 0;
      const idf = Math.log((this.N - df + 0.5) / (df + 0.5) + 1);
      const tfn = (tf * (this.k1 + 1)) / (tf + this.k1 * (1 - this.b + this.b * lenNorm));
      score += idf * tfn;
    }
    return score;
  }

  search(query, { limit = 10, tier = "all", minScore = 0, noiseFilter = true, namespace = null } = {}) {
    const safeTier = VALID_TIERS.has(tier) ? tier : "all";
    const safeLimit = Math.max(1, Math.min(Number.isFinite(limit) ? Math.floor(limit) : 10, 50));
    const queryTokens = this.tokenize(query);
    if (queryTokens.length === 0) return [];
    const results = [];
    for (const [key, doc] of this.docs.entries()) {
      if (safeTier !== "all" && doc.tier !== safeTier) continue;
      if (namespace && !key.startsWith(namespace + ":")) continue;
      if (noiseFilter && isSessionNoise(key, doc.content)) continue;
      const score = this._scoreDoc(queryTokens, key);
      if (score > minScore) results.push({ key, score, tier: doc.tier, content: doc.content, updated_at: doc.updatedAt });
    }
    return results.sort((a, b) => b.score - a.score).slice(0, safeLimit);
  }

  get size() { return this.N; }
}

const bm25 = new BM25Index();

// ---------------------------------------------------------------------------
// Session buffer — ambient turn capture without explicit agent store calls.
// Agents POST to /api/buffer fire-and-forget; buffer auto-flushes to Redis.
// ---------------------------------------------------------------------------

const sessionBuffers = new Map(); // session_id -> [{ role, content, agent, ts }]

async function flushBuffer(sessionId) {
  const buf = sessionBuffers.get(sessionId);
  if (!buf || buf.length === 0) return;
  const key = `session:${sessionId}:buffer`;
  try {
    const existing = await redis.get(key);
    const prev = existing ? JSON.parse(existing) : [];
    await redis.setEx(key, 7 * 24 * 60 * 60, JSON.stringify([...prev, ...buf]));
    sessionBuffers.set(sessionId, []);
  } catch (e) {
    console.error(`Buffer flush error session ${sessionId}:`, e.message);
  }
}

function bufferAppend(sessionId, agentName, role, content) {
  if (!sessionBuffers.has(sessionId)) sessionBuffers.set(sessionId, []);
  const buf = sessionBuffers.get(sessionId);
  buf.push({ role: role || "user", content, agent: agentName || "unknown", ts: new Date().toISOString() });
  if (buf.length >= BUFFER_CAPACITY) flushBuffer(sessionId).catch(() => {});
}

// ---------------------------------------------------------------------------
// Procedural memory — stable agent preferences/style knobs, no TTL/decay.
// Redis namespace: proc:agent:{name}:{key}
// ---------------------------------------------------------------------------

function procKey(agentName, key) {
  if (!PROC_KEY_RE.test(agentName)) throw new Error("invalid agent_name");
  if (!PROC_KEY_RE.test(key))       throw new Error("invalid proc key");
  return `proc:agent:${agentName}:${key}`;
}

async function procGet(agentName) {
  const prefix = `proc:agent:${agentName}:`;
  const result = {};
  for await (const k of scanKeys(`${prefix}*`, "string")) {
    const val = await redis.get(k);
    if (!val) continue;
    const shortKey = k.slice(prefix.length);
    try { result[shortKey] = JSON.parse(val); } catch { result[shortKey] = val; }
  }
  return result;
}

// Pending-ops queue: writes/removes that occur during a rebuild are applied
// after the swap so they aren't silently discarded — fix #4
let isBuilding = false;
const pendingOps = [];

function indexAdd(key, content, tier, updatedAt) {
  bm25.add(key, content, tier, updatedAt); // update live index for immediate search
  if (isBuilding) pendingOps.push({ op: "add", key, content, tier, updatedAt });
}

function indexRemove(key) {
  bm25.remove(key);
  if (isBuilding) pendingOps.push({ op: "remove", key });
}

// ---------------------------------------------------------------------------
// Index build
// ---------------------------------------------------------------------------

async function buildIndex() {
  if (isBuilding) return; // skip if already running
  isBuilding = true;
  pendingOps.length = 0;

  const entries = [];

  // Scan Redis — scanKeys normalizes page/individual yields from scanIterator
  try {
    for await (const key of scanKeys("*", "string")) {
      try {
        const val = await redis.get(key);
        if (val) entries.push({ key, content: val, tier: "longterm", updatedAt: null });
      } catch (e) {
        console.warn(`Index build — skipping key '${key}': ${e.message}`);
      }
    }
  } catch (e) {
    console.error("Index build — Redis scan error:", e.message);
  }

  // Walk Obsidian vault
  async function walk(dir) {
    let items;
    try { items = await fs.readdir(dir, { withFileTypes: true }); }
    catch (e) { if (e.code === "ENOENT") return; throw e; }
    for (const entry of items) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) { await walk(fullPath); continue; }
      if (!entry.name.endsWith(".md")) continue;
      try {
        const content = await fs.readFile(fullPath, "utf-8");
        const stat    = await fs.stat(fullPath);
        const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
        const key = relative.replace(/\//g, ":").replace(/\\/, ":").replace(/\.md$/, "");
        entries.push({ key, content, tier: "working", updatedAt: stat.mtime.toISOString() });
      } catch (e) {
        console.error("Index build — vault read error:", fullPath, e.message);
      }
    }
  }
  await walk(OBSIDIAN_VAULT_PATH);

  // Build fresh index in bulk — O(N log N), no event-loop stall per doc
  const fresh = new BM25Index();
  fresh.bulkAdd(entries);

  // Atomic field swap
  bm25.docs   = fresh.docs;
  bm25.df     = fresh.df;
  bm25.avgLen = fresh.avgLen;
  bm25.N      = fresh.N;

  // Re-apply writes/removes that occurred during the build
  for (const op of pendingOps) {
    if (op.op === "add")    bm25.add(op.key, op.content, op.tier, op.updatedAt);
    else if (op.op === "remove") bm25.remove(op.key);
  }
  pendingOps.length = 0;
  isBuilding = false;

  console.log(`BM25 index built: ${bm25.size} docs (${entries.length} total entries scanned)`);
}

// ---------------------------------------------------------------------------
// Obsidian vault helpers
// ---------------------------------------------------------------------------

async function saveToObsidian(key, content, metadata) {
  const filePath = keyToObsidianPath(key); // throws on bad key
  await fs.mkdir(path.dirname(filePath), { recursive: true });
  let output = content;
  if (metadata && Object.keys(metadata).length > 0) {
    const lines = ["---"];
    for (const [k, v] of Object.entries(metadata))
      lines.push(`${k}: ${Array.isArray(v) ? `[${v.join(", ")}]` : v}`);
    lines.push("---", "");
    output = lines.join("\n") + content;
  }
  await fs.writeFile(filePath, output, "utf-8");
  return filePath;
}

async function readFromObsidian(key) {
  const filePath = keyToObsidianPath(key);
  try { return await fs.readFile(filePath, "utf-8"); }
  catch (e) { if (e.code === "ENOENT") return null; throw e; }
}

async function deleteFromObsidian(key) {
  const filePath = keyToObsidianPath(key);
  try { await fs.unlink(filePath); return true; }
  catch (e) { if (e.code === "ENOENT") return false; throw e; }
}

async function searchObsidian(query, namespace) {
  const results = [];
  async function walk(dir) {
    let items;
    try { items = await fs.readdir(dir, { withFileTypes: true }); }
    catch (e) { if (e.code === "ENOENT") return; throw e; }
    for (const entry of items) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) { await walk(fullPath); continue; }
      if (!entry.name.endsWith(".md")) continue;
      const content = await fs.readFile(fullPath, "utf-8");
      if (!content.toLowerCase().includes(query.toLowerCase())) continue;
      const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
      const key = relative.replace(/[\\/]/g, ":").replace(/\.md$/, "");
      if (namespace && !key.startsWith(namespace + ":")) continue;
      results.push({ key, content, path: relative });
    }
  }
  await walk(OBSIDIAN_VAULT_PATH);
  return results;
}

async function listObsidian(prefix) {
  const results = [];
  async function walk(dir) {
    let items;
    try { items = await fs.readdir(dir, { withFileTypes: true }); }
    catch (e) { if (e.code === "ENOENT") return; throw e; }
    for (const entry of items) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) { await walk(fullPath); continue; }
      if (!entry.name.endsWith(".md")) continue;
      const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
      const key = relative.replace(/[\\/]/g, ":").replace(/\.md$/, "");
      if (!prefix || key.startsWith(prefix)) results.push({ key, path: relative });
    }
  }
  await walk(OBSIDIAN_VAULT_PATH);
  return results;
}

// ---------------------------------------------------------------------------
// MCP server factory
// ---------------------------------------------------------------------------

function createMcpServerInstance(profile = "full") {
  const server = new McpServer({ name: "heurchain-mcp", version: "1.3.0" });

  // --- Cache Tools ---

  server.tool("cache_set", "Store a key-value pair in Redis (longterm tier). Optionally scope to a namespace.", {
    key:       z.string().describe("Cache key"),
    value:     z.string().describe("Value to cache"),
    ttl:       z.number().optional().describe("Time-to-live in seconds"),
    namespace: z.string().optional().describe("Optional namespace prefix, e.g. 'agent-hermes'. Key becomes namespace:key.")
  }, async ({ key, value, ttl, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try { validateKey(fullKey); } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ cached: false, error: e.message }) }] };
    }
    if (ttl) await redis.setEx(fullKey, ttl, value);
    else await redis.set(fullKey, value);
    indexAdd(fullKey, value, "longterm", new Date().toISOString());
    return { content: [{ type: "text", text: JSON.stringify({ cached: true, key: fullKey }) }] };
  });

  server.tool("cache_get", "Retrieve a cached value from Redis by key", {
    key:       z.string().describe("Cache key to retrieve"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    const val = await redis.get(fullKey);
    if (val) return { content: [{ type: "text", text: val }] };
    return { content: [{ type: "text", text: JSON.stringify({ hit: false }) }] };
  });

  server.tool("cache_delete", "Delete a cached key from Redis", {
    key:       z.string().describe("Cache key to delete"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    const deleted = await redis.del(fullKey);
    indexRemove(fullKey);
    return { content: [{ type: "text", text: JSON.stringify({ deleted: deleted > 0, key: fullKey }) }] };
  });

  server.tool("redis_stats", "Get Redis server statistics and memory info", {}, async () => {
    const info = await redis.info("stats");
    return { content: [{ type: "text", text: info }] };
  });

  server.tool("health_check", "Check the health of the MCP server, Redis, and Obsidian vault", {}, async () => {
    const redisOk = redis.isReady;
    let obsidianOk = false;
    try { await fs.access(OBSIDIAN_VAULT_PATH); obsidianOk = true; } catch {}
    return { content: [{ type: "text", text: JSON.stringify({
      status: (redisOk && obsidianOk) ? "healthy" : "degraded",
      redis: redisOk, obsidian: obsidianOk,
      vault_path: OBSIDIAN_VAULT_PATH,
      bm25_docs: bm25.size,
      uptime: process.uptime()
    }) }] };
  });

  // --- Obsidian Vault Tools ---

  server.tool("obsidian_write_note", "Write a markdown note to the Obsidian vault (working tier). Key colons become folder separators.", {
    key:       z.string().describe("Note key (e.g. doc:proxmox:setup)"),
    content:   z.string().describe("Markdown body"),
    metadata:  z.record(z.string()).optional().describe("Optional frontmatter metadata"),
    namespace: z.string().optional().describe("Optional namespace prefix")
  }, async ({ key, content, metadata, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try {
      const filePath = await saveToObsidian(fullKey, content, metadata);
      indexAdd(fullKey, content, "working", new Date().toISOString());
      return { content: [{ type: "text", text: JSON.stringify({ stored: true, key: fullKey, path: filePath, tier: "working" }) }] };
    } catch (e) {
      console.error(`obsidian_write_note error for key '${fullKey}':`, e);
      return { content: [{ type: "text", text: JSON.stringify({ stored: false, key: fullKey, error: e.message.startsWith("invalid key") ? e.message : "write failed" }) }] };
    }
  });

  server.tool("obsidian_read_note", "Read a markdown note from the Obsidian vault by key", {
    key:       z.string().describe("Note key"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try {
      const content = await readFromObsidian(fullKey);
      if (content === null) return { content: [{ type: "text", text: JSON.stringify({ found: false, key: fullKey }) }] };
      return { content: [{ type: "text", text: JSON.stringify({ found: true, key: fullKey, content }) }] };
    } catch (e) {
      console.error(`obsidian_read_note error for key '${fullKey}':`, e);
      return { content: [{ type: "text", text: JSON.stringify({ found: false, key: fullKey, error: e.message.startsWith("invalid key") ? e.message : "read failed" }) }] };
    }
  });

  server.tool("obsidian_delete_note", "Delete a markdown note from the Obsidian vault by key", {
    key:       z.string().describe("Note key to delete"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try {
      const deleted = await deleteFromObsidian(fullKey);
      indexRemove(fullKey);
      return { content: [{ type: "text", text: JSON.stringify({ deleted, key: fullKey }) }] };
    } catch (e) {
      console.error(`obsidian_delete_note error for key '${fullKey}':`, e);
      return { content: [{ type: "text", text: JSON.stringify({ deleted: false, key: fullKey, error: e.message.startsWith("invalid key") ? e.message : "delete failed" }) }] };
    }
  });

  server.tool("obsidian_search_notes", "Search markdown notes in the Obsidian vault by keyword (substring, vault only). Prefer heurchain_search for ranked results across all tiers.", {
    query:     z.string().describe("Search term"),
    limit:     z.number().optional().describe("Max results (1–100, default 10)").default(10),
    namespace: z.string().optional().describe("Restrict to a namespace prefix")
  }, async ({ query, limit, namespace }) => {
    const safeLimit = Math.max(1, Math.min(Number.isFinite(limit) ? Math.floor(limit) : 10, 100));
    try {
      const results = await searchObsidian(query, namespace || null);
      return { content: [{ type: "text", text: JSON.stringify({
        count: results.length,
        results: results.slice(0, safeLimit).map(r => ({ key: r.key, preview: r.content.substring(0, 200) }))
      }) }] };
    } catch (e) {
      console.error("obsidian_search_notes error:", e);
      return { content: [{ type: "text", text: JSON.stringify({ error: "search failed" }) }] };
    }
  });

  server.tool("obsidian_list_notes", "List all markdown notes in the Obsidian vault, optionally filtered by prefix", {
    prefix: z.string().optional().describe("Key prefix filter, e.g. doc:proxmox:")
  }, async ({ prefix }) => {
    try {
      const results = await listObsidian(prefix);
      return { content: [{ type: "text", text: JSON.stringify({ count: results.length, notes: results }) }] };
    } catch (e) {
      console.error("obsidian_list_notes error:", e);
      return { content: [{ type: "text", text: JSON.stringify({ error: "list failed" }) }] };
    }
  });

  // --- HeurChain BM25 Search ---

  server.tool("heurchain_search",
    "Search the full knowledge base using HeurChain's native BM25 ranked index. Covers all Redis keys (longterm) and Obsidian vault files (working). Preferred over obsidian_search_notes.", {
    query:       z.string().describe("Search query"),
    limit:       z.number().optional().describe("Max results (default 10, max 50)").default(10),
    tier:        z.string().optional().describe("'all' | 'longterm' | 'working' (default: all)").default("all"),
    minScore:    z.number().optional().describe("Minimum BM25 score (default 0; try 0.5 to reduce noise)").default(0),
    noiseFilter: z.boolean().optional().describe("Skip low-signal session metadata (default true)").default(true),
    namespace:   z.string().optional().describe("Restrict to a namespace, e.g. 'agent-hermes'")
  }, async ({ query, limit, tier, minScore, noiseFilter, namespace }) => {
    try {
      const results = bm25.search(query, {
        limit: Math.min(limit, 50),
        tier,
        minScore,
        noiseFilter,
        namespace: namespace || null
      });
      return { content: [{ type: "text", text: JSON.stringify({
        count: results.length,
        index_size: bm25.size,
        results: results.map(r => ({
          key: r.key,
          score: Math.round(r.score * 1000) / 1000,
          tier: r.tier,
          preview: (r.content || "").substring(0, 300),
          updated_at: r.updated_at
        }))
      }) }] };
    } catch (e) {
      console.error("heurchain_search error:", e);
      return { content: [{ type: "text", text: JSON.stringify({ error: "search failed" }) }] };
    }
  });

  // --- heurchain_export (Glacier Sync) ---

  server.tool("heurchain_export",
    "Export Redis longterm-tier entries to Obsidian vault markdown files. Makes the Redis knowledge base human-readable. Idempotent — safe to run repeatedly.", {
    prefix:    z.string().optional().describe("Only export keys matching this prefix (e.g. 'doc:'). Defaults to all keys."),
    overwrite: z.boolean().optional().describe("Overwrite existing vault files (default false — skip if file exists)").default(false)
  }, async ({ prefix, overwrite }) => {
    let exported = 0, skipped = 0, errors = 0;
    try {
      for await (const key of scanKeys(prefix ? `${prefix}*` : "*", "string")) {
        // skip keys that would fail path validation
        try { validateKey(key); } catch { skipped++; continue; }
        const val = await redis.get(key);
        if (!val) continue;
        const filePath = keyToObsidianPath(key);
        if (!overwrite) {
          try { await fs.access(filePath); skipped++; continue; } catch {}
        }
        try {
          await fs.mkdir(path.dirname(filePath), { recursive: true });
          // avoid double-frontmatter if the Redis value already starts with ---
          const hasFrontmatter = val.trimStart().startsWith("---");
          const header = hasFrontmatter
            ? `<!-- exported: key=${key} at ${new Date().toISOString()} -->\n\n`
            : `---\nkey: ${key}\nexported_at: ${new Date().toISOString()}\nsource: redis-longterm\n---\n\n`;
          await fs.writeFile(filePath, header + val, "utf-8");
          indexAdd(key, val, "longterm", new Date().toISOString());
          exported++;
        } catch (e) {
          console.error(`heurchain_export: failed to write '${key}':`, e.message);
          errors++;
        }
      }
      return { content: [{ type: "text", text: JSON.stringify({ exported, skipped, errors, vault_path: OBSIDIAN_VAULT_PATH }) }] };
    } catch (e) {
      console.error("heurchain_export error:", e);
      return { content: [{ type: "text", text: JSON.stringify({ error: "export failed", exported, skipped, errors }) }] };
    }
  });

  // --- Procedural Memory Tools ---

  server.tool("proc_set",
    "Store a stable agent preference or style knob in procedural memory. These persist indefinitely across sessions — no TTL, no decay. Use for learned behaviors, coding style, user preferences.", {
    agent_name: z.string().describe("Agent identifier (e.g. 'claude-code', 'hermes')"),
    key:        z.string().describe("Preference name (alphanumeric, hyphens, underscores — e.g. 'coding_style')"),
    value:      z.string().describe("Preference value")
  }, async ({ agent_name, key, value }) => {
    try {
      const rk = procKey(agent_name, key);
      await redis.set(rk, value);
      return { content: [{ type: "text", text: JSON.stringify({ stored: true, key: rk }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ stored: false, error: e.message }) }] };
    }
  });

  server.tool("proc_get",
    "Read all procedural memory knobs for an agent as compact JSON. Call once at session start and inject into system prompt — zero tool calls needed during the session.", {
    agent_name: z.string().describe("Agent identifier")
  }, async ({ agent_name }) => {
    try {
      const result = await procGet(agent_name);
      return { content: [{ type: "text", text: JSON.stringify(result) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- Infrastructure Tools (full profile only) ---
  if (profile === "full") {

  // --- Prometheus Monitoring Tools ---

  server.tool("prometheus_get_targets", "Returns the status of all targets Prometheus is currently scraping", {}, async () => {
    try {
      const response = await fetch("http://prometheus:9090/api/v1/targets");
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const data = await response.json();
      const targets = data.data.activeTargets.map(t => ({
        job: t.labels.job, instance: t.labels.instance, health: t.health, last_scrape: t.lastScrape
      }));
      return { content: [{ type: "text", text: JSON.stringify({ targets }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "request failed" }) }] };
    }
  });

  server.tool("prometheus_get_alerts", "Returns any currently firing or pending Prometheus alerts", {}, async () => {
    try {
      const response = await fetch("http://prometheus:9090/api/v1/alerts");
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const data = await response.json();
      return { content: [{ type: "text", text: JSON.stringify({ alerts: data.data.alerts }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "request failed" }) }] };
    }
  });

  server.tool("prometheus_query", "Execute a PromQL query against Prometheus", {
    promql_query: z.string().describe("The PromQL query string to execute")
  }, async ({ promql_query }) => {
    try {
      const response = await fetch(`http://prometheus:9090/api/v1/query?query=${encodeURIComponent(promql_query)}`);
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const data = await response.json();
      return { content: [{ type: "text", text: JSON.stringify(data.data) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "request failed" }) }] };
    }
  });

  // --- Grafana Monitoring Tools — fail closed if creds not configured (#6) ---

  const grafanaUser = process.env.GRAFANA_USER;
  const grafanaPass = process.env.GRAFANA_PASSWORD;
  const grafanaAuth = (grafanaUser && grafanaPass)
    ? "Basic " + Buffer.from(`${grafanaUser}:${grafanaPass}`).toString("base64")
    : null;

  function grafanaHeaders() {
    if (!grafanaAuth) throw new Error("Grafana credentials not configured (GRAFANA_USER / GRAFANA_PASSWORD not set)");
    return { "Authorization": grafanaAuth };
  }

  server.tool("grafana_get_health", "Check the health status of the Grafana server", {}, async () => {
    try {
      const response = await fetch("http://grafana:3000/api/health", { headers: grafanaHeaders() });
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      return { content: [{ type: "text", text: JSON.stringify(await response.json()) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message.includes("credentials") ? e.message : "request failed" }) }] };
    }
  });

  server.tool("grafana_list_dashboards", "List all available dashboards in Grafana", {}, async () => {
    try {
      const response = await fetch("http://grafana:3000/api/search?type=dash-db", { headers: grafanaHeaders() });
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const dashboards = await response.json();
      return { content: [{ type: "text", text: JSON.stringify({ dashboards: dashboards.map(d => ({ title: d.title, url: d.url })) }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message.includes("credentials") ? e.message : "request failed" }) }] };
    }
  });

  // --- Proxmox Documentation Tools ---

  async function searchDocs(keyPattern, contentFilter) {
    const keys = [];
    for await (const key of scanKeys(keyPattern, "string")) keys.push(key);
    if (keys.length === 0) return [];
    const results = [];
    for (const key of keys) {
      const val = await redis.get(key);
      if (val && (!contentFilter || val.toLowerCase().includes(contentFilter.toLowerCase())))
        results.push({ key, content: val });
    }
    return results;
  }

  server.tool("proxmox_get_cluster_status", "Retrieve the Proxmox cluster status document from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:proxmox:cluster-status*", null);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ error: "No cluster status document found. Store one with key 'doc:proxmox:cluster-status' using cache_set." }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  server.tool("proxmox_list_nodes", "List all Proxmox nodes documented in the knowledge base", {}, async () => {
    const results = await searchDocs("doc:proxmox:node:*", null);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ error: "No node documents found." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ nodes: results.map(r => ({ key: r.key, title: r.key.split(":").pop() })) }) }] };
  });

  server.tool("proxmox_find_vm", "Search for a specific VM or container by ID or name in the knowledge base", {
    vm_id_or_name: z.string().describe("VM ID or name to search for")
  }, async ({ vm_id_or_name }) => {
    let results = await searchDocs("doc:proxmox:vm:*", vm_id_or_name);
    if (!results.length) results = await searchDocs("doc:proxmox:ct:*", vm_id_or_name);
    if (!results.length) results = await searchDocs("doc:proxmox:*", vm_id_or_name);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ error: `No VM or CT found matching "${vm_id_or_name}".` }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  // --- Ceph Documentation Tools ---

  server.tool("ceph_get_health_status", "Retrieve the latest Ceph cluster health status from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:ceph:cluster-status*", null);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ error: "No Ceph status document found." }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  server.tool("ceph_list_osd_notes", "List all Ceph OSD documentation notes from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:ceph:osd:*", null);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ error: "No OSD documents found." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ osd_notes: results.map(r => r.key) }) }] };
  });

  // --- Network Documentation Tools ---

  server.tool("network_search_docs", "Search across network and infrastructure documentation in the knowledge base", {
    search_query: z.string().describe("Search query to find relevant documentation")
  }, async ({ search_query }) => {
    const results = [...await searchDocs("doc:network:*", search_query), ...await searchDocs("doc:infrastructure:*", search_query)];
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ status: "No relevant documents found." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ results: results.slice(0, 5).map(r => ({ key: r.key, preview: r.content.substring(0, 200) })) }) }] };
  });

  server.tool("network_get_runbook", "Retrieve a specific network runbook by topic from the knowledge base", {
    topic: z.string().describe("Runbook topic to search for")
  }, async ({ topic }) => {
    const results = await searchDocs("doc:runbook:*", topic);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ error: `No runbook found for topic: "${topic}".` }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  // --- User Context Tools ---

  const USER_HISTORY_TTL = 90 * 24 * 60 * 60;

  server.tool("user_context_add_entry", "Add an entry to a user's daily interaction history log", {
    user_id: z.string().describe("User identifier"),
    content: z.string().describe("Content to log")
  }, async ({ user_id, content }) => {
    const today = new Date().toISOString().split("T")[0];
    const key = `doc:users:${user_id}:history:${today}`;
    let existing = await redis.get(key);
    if (!existing) existing = `## User History for ${user_id} - ${today}\n\n`;
    const entry = `**[${new Date().toLocaleTimeString()}]**\n${content}\n\n---\n\n`;
    const updated = existing + entry;
    await redis.setEx(key, USER_HISTORY_TTL, updated);
    indexAdd(key, updated, "longterm", new Date().toISOString());
    return { content: [{ type: "text", text: JSON.stringify({ success: true, key }) }] };
  });

  server.tool("user_context_get_history", "Retrieve the full interaction history for a user on a specific date", {
    user_id: z.string().describe("User identifier"),
    date:    z.string().describe("Date in YYYY-MM-DD format")
  }, async ({ user_id, date }) => {
    const key = `doc:users:${user_id}:history:${date}`;
    const val = await redis.get(key);
    if (!val) return { content: [{ type: "text", text: JSON.stringify({ status: "Not Found", message: `No history for user '${user_id}' on '${date}'.` }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ user_id, date, history: val }) }] };
  });

  server.tool("user_context_search_history", "Search across all of a user's history documents by keyword", {
    user_id: z.string().describe("User identifier"),
    query:   z.string().describe("Search query")
  }, async ({ user_id, query }) => {
    const results = await searchDocs(`doc:users:${user_id}:history:*`, query);
    if (!results.length) return { content: [{ type: "text", text: JSON.stringify({ status: "No matches found." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({
      results: results.map(r => ({ date: r.key.split(":").pop(), match_preview: r.content.substring(0, 200) + "..." }))
    }) }] };
  });

  } // end profile === "full"

  return server;
}

// ---------------------------------------------------------------------------
// SSE transport — fix #3: max session cap + cleanup on error/close
// ---------------------------------------------------------------------------

const MAX_SESSIONS = 50;
const transports = {};

app.get("/sse", requireAuth, async (req, res) => {
  if (Object.keys(transports).length >= MAX_SESSIONS) {
    return res.status(503).json({ error: "Max concurrent sessions reached" });
  }
  const profile = req.query.profile === "slim" ? "slim" : "full";
  const transport = new SSEServerTransport("/messages", res);
  const mcpServer = createMcpServerInstance(profile);
  const sessionId = transport.sessionId;
  transports[sessionId] = { transport, server: mcpServer };

  const cleanup = () => {
    if (!transports[sessionId]) return;
    mcpServer.close().catch(() => {});
    delete transports[sessionId];
  };

  res.on("close", cleanup);
  req.on("close", cleanup);

  try {
    await mcpServer.connect(transport);
  } catch (e) {
    cleanup();
    console.error("SSE connect error:", e.message);
    if (!res.headersSent) res.status(500).json({ error: "Connection failed" });
  }
});

app.post("/messages", requireAuth, async (req, res) => {
  const sessionId = req.query.sessionId;
  const entry = transports[sessionId];
  if (entry) await entry.transport.handlePostMessage(req, res);
  else res.status(400).json({ error: "No transport found for sessionId" });
});

// ---------------------------------------------------------------------------
// REST API (backwards compatibility) — all routes require auth except /health
// ---------------------------------------------------------------------------

const jsonParser = express.json();

app.get("/health", async (req, res) => {
  const redisOk = redis.isReady;
  let obsidianOk = false;
  try { await fs.access(OBSIDIAN_VAULT_PATH); obsidianOk = true; } catch {}
  res.json({
    status: (redisOk && obsidianOk) ? "healthy" : "degraded",
    redis: redisOk, obsidian: obsidianOk,
    vault_path: OBSIDIAN_VAULT_PATH,
    bm25_docs: bm25.size,
    uptime: process.uptime()
  });
});

app.get("/api/tools", requireAuth, (req, res) => {
  const profile = req.query.profile === "slim" ? "slim" : "full";
  const core = [
    "cache_set", "cache_get", "cache_delete", "redis_stats", "health_check",
    "obsidian_write_note", "obsidian_read_note", "obsidian_delete_note",
    "obsidian_search_notes", "obsidian_list_notes",
    "heurchain_search", "heurchain_export",
    "proc_set", "proc_get"
  ];
  const infra = [
    "prometheus_get_targets", "prometheus_get_alerts", "prometheus_query",
    "grafana_get_health", "grafana_list_dashboards",
    "proxmox_get_cluster_status", "proxmox_list_nodes", "proxmox_find_vm",
    "ceph_get_health_status", "ceph_list_osd_notes",
    "network_search_docs", "network_get_runbook",
    "user_context_add_entry", "user_context_get_history", "user_context_search_history"
  ];
  res.json({
    profile,
    tools: profile === "slim" ? core : [...core, ...infra],
    rest: ["/api/search", "/api/buffer", "/api/proc", "/api/session-context"]
  });
});

app.post("/api/cache", requireAuth, jsonParser, async (req, res) => {
  const { key, value, ttl } = req.body;
  if (ttl) await redis.setEx(key, ttl, JSON.stringify(value));
  else await redis.set(key, JSON.stringify(value));
  res.json({ cached: true });
});

app.get("/api/cache/:key", requireAuth, async (req, res) => {
  const val = await redis.get(req.params.key);
  if (val) res.json({ hit: true, data: JSON.parse(val) });
  else res.json({ hit: false });
});

app.get("/api/stats", requireAuth, async (req, res) => {
  const info = await redis.info("stats");
  res.json({ redis_info: info, bm25_docs: bm25.size });
});

app.get("/api/obsidian/notes", requireAuth, async (req, res) => {
  const { key, prefix } = req.query;
  if (key) {
    try {
      const content = await readFromObsidian(key);
      if (content === null) return res.status(404).json({ found: false, key });
      return res.json({ found: true, key, content });
    } catch (e) {
      return res.status(400).json({ error: e.message.startsWith("invalid key") ? e.message : "read failed" });
    }
  }
  const notes = await listObsidian(prefix);
  res.json({ count: notes.length, notes });
});

app.post("/api/obsidian/notes", requireAuth, jsonParser, async (req, res) => {
  const { key, content, metadata } = req.body;
  if (!key || content === undefined) return res.status(400).json({ error: "key and content required" });
  try {
    const filePath = await saveToObsidian(key, content, metadata);
    res.json({ stored: true, key, path: filePath });
  } catch (e) {
    res.status(e.message.startsWith("invalid key") ? 400 : 500)
       .json({ error: e.message.startsWith("invalid key") ? e.message : "write failed" });
  }
});

app.delete("/api/obsidian/notes", requireAuth, async (req, res) => {
  const { key } = req.query;
  if (!key) return res.status(400).json({ error: "key required" });
  try {
    const deleted = await deleteFromObsidian(key);
    res.json({ deleted, key });
  } catch (e) {
    res.status(400).json({ error: e.message.startsWith("invalid key") ? e.message : "delete failed" });
  }
});

app.get("/api/obsidian/search", requireAuth, async (req, res) => {
  const { q, limit } = req.query;
  if (!q) return res.status(400).json({ error: "q required" });
  const max = Math.max(1, Math.min(parseInt(limit) || 10, 100));
  const results = await searchObsidian(q, null);
  res.json({ count: results.length, results: results.slice(0, max).map(r => ({ key: r.key, path: r.path, preview: r.content.substring(0, 200) })) });
});

// ---------------------------------------------------------------------------
// BM25 search REST — frictionless recall via curl/hook, no MCP tool overhead
// ---------------------------------------------------------------------------

app.get("/api/search", requireAuth, async (req, res) => {
  const { q, limit, tier, minScore, namespace } = req.query;
  if (!q) return res.status(400).json({ error: "q required" });
  const results = bm25.search(q, {
    limit:       Math.min(parseInt(limit) || 10, 50),
    tier:        tier || "all",
    minScore:    parseFloat(minScore) || 0,
    noiseFilter: true,
    namespace:   namespace || null
  });
  res.json({
    count: results.length,
    index_size: bm25.size,
    results: results.map(r => ({
      key:     r.key,
      score:   Math.round(r.score * 1000) / 1000,
      tier:    r.tier,
      preview: (r.content || "").substring(0, 300)
    }))
  });
});

// ---------------------------------------------------------------------------
// Session buffer — ambient turn capture, POST fire-and-forget from agent hook
// ---------------------------------------------------------------------------

app.post("/api/buffer", requireAuth, jsonParser, async (req, res) => {
  const { session_id, agent_name, role, content } = req.body;
  if (!session_id || !content) return res.status(400).json({ error: "session_id and content required" });
  bufferAppend(session_id, agent_name, role, content);
  res.json({ buffered: true, session_id });
});

app.post("/api/buffer/flush", requireAuth, jsonParser, async (req, res) => {
  const { session_id } = req.body;
  if (!session_id) return res.status(400).json({ error: "session_id required" });
  await flushBuffer(session_id);
  res.json({ flushed: true, session_id });
});

// ---------------------------------------------------------------------------
// Procedural memory REST — stable agent preference knobs, no TTL
// ---------------------------------------------------------------------------

app.post("/api/proc", requireAuth, jsonParser, async (req, res) => {
  const { agent_name, key, value } = req.body;
  if (!agent_name || !key || value === undefined) return res.status(400).json({ error: "agent_name, key, value required" });
  try {
    const rk = procKey(agent_name, key);
    await redis.set(rk, typeof value === "string" ? value : JSON.stringify(value));
    res.json({ stored: true, key: rk });
  } catch (e) {
    res.status(400).json({ error: e.message });
  }
});

app.get("/api/proc/:agent", requireAuth, async (req, res) => {
  try {
    if (!PROC_KEY_RE.test(req.params.agent)) return res.status(400).json({ error: "invalid agent name" });
    res.json(await procGet(req.params.agent));
  } catch (e) {
    res.status(500).json({ error: "proc read failed" });
  }
});

app.delete("/api/proc/:agent/:key", requireAuth, async (req, res) => {
  try {
    const rk = procKey(req.params.agent, req.params.key);
    const deleted = await redis.del(rk);
    res.json({ deleted: deleted > 0, key: rk });
  } catch (e) {
    res.status(400).json({ error: e.message });
  }
});

// ---------------------------------------------------------------------------
// Session-context injection — compact recall for system-prompt injection.
// Returns proc knobs + last session summary + top BM25 hits, hard-capped at
// SESSION_CONTEXT_TOKENS (~400 tokens). Call via hook at session start;
// inject the JSON into system prompt. Zero MCP tool overhead.
// ---------------------------------------------------------------------------

app.get("/api/session-context", requireAuth, async (req, res) => {
  const { agent, hint } = req.query;
  if (!agent) return res.status(400).json({ error: "agent required" });
  if (!PROC_KEY_RE.test(agent)) return res.status(400).json({ error: "invalid agent name" });

  // 1. Procedural knobs
  let proc = {};
  try { proc = await procGet(agent); } catch {}

  // 2. Last session summary — scan for most recent buffer entry or session record
  let last_session = null;
  try {
    const candidates = [];
    for await (const k of scanKeys("session:*:buffer", "string")) {
      const raw = await redis.get(k);
      if (!raw) continue;
      try {
        const turns = JSON.parse(raw);
        const agentTurns = turns.filter(t => t.agent === agent);
        if (agentTurns.length === 0) continue;
        const last = agentTurns[agentTurns.length - 1];
        candidates.push({ ts: last.ts, summary: last.content.substring(0, 200) });
      } catch {}
    }
    if (candidates.length > 0) {
      candidates.sort((a, b) => b.ts.localeCompare(a.ts));
      last_session = candidates[0].summary;
    }
  } catch {}

  // 3. BM25 relevant results, token-budgeted
  let relevant = [];
  if (hint) {
    try {
      const hits = bm25.search(hint, { limit: 15, minScore: 0.5, noiseFilter: true });
      let budget = SESSION_CONTEXT_TOKENS - JSON.stringify(proc).length / 4 - 50;
      for (const r of hits) {
        const excerpt = (r.content || "").substring(0, 200);
        budget -= Math.ceil((r.key.length + excerpt.length) / 4);
        if (budget < 0) break;
        relevant.push({ key: r.key, score: Math.round(r.score * 100) / 100, excerpt });
      }
    } catch {}
  }

  res.json({ proc, last_session, relevant });
});

// ---------------------------------------------------------------------------
// Startup
// ---------------------------------------------------------------------------

(async () => {
  await redis.connect();
  await buildIndex();
  setInterval(buildIndex, 60 * 60 * 1000); // hourly BM25 rebuild
  // Flush all active session buffers on heartbeat
  setInterval(async () => {
    for (const sessionId of sessionBuffers.keys()) {
      await flushBuffer(sessionId).catch(() => {});
    }
  }, BUFFER_FLUSH_MS);
  const port = parseInt(process.env.MCP_PORT, 10) || 3010;
  app.listen(port, () => console.log(`HeurChain MCP v1.3.0 listening on port ${port} — ${bm25.size} docs indexed`));
})();
