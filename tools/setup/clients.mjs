// Client detection, the recall-hook qualification list, and MCP registration
// through each client's own `mcp` command (which preserves unrelated entries).
import {readFile} from 'node:fs/promises';
import {join} from 'node:path';
import os from 'node:os';
import {spawnClient} from './winspawn.mjs';
import {commandLine,locations} from './install.mjs';

// Automatic recall is ON by default only for these exact client builds, each
// qualified against a local 0.6.1-oss.1 broker with tools/setup/native-check.mjs
// (receipts and method: docs/agent-setup.md). Every other build is opt-in with
// --recall-hook. Builds qualified against the hosted service are not listed:
// they were not run against this broker.
export const QUALIFIED={
  claude:[{version:'2.1.329',runtimes:['windows'],evidence:'native-check against local broker 0.6.1-oss.1, 2026-11-20'}],
  codex:[{version:'0.170.0',runtimes:['windows'],evidence:'native-check against local broker 0.6.1-oss.1, 2026-11-20'}],
};
export const NAMES={claude:'Claude Code CLI',codex:'Codex CLI'};
export const MCP_NAME={claude:'heurchain-local',codex:'heurchain_local'};

export function runtimeOf({platform=process.platform,release=os.release(),env=process.env}={}){
  if(platform==='win32')return 'windows';
  if(platform==='darwin')return 'macos';
  if(platform==='linux')return env.WSL_DISTRO_NAME||env.WSL_INTEROP||/microsoft/i.test(release)?'wsl':'linux';
  return 'unsupported';
}

export function recallDefault(client,version,runtime){
  const hit=(QUALIFIED[client]||[]).find(q=>q.version===version&&q.runtimes.includes(runtime));
  if(hit)return {enabled:true,reason:`${NAMES[client]} ${version} on ${runtime} is qualified against this broker build (${hit.evidence}).`};
  const list=(QUALIFIED[client]||[]).map(q=>q.version+' ('+q.runtimes.join(', ')+')').join(', ')||'none';
  return {enabled:false,reason:`${NAMES[client]} ${version||'(version unknown)'} on ${runtime} is not qualified for automatic recall (qualified: ${list}). Opt in with --recall-hook.`};
}

export function run(name,args,{env=process.env,input,timeout=30000}={}){
  return new Promise(resolve=>{
    let p;try{p=spawnClient(name,args,{env,stdio:[input===undefined?'ignore':'pipe','pipe','pipe']});}catch(e){resolve({code:-1,stdout:'',stderr:String(e.message)});return;}
    let out='',err='';const timer=setTimeout(()=>p.kill(),timeout);
    p.stdout?.on('data',d=>out+=d);p.stderr?.on('data',d=>err+=d);
    p.on('error',e=>{clearTimeout(timer);resolve({code:-1,stdout:out,stderr:String(e.message)});});
    p.on('close',code=>{clearTimeout(timer);resolve({code,stdout:out,stderr:err});});
    if(input!==undefined)p.stdin.end(input);
  });
}

const VERSION={claude:/^([0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?) \(Claude Code\)$/m,codex:/^codex-cli ([0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?)$/m};
export async function detect(client,{env=process.env,runImpl=run}={}){
  const r=await runImpl(client,['--version'],{env,timeout:15000});
  const version=r.code===0?String(r.stdout).trim().match(VERSION[client])?.[1]||null:null;
  return {client,status:version?'version-observed':r.code===-1?'not-found':'manual-version-check',version};
}

// The entry each client gets. Claude Code: Streamable HTTP with a headersHelper
// that prints the key header at connect time. Codex: the stdio bridge. Neither
// puts the key in a config file.
export function serverFor(client,{endpoint,home,env,node,platform}){
  const where=locations(client,{home,env});
  if(client==='claude')return {type:'http',url:endpoint,headersHelper:commandLine(where.bridgeScript,where.stateFile,{node,env,platform,extra:' --print-headers'})};
  return {command:node,args:[where.bridgeScript,'--state',where.stateFile]};
}

async function claudeEntry({home,env}){
  const file=env.CLAUDE_CONFIG_DIR?join(env.CLAUDE_CONFIG_DIR,'.claude.json'):join(home,'.claude.json');
  try{return JSON.parse(await readFile(file,'utf8'))?.mcpServers?.[MCP_NAME.claude]??null;}catch(e){if(e.code==='ENOENT')return null;throw Error('Could not read the Claude Code user configuration to compare the existing entry.');}
}
async function codexEntry({env,runImpl}){
  const r=await runImpl('codex',['mcp','get',MCP_NAME.codex,'--json'],{env});
  if(r.code!==0)return null;
  try{const j=JSON.parse(r.stdout);const t=j.transport||j;return t.command?{command:t.command,args:t.args||[]}:{url:t.url||null};}catch{return null;}
}
const same=(a,b)=>JSON.stringify(a)===JSON.stringify(b);

// Add or update the entry. An identical entry is left alone (repeat setup); a
// different one is removed and re-added (upgrade). Other entries are untouched.
export async function registerMcp(client,server,{apply=false,home=os.homedir(),env=process.env,runImpl=run}={}){
  const current=client==='claude'?await claudeEntry({home,env}):await codexEntry({env,runImpl});
  const wanted=client==='claude'?server:{command:server.command,args:server.args};
  const label=client==='claude'?'Claude Code user config (claude mcp add-json --scope user)':'$CODEX_HOME/config.toml (codex mcp add)';
  if(current&&same(current,wanted))return {file:label,changed:false};
  if(!apply)return {file:label,changed:true,applied:false,replaces:Boolean(current)};
  if(current){const r=await removeMcp(client,{apply:true,env,runImpl});if(!r.changed)throw Error('Could not remove the old '+MCP_NAME[client]+' entry.');}
  const r=client==='claude'
    ?await runImpl('claude',['mcp','add-json','--scope','user',MCP_NAME.claude,'-'],{env,input:JSON.stringify(server)})
    :await runImpl('codex',['mcp','add',MCP_NAME.codex,'--',server.command,...server.args],{env});
  if(r.code!==0)throw Error(`${client} mcp add failed: `+String(r.stderr||r.stdout).trim().slice(0,200));
  const after=client==='claude'?await claudeEntry({home,env}):await codexEntry({env,runImpl});
  if(!after||!same(after,wanted))throw Error('The client did not save the expected '+MCP_NAME[client]+' entry; review it with `'+client+' mcp get '+MCP_NAME[client]+'`.');
  return {file:label,changed:true,applied:true,replaced:Boolean(current)};
}

export async function removeMcp(client,{apply=false,home=os.homedir(),env=process.env,runImpl=run}={}){
  const current=client==='claude'?await claudeEntry({home,env}):await codexEntry({env,runImpl});
  if(!current)return {changed:false};
  if(!apply)return {changed:true,applied:false};
  const r=client==='claude'?await runImpl('claude',['mcp','remove','--scope','user',MCP_NAME.claude],{env}):await runImpl('codex',['mcp','remove',MCP_NAME.codex],{env});
  return {changed:r.code===0,applied:r.code===0};
}
