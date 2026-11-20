#!/usr/bin/env node
// HeurChain stdio-to-HTTP MCP bridge, and the Claude Code headersHelper
// (--print-headers), for a self-hosted broker.
//
// Canonical source: tools/setup/mcp-bridge.mjs. Setup copies this file into
// ~/.heurchain/<client>/ so the API key never goes into a client config file and
// a client started before setup (or from a desktop launcher) still finds it.
// Node built-ins only.
//
// Contract:
//   stdin  : newline-delimited JSON-RPC messages from the MCP client.
//   stdout : one line per JSON-RPC response the broker returns. Notifications
//            (no id) get no output, as MCP stdio requires.
//   state  : --state <file>, the secret-free JSON written by setup
//            ({endpoint, tenant_id, variable}).
// The key comes from the environment variable named in the state file, or where
// setup stored it: the Windows user environment (HKCU\Environment) or
// ~/.heurchain/credentials.env. It is never logged or written anywhere.
import {readFileSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {homedir} from 'node:os';
import {join} from 'node:path';
import {createInterface} from 'node:readline';

export const BRIDGE_VERSION = '1.0.0-oss.1';
const VARIABLE = /^HEURCHAIN_LOCAL_[A-Z0-9]{2,24}_KEY$/;

export function storedKey(variable, {env = process.env, platform = process.platform, home = homedir(), run = spawnSync} = {}) {
  if (!VARIABLE.test(String(variable))) return '';
  if (env[variable]) return env[variable];
  try {
    if (platform === 'win32') {
      const r = run('reg', ['query', 'HKCU\\Environment', '/v', variable], {encoding: 'utf8', windowsHide: true, timeout: 3000});
      const m = String(r.stdout || '').match(new RegExp('^\\s*' + variable + '\\s+REG_(?:EXPAND_)?SZ\\s+(\\S+)\\s*$', 'm'));
      return m ? m[1] : '';
    }
    const line = readFileSync(join(home, '.heurchain', 'credentials.env'), 'utf8').split('\n').find(l => l.startsWith('export ' + variable + '='));
    const m = line && line.match(/='([^']*)'$/);
    return m ? m[1] : '';
  } catch { return ''; }
}

// Headers for the Claude Code headersHelper. The 0.6.1-oss.1 broker
// authenticates with X-HeurChain-API-Key plus X-HeurChain-Tenant. Without a
// stored key only the tenant header is sent, so the broker answers a plain 401.
export function headersFor(state, key) {
  return key ? {'X-HeurChain-API-Key': key, 'X-HeurChain-Tenant': state.tenant_id} : {'X-HeurChain-Tenant': state.tenant_id};
}

// A broker reply is JSON, or SSE whose data lines each carry one JSON-RPC message.
export function parseReply(text, contentType) {
  if (!text || !text.trim()) return [];
  if (/text\/event-stream/i.test(contentType || '')) {
    return text.split(/\r?\n/).filter(l => l.startsWith('data:')).map(l => l.slice(5).trim()).filter(Boolean).map(d => JSON.parse(d));
  }
  const value = JSON.parse(text);
  return Array.isArray(value) ? value : [value];
}

const errorFor = (message, text) => (message && message.id !== undefined && message.id !== null)
  ? [{jsonrpc: '2.0', id: message.id, error: {code: -32000, message: text}}] : [];

export async function forward(line, {state, key, fetchImpl = globalThis.fetch, protocolVersion}) {
  let message;
  try { message = JSON.parse(line); } catch { return [{jsonrpc: '2.0', id: null, error: {code: -32700, message: 'Parse error'}}]; }
  if (!key) return errorFor(message, 'HeurChain key not found. Restart the client after setup, or run setup again.');
  const headers = {
    'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream',
    'User-Agent': 'heurchain-mcp-bridge/' + BRIDGE_VERSION,
    'X-HeurChain-API-Key': key, 'X-HeurChain-Tenant': state.tenant_id,
  };
  if (protocolVersion) headers['MCP-Protocol-Version'] = protocolVersion;
  let response;
  try {
    response = await fetchImpl(state.endpoint, {method: 'POST', headers, body: JSON.stringify(message), signal: AbortSignal.timeout(60000)});
  } catch (error) {
    return errorFor(message, 'HeurChain broker is unreachable: ' + String(error && error.message).slice(0, 120));
  }
  const text = await response.text();
  if (!response.ok) return errorFor(message, 'HeurChain broker answered HTTP ' + response.status + (response.status === 401 ? ': the key was revoked or is not set' : ''));
  try { return parseReply(text, response.headers.get('content-type')); }
  catch { return errorFor(message, 'Unreadable reply from the HeurChain broker'); }
}

async function main() {
  const at = process.argv.indexOf('--state');
  const raw = at > 0 ? process.argv[at + 1] : '';
  const state = JSON.parse(readFileSync(raw.startsWith('~') ? join(homedir(), raw.slice(1)) : raw, 'utf8'));
  if (!/^https?:\/\//.test(String(state.endpoint || ''))) throw Error('State file has no broker endpoint.');
  const key = storedKey(state.variable);
  if (process.argv.includes('--print-headers')) {
    process.stdout.write(JSON.stringify(headersFor(state, key)));
    return;
  }
  let protocolVersion = '';
  // Requests are forwarded in order, one at a time, so replies keep request order.
  let queue = Promise.resolve();
  const lines = createInterface({input: process.stdin, crlfDelay: Infinity});
  lines.on('line', line => {
    if (!line.trim()) return;
    queue = queue.then(async () => {
      const replies = await forward(line, {state, key, protocolVersion});
      for (const reply of replies) {
        const negotiated = reply && reply.result && reply.result.protocolVersion;
        if (typeof negotiated === 'string') protocolVersion = negotiated;
        process.stdout.write(JSON.stringify(reply) + '\n');
      }
    });
  });
  lines.on('close', () => { queue.then(() => process.exit(0)); });
}

if (process.argv[1] && /mcp-bridge\.mjs$/.test(process.argv[1].replaceAll('\\', '/'))) main().catch(error => {
  process.stderr.write('heurchain-mcp-bridge: ' + String(error && error.message).slice(0, 200) + '\n');
  process.exit(1);
});
