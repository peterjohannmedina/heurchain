---
name: grafana
description: A skill to interact with the Grafana monitoring server API.
---

### Tools

#### `get_health()`
Checks the health status of the Grafana server.

````js
async function get_health() {
  const url = 'http://grafana:3000/api/health';
  const authHeader = 'Basic YWRtaW46U2VjdXJlUGFzczEyMyE=';
  try {
    const response = await fetch(url, {
      headers: { 'Authorization': authHeader }
    });
    if (!response.ok) {
      return { error: `HTTP error! status: ${response.status}` };
    }
    return await response.json();
  } catch (e) {
    return { error: e.message };
  }
}
````

#### `list_dashboards()`
Lists all available dashboards in Grafana.

````js
async function list_dashboards() {
  const url = 'http://grafana:3000/api/search?type=dash-db';
  const authHeader = 'Basic YWRtaW46U2VjdXJlUGFzczEyMyE=';
  try {
    const response = await fetch(url, {
      headers: { 'Authorization': authHeader }
    });
    if (!response.ok) {
      return { error: `HTTP error! status: ${response.status}` };
    }
    const dashboards = await response.json();
    // Simplify output
    return { dashboards: dashboards.map(d => ({ title: d.title, url: d.url })) };
  } catch (e) {
    return { error: e.message };
  }
}
````
