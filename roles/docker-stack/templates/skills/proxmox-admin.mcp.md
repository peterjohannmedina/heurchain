---
name: proxmox
description: A skill to query and manage the Proxmox environment via Obsidian notes.
---

### Tools

#### `get_cluster_status()`
Searches Obsidian for a note titled "Proxmox Cluster Status" and returns its content. Assumes the note is tagged with `#proxmox` and `#summary`.

````js
async function get_cluster_status() {
  // This tool uses the built-in obsidian tool provided by the MCP server
  const results = await obsidian.search({ query: 'file:"Proxmox Cluster Status" tag:#proxmox tag:#summary' });
  if (!results || results.length === 0) {
    return { error: 'Status note not found. Ensure a note titled "Proxmox Cluster Status" with tags #proxmox and #summary exists.' };
  }
  const note_path = results[0].path;
  return await obsidian.get_note({ path: note_path });
}
````

#### `list_nodes()`
Finds all notes tagged `#proxmox-node` to list all nodes in the cluster.

````js
async function list_nodes() {
  const results = await obsidian.search({ query: 'tag:#proxmox-node' });
  if (!results || results.length === 0) {
    return { error: 'No Proxmox nodes found. Ensure each node has a note with the tag #proxmox-node.' };
  }
  return { nodes: results.map(r => ({ path: r.path, title: r.basename })) };
}
````

#### `find_vm(vm_id_or_name)`
Searches for a specific VM or container note by its ID or name.

````js
async function find_vm(vm_id_or_name) {
  if (!vm_id_or_name) {
    return { error: 'VM ID or name is required.' };
  }
  // Search for files that are tagged #vm or #ct AND contain the name/ID in the filename or content
  const query = `(tag:#vm OR tag:#ct) AND ("${vm_id_or_name}")`;
  const results = await obsidian.search({ query });
  if (!results || results.length === 0) {
    return { error: `No VM or CT found matching "${vm_id_or_name}".` };
  }
  // Return content of the first match
  const note_path = results[0].path;
  const content = await obsidian.get_note({ path: note_path });
  return { path: note_path, content };
}
````
