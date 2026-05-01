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
const HEURCHAIN_URL = process.env.HEURCHAIN_URL || "http://host.docker.internal:3012";

(async () => {
  await redis.connect();
  const port = parseInt(process.env.MCP_PORT, 10) || 3010;
  app.listen(port, () => console.log("MCP server listening on port " + port));
})();

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
  try {
    return await fs.readFile(filePath, "utf-8");
  } catch (e) {
    if (e.code === "ENOENT") return null;
    throw e;
  }
}

async function deleteFromObsidian(key) {
  const filePath = keyToObsidianPath(key);
  try {
    await fs.unlink(filePath);
    return true;
  } catch (e) {
    if (e.code === "ENOENT") return false;
    throw e;
  }
}

async function searchObsidian(query) {
  const results = [];
  async function walk(dir) {
    const entries = await fs.readdir(dir, { withFileTypes: true });
    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        await walk(fullPath);
      } else if (entry.name.endsWith(".md")) {
        const content = await fs.readFile(fullPath, "utf-8");
        if (content.toLowerCase().includes(query.toLowerCase())) {
          const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
          const key = relative.replace(/\//g, ":").replace(/\.md$/, "");
          results.push({ key, content, path: relative });
        }
      }
    }
  }
  try {
    await walk(OBSIDIAN_VAULT_PATH);
  } catch (e) {
    if (e.code !== "ENOENT") throw e;
  }
  return results;
}

async function listObsidian(prefix) {
  const results = [];
  async function walk(dir) {
    const entries = await fs.readdir(dir, { withFileTypes: true });
    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        await walk(fullPath);
      } else if (entry.name.endsWith(".md")) {
        const relative = path.relative(OBSIDIAN_VAULT_PATH, fullPath);
        const key = relative.replace(/\//g, ":").replace(/\.md$/, "");
        if (!prefix || key.startsWith(prefix)) {
          results.push({ key, path: relative });
        }
      }
    }
  }
  try {
    await walk(OBSIDIAN_VAULT_PATH);
  } catch (e) {
    if (e.code !== "ENOENT") throw e;
  }
  return results;
}

// ---------------------------------------------------------------------------
// MCP Server Factory
// ---------------------------------------------------------------------------

function createMcpServerInstance() {
  const server = new McpServer({
    name: "second-brain-mcp",
    version: "1.1.0"
  });

  // --- Cache Tools ---

  server.tool("cache_set", "Store a key-value pair in Redis cache with optional TTL", {
    key: z.string().describe("Cache key"),
    value: z.string().describe("Value to cache"),
    ttl: z.number().optional().describe("Time-to-live in seconds")
  }, async ({ key, value, ttl }) => {
    if (ttl) await redis.setEx(key, ttl, value);
    else await redis.set(key, value);
    return { content: [{ type: "text", text: JSON.stringify({ cached: true, key }) }] };
  });

  server.tool("cache_get", "Retrieve a cached value from Redis by key", {
    key: z.string().describe("Cache key to retrieve")
  }, async ({ key }) => {
    const val = await redis.get(key);
    if (val) return { content: [{ type: "text", text: val }] };
    return { content: [{ type: "text", text: JSON.stringify({ hit: false }) }] };
  });

  server.tool("cache_delete", "Delete a cached key from Redis", {
    key: z.string().describe("Cache key to delete")
  }, async ({ key }) => {
    const deleted = await redis.del(key);
    return { content: [{ type: "text", text: JSON.stringify({ deleted: deleted > 0, key }) }] };
  });

  server.tool("redis_stats", "Get Redis server statistics and memory info", {}, async () => {
    const info = await redis.info("stats");
    return { content: [{ type: "text", text: info }] };
  });

  server.tool("health_check", "Check the health of the MCP server, Redis, and Obsidian vault", {}, async () => {
    const redisOk = redis.isReady;
    let obsidianOk = false;
    try {
      await fs.access(OBSIDIAN_VAULT_PATH);
      obsidianOk = true;
    } catch {}
    return {
      content: [{
        type: "text",
        text: JSON.stringify({
          status: (redisOk && obsidianOk) ? "healthy" : "degraded",
          redis: redisOk,
          obsidian: obsidianOk,
          vault_path: OBSIDIAN_VAULT_PATH,
          uptime: process.uptime()
        })
      }]
    };
  });

  // --- Obsidian Vault Tools (Long-Term MD Storage) ---

  server.tool("obsidian_write_note", "Write or overwrite a markdown note in the Obsidian vault for long-term storage", {
    key: z.string().describe("Note key (colons become folder separators, e.g., doc:proxmox:setup)"),
    content: z.string().describe("Markdown body"),
    metadata: z.record(z.string()).optional().describe("Optional frontmatter metadata (key-value pairs)")
  }, async ({ key, content, metadata }) => {
    try {
      const filePath = await saveToObsidian(key, content, metadata);
      return { content: [{ type: "text", text: JSON.stringify({ stored: true, key, path: filePath, tier: "reference" }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ stored: false, key, error: e.message }) }] };
    }
  });

  server.tool("obsidian_read_note", "Read a markdown note from the Obsidian vault by key", {
    key: z.string().describe("Note key")
  }, async ({ key }) => {
    try {
      const content = await readFromObsidian(key);
      if (content === null) {
        return { content: [{ type: "text", text: JSON.stringify({ found: false, key }) }] };
      }
      return { content: [{ type: "text", text: JSON.stringify({ found: true, key, content }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ found: false, key, error: e.message }) }] };
    }
  });

  server.tool("obsidian_delete_note", "Delete a markdown note from the Obsidian vault by key", {
    key: z.string().describe("Note key to delete")
  }, async ({ key }) => {
    try {
      const deleted = await deleteFromObsidian(key);
      return { content: [{ type: "text", text: JSON.stringify({ deleted, key }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ deleted: false, key, error: e.message }) }] };
    }
  });

  server.tool("obsidian_search_notes", "Search all markdown notes in the Obsidian vault by keyword", {
    query: z.string().describe("Search term"),
    limit: z.number().optional().describe("Max results to return").default(10)
  }, async ({ query, limit }) => {
    try {
      const results = await searchObsidian(query);
      const sliced = results.slice(0, limit);
      return { content: [{ type: "text", text: JSON.stringify({ count: results.length, results: sliced.map(r => ({ key: r.key, preview: r.content.substring(0, 200) })) }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  server.tool("obsidian_list_notes", "List all markdown notes in the Obsidian vault, optionally filtered by prefix", {
    prefix: z.string().optional().describe("Key prefix filter, e.g., doc:proxmox:")
  }, async ({ prefix }) => {
    try {
      const results = await listObsidian(prefix);
      return { content: [{ type: "text", text: JSON.stringify({ count: results.length, notes: results }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- HeurChain BM25 Search ---

  server.tool("heurchain_search", "Search the full knowledge base using HeurChain's BM25 ranked index (preferred over obsidian_search_notes — covers all 460+ Redis keys with relevance scoring)", {
    query: z.string().describe("Search query"),
    limit: z.number().optional().describe("Max results (default 10, max 50)").default(10),
    tier: z.string().optional().describe("'all' | 'longterm' | 'working' (default: all)").default("all")
  }, async ({ query, limit, tier }) => {
    try {
      const url = `${HEURCHAIN_URL}/search?q=${encodeURIComponent(query)}&limit=${limit}&tier=${tier}`;
      const response = await fetch(url);
      if (!response.ok) {
        return { content: [{ type: "text", text: JSON.stringify({ error: `HeurChain returned HTTP ${response.status}` }) }] };
      }
      const results = await response.json();
      return { content: [{ type: "text", text: JSON.stringify({ count: results.length, results: results.map(r => ({ key: r.key, score: r.score, tier: r.tier, preview: (r.content || "").substring(0, 300), updated_at: r.updated_at })) }) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  // --- Prometheus Monitoring Tools ---

  server.tool("prometheus_get_targets", "Returns the status of all targets Prometheus is currently scraping", {}, async () => {
    try {
      const response = await fetch("http://prometheus:9090/api/v1/targets");
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const data = await response.json();
      const targets = data.data.activeTargets.map(t => ({
        job: t.labels.job,
        instance: t.labels.instance,
        health: t.health,
        last_scrape: t.lastScrape
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
      const response = await fetch("http://grafana:3000/api/health", {
        headers: { "Authorization": grafanaAuth }
      });
      if (!response.ok) return { content: [{ type: "text", text: JSON.stringify({ error: `HTTP ${response.status}` }) }] };
      const data = await response.json();
      return { content: [{ type: "text", text: JSON.stringify(data) }] };
    } catch (e) {
      return { content: [{ type: "text", text: JSON.stringify({ error: e.message }) }] };
    }
  });

  server.tool("grafana_list_dashboards", "List all available dashboards in Grafana", {}, async () => {
    try {
      const response = await fetch("http://grafana:3000/api/search?type=dash-db", {
        headers: { "Authorization": grafanaAuth }
      });
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
    for await (const key of redis.scanIterator({ MATCH: keyPattern, COUNT: 100 })) {
      keys.push(key);
    }
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
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "No cluster status document found. Store one with key 'doc:proxmox:cluster-status' using cache_set." }) }] };
    }
    return { content: [{ type: "text", text: results[0].content }] };
  });

  server.tool("proxmox_list_nodes", "List all Proxmox nodes documented in the knowledge base", {}, async () => {
    const results = await searchDocs("doc:proxmox:node:*", null);
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "No node documents found. Store them with keys like 'doc:proxmox:node:pver430' using cache_set." }) }] };
    }
    return { content: [{ type: "text", text: JSON.stringify({ nodes: results.map(r => ({ key: r.key, title: r.key.split(":").pop() })) }) }] };
  });

  server.tool("proxmox_find_vm", "Search for a specific VM or container by ID or name in the knowledge base", {
    vm_id_or_name: z.string().describe("VM ID or name to search for")
  }, async ({ vm_id_or_name }) => {
    let results = await searchDocs("doc:proxmox:vm:*", vm_id_or_name);
    if (results.length === 0) results = await searchDocs("doc:proxmox:ct:*", vm_id_or_name);
    if (results.length === 0) results = await searchDocs("doc:proxmox:*", vm_id_or_name);
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ error: `No VM or CT found matching "${vm_id_or_name}".` }) }] };
    }
    return { content: [{ type: "text", text: results[0].content }] };
  });

  // --- Ceph Documentation Tools (Redis-backed) ---

  server.tool("ceph_get_health_status", "Retrieve the latest Ceph cluster health status from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:ceph:cluster-status*", null);
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "No Ceph status document found. Store one with key 'doc:ceph:cluster-status' using cache_set." }) }] };
    }
    return { content: [{ type: "text", text: results[0].content }] };
  });

  server.tool("ceph_list_osd_notes", "List all Ceph OSD documentation notes from the knowledge base", {}, async () => {
    const results = await searchDocs("doc:ceph:osd:*", null);
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ error: "No OSD documents found. Store them with keys like 'doc:ceph:osd:0' using cache_set." }) }] };
    }
    return { content: [{ type: "text", text: JSON.stringify({ osd_notes: results.map(r => r.key) }) }] };
  });

  // --- Network Documentation Tools (Redis-backed) ---

  server.tool("network_search_docs", "Search across network and infrastructure documentation in the knowledge base", {
    search_query: z.string().describe("Search query to find relevant documentation")
  }, async ({ search_query }) => {
    const results = await searchDocs("doc:network:*", search_query);
    const infraResults = await searchDocs("doc:infrastructure:*", search_query);
    const all = [...results, ...infraResults];
    if (all.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ status: "No relevant documents found." }) }] };
    }
    return { content: [{ type: "text", text: JSON.stringify({ results: all.slice(0, 5).map(r => ({ key: r.key, preview: r.content.substring(0, 200) })) }) }] };
  });

  server.tool("network_get_runbook", "Retrieve a specific network runbook by topic from the knowledge base", {
    topic: z.string().describe("Runbook topic to search for")
  }, async ({ topic }) => {
    const results = await searchDocs("doc:runbook:*", topic);
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ error: `No runbook found for topic: "${topic}". Store runbooks with keys like 'doc:runbook:topic-name' using cache_set.` }) }] };
    }
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
    await redis.setEx(key, USER_HISTORY_TTL, existing + entry);
    return { content: [{ type: "text", text: JSON.stringify({ success: true, key, entry_added: entry }) }] };
  });

  server.tool("user_context_get_history", "Retrieve the full interaction history for a user on a specific date", {
    user_id: z.string().describe("User identifier"),
    date: z.string().describe("Date in YYYY-MM-DD format")
  }, async ({ user_id, date }) => {
    const key = `doc:users:${user_id}:history:${date}`;
    const val = await redis.get(key);
    if (!val) {
      return { content: [{ type: "text", text: JSON.stringify({ status: "Not Found", message: `No history found for user '${user_id}' on date '${date}'.` }) }] };
    }
    return { content: [{ type: "text", text: JSON.stringify({ user_id, date, history: val }) }] };
  });

  server.tool("user_context_search_history", "Search across all of a user's history documents by keyword", {
    user_id: z.string().describe("User identifier"),
    query: z.string().describe("Search query")
  }, async ({ user_id, query }) => {
    const results = await searchDocs(`doc:users:${user_id}:history:*`, query);
    if (results.length === 0) {
      return { content: [{ type: "text", text: JSON.stringify({ status: "No matches found." }) }] };
    }
    return { content: [{ type: "text", text: JSON.stringify({
      results: results.map(r => ({
        date: r.key.split(":").pop(),
        match_preview: r.content.substring(0, 200) + "..."
      }))
    }) }] };
  });

  return server;
}

// SSE transport management
const transports = {};

app.get("/sse", async (req, res) => {
  const transport = new SSEServerTransport("/messages", res);
  const mcpServer = createMcpServerInstance();
  transports[transport.sessionId] = { transport, server: mcpServer };
  res.on("close", () => {
    mcpServer.close();
    delete transports[transport.sessionId];
  });
  await mcpServer.connect(transport);
});

app.post("/messages", async (req, res) => {
  const sessionId = req.query.sessionId;
  const entry = transports[sessionId];
  if (entry) {
    await entry.transport.handlePostMessage(req, res);
  } else {
    res.status(400).json({ error: "No transport found for sessionId" });
  }
});

// --- REST API (backwards compatibility) ---
const jsonParser = express.json();

app.get("/health", async (req, res) => {
  const redisOk = redis.isReady;
  let obsidianOk = false;
  try {
    await fs.access(OBSIDIAN_VAULT_PATH);
    obsidianOk = true;
  } catch {}
  res.json({
    status: (redisOk && obsidianOk) ? "healthy" : "degraded",
    redis: redisOk,
    obsidian: obsidianOk,
    vault_path: OBSIDIAN_VAULT_PATH,
    uptime: process.uptime()
  });
});

app.get("/api/tools", (req, res) => {
  res.json({ tools: [
    "cache_set", "cache_get", "cache_delete", "redis_stats", "health_check",
    "obsidian_write_note", "obsidian_read_note", "obsidian_delete_note",
    "obsidian_search_notes", "obsidian_list_notes",
    "heurchain_search",
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
  res.json({ redis_info: info });
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
    results: results.slice(0, max).map(r => ({
      key: r.key,
      path: r.path,
      preview: r.content.substring(0, 200)
    }))
  });
});

