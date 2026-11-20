#!/usr/bin/env node
// One local flow for a self-hosted HeurChain broker: MCP entry, heurchain-memory
// skill and automatic recall hook for Claude Code CLI or Codex CLI.
// Node built-ins only. Dry run unless --apply. See docs/agent-setup.md.
import {readFile} from 'node:fs/promises';
import os from 'node:os';
import {rollback} from './configure.mjs';
import {CLIENTS,installPieces,removePieces,clientState,locations} from './install.mjs';
import {detect,recallDefault,runtimeOf,serverFor,registerMcp,removeMcp,QUALIFIED,NAMES,MCP_NAME} from './clients.mjs';
import {persistKey,removeKey,hasKey,variableFor,KEY} from './credentials.mjs';

export const SETUP_VERSION='0.1.0-oss.1';
const help=`HeurChain self-hosted agent setup ${SETUP_VERSION} (Node 20 or later)

  node tools/setup/cli.mjs connect --client claude|codex --tenant TENANT_ID
        [--url http://127.0.0.1:8080] [--key-stdin] [--recall-hook|--no-recall-hook]
        [--no-skill] [--client-version V] [--apply]
      Verifies the broker and key (health, MCP initialize, tools/list; no writes),
      stores the key for this client, adds the MCP entry, installs the skill and,
      for qualified client builds, registers the automatic recall hook.
      The key comes from HEURCHAIN_API_KEY or, with --key-stdin, one line on stdin.
      A repeat run reuses the stored key. Without --apply only the plan is shown.
  node tools/setup/cli.mjs uninstall --client claude|codex [--keep-key] [--apply]
  node tools/setup/cli.mjs status --client claude|codex
  node tools/setup/cli.mjs rollback --file FILE --backup BACKUP
  node tools/setup/cli.mjs --version

Automatic recall is ON by default only for client builds qualified against this
broker build (${Object.entries(QUALIFIED).map(([c,l])=>NAMES[c]+' '+l.map(q=>q.version+' on '+q.runtimes.join('/')).join(', ')).join('; ')}).
Off switches: --no-recall-hook (also unregisters an earlier hook), "enabled": false in
the client state file, or HEURCHAIN_RECALL_HOOK=off in the client's environment.
Review the hook in the client's /hooks; Codex runs it only after you trust it there.
`;
export const disclosure='Automatic recall: before a prompt that looks like it depends on earlier work, a local hook sends up to 300 characters of the prompt (obvious secrets removed) to heurchain_search on your broker and adds up to 3 matching excerpts, labelled as hook-initiated, to that prompt. Self-contained prompts are not sent.';

function fail(code,message){const e=new Error(message);e.safeCode=code;throw e;}
const FLAGS=['--apply','--key-stdin','--recall-hook','--no-recall-hook','--no-skill','--keep-key'];
const VALUES=['--client','--tenant','--url','--client-version','--file','--backup'];
const ALLOWED={connect:['--client','--tenant','--url','--key-stdin','--recall-hook','--no-recall-hook','--no-skill','--client-version','--apply'],uninstall:['--client','--keep-key','--apply'],status:['--client'],rollback:['--file','--backup']};
export function parse(args){
  const command=args[0],options={};
  for(let i=1;i<args.length;i++){
    const name=args[i];
    if(![...FLAGS,...VALUES].includes(name)||Object.hasOwn(options,name))fail('E_ARGUMENTS','Unknown or duplicate option: '+name);
    if(FLAGS.includes(name)){options[name]=true;continue;}
    if(args[i+1]===undefined||args[i+1].startsWith('--'))fail('E_ARGUMENTS','Missing value for '+name);
    options[name]=args[++i];
  }
  if(!ALLOWED[command])fail('E_ARGUMENTS','Unknown command. Run with --help.');
  const bad=Object.keys(options).find(k=>!ALLOWED[command].includes(k));if(bad)fail('E_ARGUMENTS',bad+' is not an option of '+command);
  if(options['--client']!==undefined&&!CLIENTS.includes(options['--client']))fail('E_ARGUMENTS','--client must be claude (Claude Code CLI) or codex (Codex CLI). Other clients: see docs/agent-setup.md.');
  if(options['--recall-hook']&&options['--no-recall-hook'])fail('E_ARGUMENTS','Choose --recall-hook or --no-recall-hook, not both.');
  if(options['--client-version']!==undefined&&!/^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.]+)?$/.test(options['--client-version']))fail('E_ARGUMENTS','Client version must look like 2.1.329 or 0.170.0.');
  if(options['--tenant']!==undefined&&!/^[A-Za-z0-9-]{8,64}$/.test(options['--tenant']))fail('E_ARGUMENTS','Tenant ID must be the tenant_id returned by POST /admin/tenants.');
  return {command,options};
}

export function endpointFor(base){
  let url;try{url=new URL(base);}catch{fail('E_ARGUMENTS','--url must be the broker base URL, for example http://127.0.0.1:8080');}
  if(!/^https?:$/.test(url.protocol)||url.username||url.password||url.search||url.hash)fail('E_ARGUMENTS','--url must be a plain http(s) base URL without credentials or query.');
  const loopback=['127.0.0.1','localhost','[::1]'].includes(url.hostname);
  if(url.protocol==='http:'&&!loopback)fail('E_ARGUMENTS','Use https for a broker that is not on this machine (docs/install.md#exposing-it-beyond-localhost).');
  return {base:url.origin,endpoint:url.origin+'/mcp/sse'};
}

// Read-only checks before anything changes: the broker answers, the key and
// tenant are accepted, and the tools setup relies on are exposed.
export async function preflight({base,endpoint,tenantId,key,fetchImpl=fetch}){
  const health=await fetchImpl(base+'/health',{signal:AbortSignal.timeout(5000)}).then(async r=>({status:r.status,body:await r.json().catch(()=>({}))})).catch(e=>({status:0,error:e.message}));
  if(health.status!==200)fail('E_BROKER','The broker did not answer GET /health at '+base+'.');
  const headers={'Content-Type':'application/json','Accept':'application/json, text/event-stream','X-HeurChain-API-Key':key,'X-HeurChain-Tenant':tenantId};
  const rpc=async(id,method,params)=>{
    const r=await fetchImpl(endpoint,{method:'POST',headers,body:JSON.stringify({jsonrpc:'2.0',id,method,params}),signal:AbortSignal.timeout(10000)});
    const text=await r.text();
    if(r.status===401||r.status===403)fail('E_AUTH','The broker refused this key and tenant (HTTP '+r.status+'). Check the tenant ID and key; nothing was changed.');
    if(!r.ok)fail('E_BROKER','MCP '+method+' answered HTTP '+r.status+'.');
    const data=/text\/event-stream/.test(r.headers.get('content-type')||'')?text.split(/\r?\n/).filter(l=>l.startsWith('data:')).map(l=>l.slice(5).trim()).pop():text;
    const j=JSON.parse(data);if(j.error)fail('E_BROKER','MCP '+method+' failed: '+String(j.error.message).slice(0,120));return j.result;
  };
  const init=await rpc(1,'initialize',{protocolVersion:'2025-06-18',capabilities:{},clientInfo:{name:'heurchain-setup',version:SETUP_VERSION}});
  const tools=((await rpc(2,'tools/list',{}))?.tools||[]).map(t=>t.name);
  for(const needed of ['heurchain_search','heurchain_store'])if(!tools.includes(needed))fail('E_BROKER','The broker does not expose '+needed+'.');
  return {health:{version:health.body.version||null,instance:health.body.instance_id||health.body.instance||null},server:init?.serverInfo||null,protocol:init?.protocolVersion||null,tools:tools.length,tool_names:tools};
}

async function readKey(options,env){
  if(options['--key-stdin']){let raw='';for await(const c of process.stdin){raw+=c;if(raw.length>4096)break;}return raw.split(/\r?\n/)[0].trim();}
  return String(env.HEURCHAIN_API_KEY||'').trim();
}

// keyStore: injectable for tools/setup/native-check.mjs, which keeps its
// synthetic key in the disposable profile instead of the user environment.
export async function main(argv,{env=process.env,home=os.homedir(),platform=process.platform,node=process.execPath,out=console.log,keyStore={persistKey,removeKey,hasKey}}={}){
  const {persistKey:persist,removeKey:unstore,hasKey:has}=keyStore;
  if(!argv.length||['--help','-h'].includes(argv[0])){out(help);return;}
  if(argv[0]==='--version'){out(SETUP_VERSION);return;}
  if(Number(process.versions.node.split('.')[0])<20)fail('E_NODE_VERSION','Node 20 or later is required.');
  const {command,options}=parse(argv),client=options['--client'],runtime=runtimeOf({platform,env});
  if(command==='rollback'){if(!options['--file']||!options['--backup'])fail('E_ARGUMENTS','--file and --backup are required.');out(JSON.stringify(await rollback(options['--file'],options['--backup'])));return;}
  if(!client)fail('E_ARGUMENTS','--client is required.');
  if(runtime==='unsupported')fail('E_UNSUPPORTED','This operating system is not supported.');
  const variable=variableFor(client),where=locations(client,{home,env});
  if(command==='status'){
    let state=null;try{state=JSON.parse(await readFile(where.stateFile,'utf8'));}catch{}
    let hooks='';try{hooks=await readFile(where.hooksFile,'utf8');}catch{}
    const detected=await detect(client,{env});
    out(JSON.stringify({client,runtime,detected,recall_default:recallDefault(client,detected.version,runtime),state,key_stored:await has(variable,{platform,home}),key_variable:variable,
      recall_hook_registered:hooks.replaceAll('\\','/').includes(where.hookScript.replaceAll('\\','/')),hooks_file:where.hooksFile,mcp_name:MCP_NAME[client]}));
    return;
  }
  const apply=options['--apply']===true;
  if(command==='uninstall'){
    // The MCP entry goes first, so no entry is left pointing at a removed bridge.
    const mcp=await removeMcp(client,{apply,home,env});
    const pieces=await removePieces({client,apply,home,env});
    const key=options['--keep-key']?{kept:true}:apply?await unstore(variable,{platform,home}):{would_remove:await has(variable,{platform,home})};
    out(JSON.stringify({preview:!apply,client,mcp:{name:MCP_NAME[client],...mcp},...pieces,key:{variable,...key}}));
    return;
  }
  // connect
  if(!options['--tenant'])fail('E_ARGUMENTS','--tenant is required (the tenant_id returned by POST /admin/tenants).');
  const {base,endpoint}=endpointFor(options['--url']||'http://127.0.0.1:8080');
  let key=await readKey(options,env);
  const stored=await has(variable,{platform,home});
  if(key&&!KEY.test(key))fail('E_ARGUMENTS','The key does not look like a broker API key (hc_ followed by 32 hex characters).');
  if(!key&&!stored)fail('E_ARGUMENTS','Provide the API key in HEURCHAIN_API_KEY or with --key-stdin. It is never printed.');
  if(!key){const {storedKey}=await import('./mcp-bridge.mjs');key=(keyStore.readKey||storedKey)(variable,{env,platform,home});}
  if(!key)fail('E_ARGUMENTS','The stored key could not be read; provide it again.');
  const verified=await preflight({base,endpoint,tenantId:options['--tenant'],key});
  const detected=await detect(client,{env}),version=detected.version||options['--client-version']||null;
  const fallback=recallDefault(client,version,runtime);
  const recall=options['--no-recall-hook']?{enabled:false,source:'opt-out',reason:'Turned off with --no-recall-hook; an earlier HeurChain hook from this setup is unregistered.'}
    :options['--recall-hook']?{enabled:true,source:'opt-in',reason:fallback.enabled?fallback.reason:'Explicit opt-in. '+fallback.reason}:{...fallback,source:'default'};
  const state=clientState({client,endpoint,tenantId:options['--tenant'],variable,enabled:recall.enabled});
  const keyResult=apply?(options['--key-stdin']||env.HEURCHAIN_API_KEY?await persist(variable,key,{platform,home}):{method:'reused stored key'}):{would_store:variable};
  const pieces=await installPieces({client,state,skill:!options['--no-skill'],recallHook:recall.enabled,apply,home,env,node,platform});
  const mcp=await registerMcp(client,serverFor(client,{endpoint,home,env,node,platform}),{apply,home,env});
  out(JSON.stringify({preview:!apply,client:NAMES[client],client_version:version,version_source:detected.version?'detected':version?'reported':'unknown',runtime,broker:{base,endpoint,...verified},
    key:{variable,...keyResult},mcp:{name:MCP_NAME[client],...mcp},...pieces,
    recall:{...recall,disclosure,credential:'the same tenant key as the MCP entry (0.6.1-oss.1 has no read-only keys); the hook only calls heurchain_search',review:pieces.recall_hook?.review||null},
    next:apply?['Restart '+NAMES[client]+' so it loads the MCP entry and hook.',client==='codex'&&recall.enabled?'Open /hooks in Codex and trust "HeurChain memory recall"; it does not run until you do.':'Run /hooks and /mcp to review the entries.']:['Re-run with --apply to make these changes.']}));
}

if(process.argv[1]&&/cli\.mjs$/.test(process.argv[1].replaceAll('\\','/'))){
  main(process.argv.slice(2)).catch(error=>{
    console.error(JSON.stringify({error:error.safeCode||'E_SETUP_FAILED',message:error.safeCode?error.message:'Setup stopped: '+String(error.message).slice(0,240)+' Inspect the named file and any backup before retrying.'}));
    process.exitCode=1;
  });
}
