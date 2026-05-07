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

const OBSIDIAN_VAULT_PATH = process.env.OBSIDIAN_VAULT_PATH || "/opt/obsidian-vault";

// ---------------------------------------------------------------------------
// BM25 Index
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

// Low-signal session metadata patterns — excluded from search by default
const NOISE_KEY_PATTERNS = [/session-summary/, /agent-end/, /heartbeat/];
const NOISE_CONTENT_PATTERNS = [
  /^\d+ user turns,\s*\d+ total messages/i,
  /^openclaw session on [a-z0-9-]+:/i,
  /^session ended\./i
];

function isSessionNoise(key, content) {
  if (NOISE_KEY_PATTERNS.some(p => p.test(key))) return true;
  const trimmed = (content || "").trim();
  return NOISE_CONTENT_PATTERNS.some(p => p.test(trimmed));
}

class BM25Index {
  constructor(k1 = 1.5, b = 0.75) {
    this.k1 = k1;
    this.b = b;
    this.docs = new Map();  // key -> { tf, tokens, content, tier, updatedAt }
    this.df = new Map();    // term -> document frequency count
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

  add(key, content, tier, updatedAt) {
    if (this.docs.has(key)) this._removeFromIndex(key);
    const tokens = this.tokenize(content);
    const tf = new Map();
    for (const t of tokens) tf.set(t, (tf.get(t) || 0) + 1);
    this.docs.set(key, { tf, tokens, content, tier, updatedAt });
    for (const term of tf.keys()) this.df.set(term, (this.df.get(term) || 0) + 1);
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
      const count = this.df.get(term) || 0;
      if (count <= 1) this.df.delete(term);
      else this.df.set(term, count - 1);
    }
    this.docs.delete(key);
  }

  _recalcStats() {
    this.N = this.docs.size;
    if (this.N === 0) { this.avgLen = 0; return; }
    let total = 0;
    for (const doc of this.docs.values()) total += doc.tokens.length;
    this.avgLen = total / this.N;
  }

  _scoreDoc(queryTokens, docKey) {
    const doc = this.docs.get(docKey);
    if (!doc) return 0;
    const dl = doc.tokens.length;
    const lenNorm = this.avgLen > 0 ? dl / this.avgLen : 1;
    let score = 0;
    for (const term of queryTokens) {
      const tf = doc.tf.get(term) || 0;
      if (tf === 0) continue;
      const df = this.df.get(term) || 0;
      const idf = Math.log((this.N - df + 0.5) / (df + 0.5) + 1);
      const tfNorm = (tf * (this.k1 + 1)) / (tf + this.k1 * (1 - this.b + this.b * lenNorm));
      score += idf * tfNorm;
    }
    return score;
  }

  search(query, { limit = 10, tier = "all", minScore = 0, noiseFilter = true, namespace = null } = {}) {
    const queryTokens = this.tokenize(query);
    if (queryTokens.length === 0) return [];
    const results = [];
    for (const [key, doc] of this.docs.entries()) {
      if (tier !== "all" && doc.tier !== tier) continue;
      if (namespace && !key.startsWith(namespace + ":")) continue;
      if (noiseFilter && isSessionNoise(key, doc.content)) continue;
      const score = this._scoreDoc(queryTokens, key);
      if (score > minScore) {
        results.push({ key, score, tier: doc.tier, content: doc.content, updated_at: doc.updatedAt });
      }
    }
    return results.sort((a, b) => b.score - a.score).slice(0, limit);
  }

  get size() { return this.N; }
}

const bm25 = new BM25Index();

// ---------------------------------------------------------------------------
// Index Build & Maintenance
// ---------------------------------------------------------------------------

async function buildIndex() {
  const freshIndex = new BM25Index();

  // Scan all Redis keys
  let redisCount = 0;
  try {
    for await (const key of redis.scanIterator({ MATCH: "*", COUNT: 200 })) {
      const val = await redis.get(key);
      if (val) {
        freshIndex.add(key, val, "longterm", null);
        redisCount++;
      }
    }
  } catch (e) {
    console.error("Index build — Redis scan error:", e.message);
  }

  // Walk Obsidian vault
  let vaultCount = 0;
  async function walk(dir) {
    let entries;
    try { entries = await fs.readdir(dir, { withFileTypes: true }); }
    catch (e) { if (e.code === "ENOENT") return; throw e; }
    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) { await walk(fullPath); continue; }
      if (!entry.name.endsWith(".md")) continue;
      try {
        const content = await fs.readFile(fullPath, "utf-8");
        const stat = await fs.stat(fullPath);
        const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
        const key = relative.replace(/\//g, ":").replace(/\.md$/, "");
        freshIndex.add(key, content, "working", stat.mtime.toISOString());
        vaultCount++;
      } catch (e) {
        console.error("Index build — vault read error:", fullPath, e.message);
      }
    }
  }
  await walk(OBSIDIAN_VAULT_PATH);

  // Swap in new index atomically
  bm25.docs = freshIndex.docs;
  bm25.df = freshIndex.df;
  bm25.avgLen = freshIndex.avgLen;
  bm25.N = freshIndex.N;

  console.log(`BM25 index built: ${redisCount} Redis keys + ${vaultCount} vault files = ${bm25.size} docs`);
}

// ---------------------------------------------------------------------------
// Obsidian Vault Helpers
// ---------------------------------------------------------------------------

function keyToObsidianPath(key) {
  const relativePath = key.replace(/:/g, "/") + ".md";
  return path.join(OBSIDIAN_VAULT_PATH, relativePath);
}

async function saveToObsidian(key, content, metadata) {
  const filePath = keyToObsidianPath(key);
  await fs.mkdir(path.dirname(filePath), { recursive: true });
  let output = content;
  if (metadata && Object.keys(metadata).length > 0) {
    const frontmatter = ["---"];
    for (const [k, v] of Object.entries(metadata)) {
      frontmatter.push(`${k}: ${Array.isArray(v) ? `[${v.join(", ")}]` : v}`);
    }
    frontmatter.push("---", "");
    output = frontmatter.join("\n") + content;
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

async function searchObsidian(query) {
  const results = [];
  async function walk(dir) {
    const entries = await fs.readdir(dir, { withFileTypes: true });
    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) { await walk(fullPath); continue; }
      if (!entry.name.endsWith(".md")) continue;
      const content = await fs.readFile(fullPath, "utf-8");
      if (content.toLowerCase().includes(query.toLowerCase())) {
        const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
        const key = relative.replace(/\//g, ":").replace(/\.md$/, "");
        results.push({ key, content, path: relative });
      }
    }
  }
  try { await walk(OBSIDIAN_VAULT_PATH); }
  catch (e) { if (e.code !== "ENOENT") throw e; }
  return results;
}

async function listObsidian(prefix) {
  const results = [];
  async function walk(dir) {
    const entries = await fs.readdir(dir, { withFileTypes: true });
    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) { await walk(fullPath); continue; }
      if (!entry.name.endsWith(".md")) continue;
      const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
      const key = relative.replace(/\//g, ":").replace(/\.md$/, "");
      if (!prefix || key.startsWith(prefix)) results.push({ key, path: relative });
    }
  }
  try { await walk(OBSIDIAN_VAULT_PATH); }
  catch (e) { if (e.code !== "ENOENT") throw e; }
  return results;
}

// ---------------------------------------------------------------------------
// MCP Server Factory
// ---------------------------------------------------------------------------

function createMcpServerInstance() {
  const server = new McpServer({
    name: "heurchain-mcp",
    version: "1.2.0"
  });

  // --- Cache Tools ---

  server.tool("cache_set", "Store a key-value pair in Redis (longterm tier). Optionally scope to a namespace.", {
    key: z.string().describe("Cache key"),
    value: z.string().describe("Value to cache"),
    ttl: z.number().optional().describe("Time-to-live in seconds"),
    namespace: z.string().optional().describe("Optional namespace prefix, e.g. 'agent-hermes'. Key becomes namespace:key.")
  }, async ({ key, value, ttl, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    if (ttl) await redis.setEx(fullKey, ttl, value);
    else await redis.set(fullKey, value);
    bm25.add(fullKey, value, "longterm", new Date().toISOString());
    return { content: [{ type: "text", text: JSON.stringify({ cached: true, key: fullKey }) }] };
  });

  server.tool("cache_get", "Retrieve a cached value from Redis by key", {
    key: z.string().describe("Cache key to retrieve"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    const val = await redis.get(fullKey);
    if (val) return { content: [{ type: "text", text: val }] };
    return { content: [{ type: "text", text: JSON.stringify({ hit: false }) }] };
  });

  server.tool("cache_delete", "Delete a cached key from Redis", {
    key: z.string().describe("Cache key to delete"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    const deleted = await redis.del(fullKey);
    bm25.remove(fullKey);
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
    return {
      content: [{
        type: "text",
        text: JSON.stringify({
          status: (redisOk && obsidianOk) ? "healthy" : "degraded",
          redis: redisOk,
          obsidian: obsidianOk,
          vault_path: OBSIDIAN_VAULT_PATH,
          bm25_docs: bm25.size,
          uptime: process.uptime()
        })
      }]
    };
  });

  // --- Obsidian Vault Tools (Working Tier / Human-Readable Glacier) ---

  server.tool("obsidian_write_note", "Write a markdown note to the Obsidian vault (working tier — human-readable glacier). Key colons become folder separators.", {
    key: z.string().describe("Note key (e.g. doc:proxmox:setup)"),
    content: z.string().describe("Markdown body"),
    metadata: z.record(z.string()).optional().describe("Optional frontmatter metadata"),
    namespace: z.string().optional().describe("Optional namespace prefix")
  }, async ({ key, content, metadata, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try {
      const filePath = await saveToObsidian(fullKey, content, metadata);
      bm25.add(fullKey, content, "working", new Date().toISOString());
      return { content: [{ type: "text", text: JSON.stringify({ stored: true, key: fullKey, path: filePath, tier: "working" }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ stored: false, key: fullKey, error: e.message }) }] };
    }
  });

  server.tool("obsidian_read_note", "Read a markdown note from the Obsidian vault by key", {
    key: z.string().describe("Note key"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try {
      const content = await readFromObsidian(fullKey);
      if (content === null) return { content: [{ type: "text", text: JSON.stringify({ found: false, key: fullKey }) }] };
      return { content: [{ type: "text", text: JSON.stringify({ found: true, key: fullKey, content }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ found: false, key: fullKey, error: e.message }) }] };
    }
  });

  server.tool("obsidian_delete_note", "Delete a markdown note from the Obsidian vault by key", {
    key: z.string().describe("Note key to delete"),
    namespace: z.string().optional().describe("Namespace prefix used when storing")
  }, async ({ key, namespace }) => {
    const fullKey = namespace ? `${namespace}:${key}` : key;
    try {
      const deleted = await deleteFromObsidian(fullKey);
      bm25.remove(fullKey);
      return { content: [{ type: "text", text: JSON.stringify({ deleted, key: fullKey }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ deleted: false, key: fullKey, error: e.message }) }] };
    }
  });

  server.tool("obsidian_search_notes", "Search markdown notes in the Obsidian vault by keyword (substring match, vault only). Prefer heurchain_search for ranked results across all tiers.", {
    query: z.string().describe("Search term"),
    limit: z.number().optional().describe("Max results").default(10),
    namespace: z.string().optional().describe("Restrict to a namespace prefix")
  }, async ({ query, limit, namespace }) => {
    try {
      let results = await searchObsidian(query);
      if (namespace) results = results.filter(r => r.key.startsWith(namespace + ":"));
      const sliced = results.slice(0, limit);
      return { content: [{ type: "text", text: JSON.stringify({
        count: results.length,
        results: sliced.map(r => ({ key: r.key, preview: r.content.substring(0, 200) }))
      }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  server.tool("obsidian_list_notes", "List all markdown notes in the Obsidian vault, optionally filtered by prefix", {
    prefix: z.string().optional().describe("Key prefix filter, e.g. doc:proxmox:")
  }, async ({ prefix }) => {
    try {
      const results = await listObsidian(prefix);
      return { content: [{ type: "text", text: JSON.stringify({ count: results.length, notes: results }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- HeurChain BM25 Search (native, in-process) ---

  server.tool("heurchain_search",
    "Search the full knowledge base using HeurChain's native BM25 ranked index. Covers all Redis keys (longterm tier) and Obsidian vault files (working tier) with relevance scoring. Preferred over obsidian_search_notes.", {
    query: z.string().describe("Search query"),
    limit: z.number().optional().describe("Max results (default 10, max 50)").default(10),
    tier: z.string().optional().describe("'all' | 'longterm' | 'working' (default: all)").default("all"),
    minScore: z.number().optional().describe("Minimum BM25 score threshold (default 0 = all results; try 0.5 to cut noise)").default(0),
    noiseFilter: z.boolean().optional().describe("Skip low-signal session metadata entries (default true)").default(true),
    namespace: z.string().optional().describe("Restrict search to a namespace, e.g. 'agent-hermes'")
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
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- heurchain_export (Glacier Sync) ---

  server.tool("heurchain_export",
    "Export Redis longterm-tier entries to Obsidian vault markdown files. Makes the Redis knowledge base human-readable and browsable. Idempotent — safe to run repeatedly.", {
    prefix: z.string().optional().describe("Only export keys matching this prefix (e.g. 'doc:', 'memory:'). Defaults to all keys."),
    overwrite: z.boolean().optional().describe("Overwrite existing vault files (default false — skip if file exists)").default(false)
  }, async ({ prefix, overwrite }) => {
    let exported = 0, skipped = 0, errors = 0;
    try {
      for await (const key of redis.scanIterator({ MATCH: prefix ? `${prefix}*` : "*", COUNT: 100 })) {
        const val = await redis.get(key);
        if (!val) continue;
        const filePath = keyToObsidianPath(key);
        if (!overwrite) {
          try { await fs.access(filePath); skipped++; continue; } catch {}
        }
        try {
          await fs.mkdir(path.dirname(filePath), { recursive: true });
          const header = `---\nkey: ${key}\nexported_at: ${new Date().toISOString()}\nsource: redis-longterm\n---\n\n`;
          await fs.writeFile(filePath, header + val, "utf-8");
          bm25.add(key, val, "longterm", new Date().toISOString());
          exported++;
        } catch (e) {
          console.error(`heurchain_export: failed to write ${key}:`, e.message);
          errors++;
        }
      }
      return { content: [{ type: "text", text: JSON.stringify({ exported, skipped, errors, vault_path: OBSIDIAN_VAULT_PATH }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message, exported, skipped, errors }) }] };
    }
  });

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
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  server.tool("prometheus_get_alerts", "Returns any currently firing or pending Prometheus alerts", {}, async () => {
    try {
      const response = await fetch("http://prometheus:9090/api/v1/alerts");
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const data = await response.json();
      return { content: [{ type: "text", text: JSON.stringify({ alerts: data.data.alerts }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
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
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- Grafana Monitoring Tools ---

  const grafanaAuth = "Basic " + Buffer.from(`${process.env.GRAFANA_USER || "admin"}:${process.env.GRAFANA_PASSWORD || "admin"}`).toString("base64");

  server.tool("grafana_get_health", "Check the health status of the Grafana server", {}, async () => {
    try {
      const response = await fetch("http://grafana:3000/api/health", { headers: { "Authorization": grafanaAuth } });
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      return { content: [{ type: "text", text: JSON.stringify(await response.json()) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  server.tool("grafana_list_dashboards", "List all available dashboards in Grafana", {}, async () => {
    try {
      const response = await fetch("http://grafana:3000/api/search?type=dash-db", { headers: { "Authorization": grafanaAuth } });
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const dashboards = await response.json();
      return { content: [{ type: "text", text: JSON.stringify({ dashboards: dashboards.map(d => ({ title: d.title, url: d.url })) }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- Proxmox Documentation Tools (Redis-backed) ---

  async function searchDocs(keyPattern, contentFilter) {
    const keys = [];
    for await (const key of redis.scanIterator({ MATCH: keyPattern, COUNT: 100 })) keys.push(key);
    if (keys.length === 0) return [];
    const results = [];
    for (const key of keys) {
      const val = await redis.get(key);
      if (val && (!contentFilter || val.toLowerCase().includes(contentFilter.toLowerCase()))) {
        results.push({ key, content: val });
      }
    }
    return results;
  }

  server.tool("proxmox_get_cluster_status", "Retrieve the Proxmox cluster status document from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:proxmox:cluster-status*", null);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ error: "No cluster status document found. Store one with key 'doc:proxmox:cluster-status' using cache_set." }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  server.tool("proxmox_list_nodes", "List all Proxmox nodes documented in the knowledge base", {}, async () => {
    const results = await searchDocs("doc:proxmox:node:*", null);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ error: "No node documents found. Store them with keys like 'doc:proxmox:node:pver430' using cache_set." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ nodes: results.map(r => ({ key: r.key, title: r.key.split(":").pop() })) }) }] };
  });

  server.tool("proxmox_find_vm", "Search for a specific VM or container by ID or name in the knowledge base", {
    vm_id_or_name: z.string().describe("VM ID or name to search for")
  }, async ({ vm_id_or_name }) => {
    let results = await searchDocs("doc:proxmox:vm:*", vm_id_or_name);
    if (results.length === 0) results = await searchDocs("doc:proxmox:ct:*", vm_id_or_name);
    if (results.length === 0) results = await searchDocs("doc:proxmox:*", vm_id_or_name);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ error: `No VM or CT found matching "${vm_id_or_name}".` }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  // --- Ceph Documentation Tools (Redis-backed) ---

  server.tool("ceph_get_health_status", "Retrieve the latest Ceph cluster health status from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:ceph:cluster-status*", null);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ error: "No Ceph status document found. Store one with key 'doc:ceph:cluster-status' using cache_set." }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  server.tool("ceph_list_osd_notes", "List all Ceph OSD documentation notes from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:ceph:osd:*", null);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ error: "No OSD documents found. Store them with keys like 'doc:ceph:osd:0' using cache_set." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ osd_notes: results.map(r => r.key) }) }] };
  });

  // --- Network Documentation Tools (Redis-backed) ---

  server.tool("network_search_docs", "Search across network and infrastructure documentation in the knowledge base", {
    search_query: z.string().describe("Search query to find relevant documentation")
  }, async ({ search_query }) => {
    const results = [...await searchDocs("doc:network:*", search_query), ...await searchDocs("doc:infrastructure:*", search_query)];
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ status: "No relevant documents found." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ results: results.slice(0, 5).map(r => ({ key: r.key, preview: r.content.substring(0, 200) })) }) }] };
  });

  server.tool("network_get_runbook", "Retrieve a specific network runbook by topic from the knowledge base", {
    topic: z.string().describe("Runbook topic to search for")
  }, async ({ topic }) => {
    const results = await searchDocs("doc:runbook:*", topic);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ error: `No runbook found for topic: "${topic}". Store runbooks with keys like 'doc:runbook:topic-name' using cache_set.` }) }] };
    return { content: [{ type: "text", text: results[0].content }] };
  });

  // --- User Context Tools (Redis-backed) ---

  const USER_HISTORY_TTL = 90 * 24 * 60 * 60; // 90 days

  server.tool("user_context_add_entry", "Add an entry to a user's daily interaction history log", {
    user_id: z.string().describe("User identifier"),
    content: z.string().describe("Content to log")
  }, async ({ user_id, content }) => {
    const today = new Date().toISOString().split("T")[0];
    const key = `doc:users:${user_id}:history:${today}`;
    let existing = await redis.get(key);
    if (!existing) existing = `## User History for ${user_id} - ${today}\n\n`;
    const timestamp = new Date().toLocaleTimeString();
    const entry = `**[${timestamp}]**\n${content}\n\n---\n\n`;
    const updated = existing + entry;
    await redis.setEx(key, USER_HISTORY_TTL, updated);
    bm25.add(key, updated, "longterm", new Date().toISOString());
    return { content: [{ type: "text", text: JSON.stringify({ success: true, key, entry_added: entry }) }] };
  });

  server.tool("user_context_get_history", "Retrieve the full interaction history for a user on a specific date", {
    user_id: z.string().describe("User identifier"),
    date: z.string().describe("Date in YYYY-MM-DD format")
  }, async ({ user_id, date }) => {
    const key = `doc:users:${user_id}:history:${date}`;
    const val = await redis.get(key);
    if (!val) return { content: [{ type: "text", text: JSON.stringify({ status: "Not Found", message: `No history found for user '${user_id}' on date '${date}'.` }) }] };
    return { content: [{ type: "text", text: JSON.stringify({ user_id, date, history: val }) }] };
  });

  server.tool("user_context_search_history", "Search across all of a user's history documents by keyword", {
    user_id: z.string().describe("User identifier"),
    query: z.string().describe("Search query")
  }, async ({ user_id, query }) => {
    const results = await searchDocs(`doc:users:${user_id}:history:*`, query);
    if (results.length === 0) return { content: [{ type: "text", text: JSON.stringify({ status: "No matches found." }) }] };
    return { content: [{ type: "text", text: JSON.stringify({
      results: results.map(r => ({ date: r.key.split(":").pop(), match_preview: r.content.substring(0, 200) + "..." }))
    }) }] };
  });

  return server;
}

// ---------------------------------------------------------------------------
// SSE Transport
// ---------------------------------------------------------------------------

const transports = {};

app.get("/sse", async (req, res) => {
  const transport = new SSEServerTransport("/messages", res);
  const mcpServer = createMcpServerInstance();
  transports[transport.sessionId] = { transport, server: mcpServer };
  res.on("close", () => { mcpServer.close(); delete transports[transport.sessionId]; });
  await mcpServer.connect(transport);
});

app.post("/messages", async (req, res) => {
  const sessionId = req.query.sessionId;
  const entry = transports[sessionId];
  if (entry) await entry.transport.handlePostMessage(req, res);
  else res.status(400).json({ error: "No transport found for sessionId" });
});

// ---------------------------------------------------------------------------
// REST API (backwards compatibility)
// ---------------------------------------------------------------------------

const jsonParser = express.json();

app.get("/health", async (req, res) => {
  const redisOk = redis.isReady;
  let obsidianOk = false;
  try { await fs.access(OBSIDIAN_VAULT_PATH); obsidianOk = true; } catch {}
  res.json({
    status: (redisOk && obsidianOk) ? "healthy" : "degraded",
    redis: redisOk,
    obsidian: obsidianOk,
    vault_path: OBSIDIAN_VAULT_PATH,
    bm25_docs: bm25.size,
    uptime: process.uptime()
  });
});

app.get("/api/tools", (req, res) => {
  res.json({ tools: [
    "cache_set", "cache_get", "cache_delete", "redis_stats", "health_check",
    "obsidian_write_note", "obsidian_read_note", "obsidian_delete_note",
    "obsidian_search_notes", "obsidian_list_notes",
    "heurchain_search", "heurchain_export",
    "prometheus_get_targets", "prometheus_get_alerts", "prometheus_query",
    "grafana_get_health", "grafana_list_dashboards",
    "proxmox_get_cluster_status", "proxmox_list_nodes", "proxmox_find_vm",
    "ceph_get_health_status", "ceph_list_osd_notes",
    "network_search_docs", "network_get_runbook",
    "user_context_add_entry", "user_context_get_history", "user_context_search_history"
  ]});
});

app.post("/api/cache", jsonParser, async (req, res) => {
  const { key, value, ttl } = req.body;
  if (ttl) await redis.setEx(key, ttl, JSON.stringify(value));
  else await redis.set(key, JSON.stringify(value));
  res.json({ cached: true });
});

app.get("/api/cache/:key", async (req, res) => {
  const val = await redis.get(req.params.key);
  if (val) res.json({ hit: true, data: JSON.parse(val) });
  else res.json({ hit: false });
});

app.get("/api/stats", async (req, res) => {
  const info = await redis.info("stats");
  res.json({ redis_info: info, bm25_docs: bm25.size });
});

// --- Obsidian REST Endpoints ---

app.get("/api/obsidian/notes", async (req, res) => {
  const { key, prefix } = req.query;
  if (key) {
    const content = await readFromObsidian(key);
    if (content === null) return res.status(404).json({ found: false, key });
    return res.json({ found: true, key, content });
  }
  const notes = await listObsidian(prefix);
  res.json({ count: notes.length, notes });
});

app.post("/api/obsidian/notes", jsonParser, async (req, res) => {
  const { key, content, metadata } = req.body;
  if (!key || content === undefined) return res.status(400).json({ error: "key and content required" });
  try {
    const filePath = await saveToObsidian(key, content, metadata);
    res.json({ stored: true, key, path: filePath });
  } catch (e) {
    res.status(500).json({ stored: false, error: e.message });
  }
});

app.delete("/api/obsidian/notes", async (req, res) => {
  const { key } = req.query;
  if (!key) return res.status(400).json({ error: "key required" });
  const deleted = await deleteFromObsidian(key);
  res.json({ deleted, key });
});

app.get("/api/obsidian/search", async (req, res) => {
  const { q, limit } = req.query;
  if (!q) return res.status(400).json({ error: "q required" });
  const results = await searchObsidian(q);
  const max = parseInt(limit) || 10;
  res.json({
    count: results.length,
    results: results.slice(0, max).map(r => ({ key: r.key, path: r.path, preview: r.content.substring(0, 200) }))
  });
});

// ---------------------------------------------------------------------------
// Startup
// ---------------------------------------------------------------------------

(async () => {
  await redis.connect();
  await buildIndex();
  // Rebuild index hourly to catch external writes
  setInterval(buildIndex, 60 * 60 * 1000);
  const port = parseInt(process.env.MCP_PORT, 10) || 3010;
  app.listen(port, () => console.log(`HeurChain MCP v1.2.0 listening on port ${port} — ${bm25.size} docs indexed`));
})();
