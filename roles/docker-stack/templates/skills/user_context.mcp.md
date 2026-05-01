---
name: user_context
description: Manages user-specific history and context within the knowledge base.
---

### Tools

#### `add_entry(user_id, content)`
Adds a new entry to a user's daily history log. It appends the content with a timestamp to the document for the current date.

````js
async function add_entry(user_id, content) {
  if (!user_id || !content) {
    return { error: 'user_id and content are required.' };
  }
  
  const today = new Date().toISOString().split('T')[0]; // YYYY-MM-DD
  const key = `doc:users:${user_id}:history:${today}`;
  
  // Get existing content to append, or start a new file
  let existing_content = await cache_get(key) || `## User History for ${user_id} - ${today}\n\n`;
  if (typeof existing_content !== 'string') {
    existing_content = `## User History for ${user_id} - ${today}\n\n`;
  }
  
  // Append new entry with a timestamp
  const timestamp = new Date().toLocaleTimeString();
  const new_entry = `**[${timestamp}]**\n${content}\n\n---\n\n`;
  const updated_content = existing_content + new_entry;
  
  // Save it back to Redis (with a long TTL, e.g., 90 days)
  const ttl_seconds = 90 * 24 * 60 * 60;
  await cache_set(key, updated_content, ttl_seconds);
  
  return { success: true, key: key, entry_added: new_entry };
}
````

#### `get_history(user_id, date)`
Retrieves the full history log for a specific user on a given date (YYYY-MM-DD).

````js
async function get_history(user_id, date) {
  if (!user_id || !date) {
    return { error: 'user_id and date (YYYY-MM-DD) are required.' };
  }
  
  const key = `doc:users:${user_id}:history:${date}`;
  const history_content = await cache_get(key);
  
  if (!history_content) {
    return { status: 'Not Found', message: `No history found for user '${user_id}' on date '${date}'.` };
  }
  
  return { user_id, date, history: history_content };
}
````

#### `search_history(user_id, query)`
Performs a semantic search across all of a user's history documents.

````js
async function search_history(user_id, query) {
  if (!user_id || !query) {
    return { error: 'user_id and query are required.' };
  }
  
  // This is a placeholder for true semantic search.
  // The current Redis implementation doesn't support semantic search out-of-the-box.
  // This simulates it by searching document content for a keyword match.
  // To make this real, the Redis cache layer would need to be enhanced with vector embeddings.
  
  // For now, we'll list all history files and search their content.
  // NOTE: This is inefficient and should be replaced with a proper search index.
  const all_docs = await redis_search_keys(`doc:users:${user_id}:history:*`);
  
  const results = [];
  for (const key of all_docs) {
    const content = await cache_get(key);
    if (content && content.toLowerCase().includes(query.toLowerCase())) {
      const date = key.split(':').pop();
      results.push({
        date: date,
        match_preview: content.substring(0, 200) + '...'
      });
    }
  }
  
  if (results.length === 0) {
    return { status: 'No matches found.' };
  }
  
  return { results };
}
````
