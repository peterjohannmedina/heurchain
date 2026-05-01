---
name: ceph
description: A skill for monitoring the Ceph cluster via Obsidian notes.
---

### Tools

#### `get_health_status()`
Retrieves the latest Ceph health status note from the Obsidian vault. Assumes a note is regularly updated with the output of `ceph -s`.

````js
async function get_health_status() {
  // This tool uses the built-in obsidian tool provided by the MCP server
  const results = await obsidian.search({ query: 'file:"Ceph Cluster Status" tag:#ceph' });
  if (!results || results.length === 0) {
    return { error: 'Ceph status note not found. Ensure a note titled "Ceph Cluster Status" with the tag #ceph is being updated.' };
  }
  const note_path = results[0].path;
  const note_content = await obsidian.get_note({ path: note_path });
  
  // Extract key metrics from the note content
  const health = note_content.match(/health: (\w+)/)?.[1];
  const osds = note_content.match(/osd: (\d+ osds: \d+ up, \d+ in)/)?.[1];
  const data = note_content.match(/data:\s+([^\n]+)/)?.[1];
  
  return {
    health_status: health || 'unknown',
    osd_status: osds || 'unknown',
    data_usage: data || 'unknown',
    full_report: note_content
  };
}
````

#### `list_osd_notes()`
Finds all notes related to specific OSDs (Object Storage Daemons).

````js
async function list_osd_notes() {
  const results = await obsidian.search({ query: 'tag:#ceph-osd' });
  if (!results || results.length === 0) {
    return { error: 'No OSD-specific notes found. Use the tag #ceph-osd for maintenance logs or status notes.' };
  }
  return { osd_notes: results.map(r => r.path) };
}
````
