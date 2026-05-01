---
name: network
description: A skill for semantic search across network documentation in Obsidian.
---

### Tools

#### `search_docs(search_query)`
Performs a semantic search across all notes in the 'infrastructure/' and 'shared/' vault paths. This is more powerful than a keyword search.

````js
async function search_docs(search_query) {
  if (!search_query) {
    return { error: 'A search query is required.' };
  }
  
  // This tool uses the built-in obsidian tool provided by the MCP server
  // It assumes a semantic search capability is enabled.
  const results = await obsidian.search({
    query: search_query,
    // Limit search to specific folders for relevance and performance
    filters: [
      { type: 'path', value: 'infrastructure/' },
      { type: 'path', value: 'shared/' }
    ]
  });
  
  if (!results || results.length === 0) {
    return { status: 'No relevant documents found.' };
  }
  
  // Return simplified results
  return { results: results.slice(0, 5).map(r => ({ path: r.path, score: r.score, title: r.basename })) };
}
````

#### `get_runbook(topic)`
Finds a specific runbook by searching for notes tagged `#runbook` that match the topic.

````js
async function get_runbook(topic) {
  if (!topic) {
    return { error: 'A runbook topic is required.' };
  }
  const query = `tag:#runbook AND ("${topic}")`;
  const results = await obsidian.search({ query });

  if (!results || results.length === 0) {
    return { error: `No runbook found for topic: "${topic}".` };
  }

  const note_path = results[0].path;
  const content = await obsidian.get_note({ path: note_path });
  return { path: note_path, content };
}
````
