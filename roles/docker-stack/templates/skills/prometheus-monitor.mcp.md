---
name: prometheus
description: A skill to interact with the Prometheus monitoring server API.
---

### Tools

#### `get_targets()`
Returns the status of all targets Prometheus is currently scraping.

````js
async function get_targets() {
  const url = 'http://prometheus:9090/api/v1/targets';
  try {
    const response = await fetch(url);
    if (!response.ok) {
      return { error: `HTTP error! status: ${response.status}` };
    }
    const data = await response.json();
    
    // Simplify the output for clarity
    const targets = data.data.activeTargets.map(t => ({
      job: t.labels.job,
      instance: t.labels.instance,
      health: t.health,
      last_scrape: t.lastScrape
    }));
    
    return { targets };
  } catch (e) {
    return { error: e.message };
  }
}
````

#### `get_alerts()`
Returns any currently firing or pending alerts.

````js
async function get_alerts() {
  const url = 'http://prometheus:9090/api/v1/alerts';
  try {
    const response = await fetch(url);
    if (!response.ok) {
      return { error: `HTTP error! status: ${response.status}` };
    }
    const data = await response.json();
    return { alerts: data.data.alerts };
  } catch (e) {
    return { error: e.message };
  }
}
````

#### `query(promql_query)`
Executes a PromQL query.

````js
async function query(promql_query) {
  if (!promql_query) {
    return { error: 'A PromQL query string is required.' };
  }
  const url = `http://prometheus:9090/api/v1/query?query=${encodeURIComponent(promql_query)}`;
  try {
    const response = await fetch(url);
    if (!response.ok) {
      return { error: `HTTP error! status: ${response.status}` };
    }
    const data = await response.json();
    return data.data;
  } catch (e) {
    return { error: e.message };
  }
}
````
