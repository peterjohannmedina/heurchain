---
name: searxng
description: A skill for web search and URL content retrieval via the self-hosted SearXNG MCP server.
---

## Overview

SearXNG MCP provides privacy-focused web search capabilities through a self-hosted SearXNG instance. The search engine runs on CT 210 (searx-node) at 192.168.1.210:8080 on pver430 (MediNAS, node 233). The MCP bridge (`mcp-searxng` npm package v0.9.1) runs locally on the workstation via `npx` and connects to the SearXNG instance.

### Infrastructure

| Component | Location | Address | Notes |
|-----------|----------|---------|-------|
| SearXNG engine | CT 210 on pver430 | 192.168.1.210:8080 | Docker: searxng + searxng-redis |
| MCP bridge | Local workstation | npx mcp-searxng | Configured in ~/.mcp.json |

### MCP Configuration

In `~/.mcp.json`:
```json
{
  "searxng": {
    "command": "npx",
    "args": ["-y", "mcp-searxng"],
    "env": {
      "SEARXNG_URL": "http://192.168.1.210:8080"
    }
  }
}
```

Optional environment variables for protected instances:
- `AUTH_USERNAME` / `AUTH_PASSWORD` — HTTP Basic Auth
- `USER_AGENT` — Custom User-Agent header
- `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` — Proxy settings

### Tools

#### `searxng_web_search(query, pageno?, time_range?, language?, safesearch?)`
Performs a web search using the local SearXNG instance. Aggregates results from 50+ engines (Google, DuckDuckGo, Brave, StartPage, Wikipedia, etc.).

````js
async function searxng_web_search(query, pageno, time_range, language, safesearch) {
  if (!query) {
    return { error: 'A search query is required.' };
  }

  const results = await searxng.web_search({
    query,
    pageno: pageno || 1,         // Page number (starts at 1)
    time_range: time_range,       // "day", "month", or "year"
    language: language || "all",  // Language code (e.g., "en", "fr")
    safesearch: safesearch || 0   // 0: None, 1: Moderate, 2: Strict
  });

  if (!results || results.length === 0) {
    return { status: 'No search results found.' };
  }

  return {
    results: results.map(r => ({
      title: r.title,
      url: r.url,
      content: r.content
    }))
  };
}
````

#### `web_url_read(url, startChar?, maxLength?, paragraphRange?, section?, readHeadings?)`
Fetches a web page and converts its content to markdown for analysis.

````js
async function web_url_read(url, options) {
  if (!url) {
    return { error: 'A URL is required.' };
  }

  const content = await searxng.url_read({
    url,
    startChar: options?.startChar || 0,       // Starting character position
    maxLength: options?.maxLength,             // Max characters to return
    paragraphRange: options?.paragraphRange,   // e.g., "1-5", "3", "10-"
    section: options?.section,                 // Extract under specific heading
    readHeadings: options?.readHeadings        // Return only headings list
  });

  if (!content) {
    return { error: `Failed to fetch content from ${url}.` };
  }

  return { url, content };
}
````

### Supported Search Categories

general, videos, social media, images, music, packages, news, and more.

### Fallback (Direct API)

If the MCP bridge is unavailable, query SearXNG directly:
```bash
curl -s "http://192.168.1.210:8080/search?q=QUERY&format=json&engines=google,brave,duckduckgo"
```

### Diagnostics

```bash
# Check SearXNG instance health
curl -s http://192.168.1.210:8080/config | jq .instance_name

# Check from Proxmox host
ssh root@192.168.1.233 'pct exec 210 -- docker ps'
```
