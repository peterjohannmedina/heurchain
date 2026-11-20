// node --test tools/setup/
// Offline tests: no broker, client or network. Every profile is a temp directory.
import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,mkdir,writeFile,readFile,readdir,rm} from 'node:fs/promises';
import {existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {needsRecall,redact,queryFor,recall,formatContext} from './recall-hook.mjs';
import {headersFor,forward} from './mcp-bridge.mjs';
import {mergeHooks,validateHooks,hookEntries,commandLine,installPieces,removePieces,locations,clientState,bundled} from './install.mjs';
import {transact,rollback} from './configure.mjs';
import {recallDefault,registerMcp} from './clients.mjs';
import {parse,endpointFor} from './cli.mjs';
import {persistKey,removeKey,hasKey} from './credentials.mjs';

const KEY='hc_'+'a'.repeat(32),TENANT='00000000-0000-4000-8000-000000000001';
const profile=async()=>{const home=await mkdtemp(join(tmpdir(),'hc-setup-test-'));return {home,env:{PATH:process.env.PATH,CODEX_HOME:join(home,'.codex')}};};
const state=client=>clientState({client,endpoint:'http://127.0.0.1:8080/mcp/sse',tenantId:TENANT,variable:'HEURCHAIN_LOCAL_'+client.toUpperCase()+'_KEY'});
const node='/usr/bin/node';

test('relevance gate: recall tasks pass, self-contained ones do not', () => {
  assert.equal(needsRecall('Draft the rollout checklist we agreed for the Atlas worker.'), true);
  assert.equal(needsRecall('Alphabetize these words: pine, lilac, oak.'), false);
  assert.equal(needsRecall('Use only the values provided below for the rollout.'), false);
  assert.equal(needsRecall('/hooks'), false);
});

test('query redaction removes broker keys and other secrets', () => {
  const q=queryFor(`rollout uses ${KEY} and password=hunter2 and sk-${'x'.repeat(20)}`);
  assert.ok(!q.includes(KEY)&&!q.includes('hunter2')&&!q.includes('sk-x'));
  assert.equal(redact('no secret'), 'no secret');
});

test('hook: off switches, missing key and self-contained prompts make no request', async () => {
  let calls=0;const fetchImpl=async()=>{calls++;throw Error('no');};
  const ev={hook_event_name:'UserPromptSubmit',prompt:'What did we decide about the rollout?'};
  const s=state('claude');
  assert.equal(await recall(ev,{state:s,env:{HEURCHAIN_RECALL_HOOK:'off',[s.variable]:KEY},fetchImpl}),null);
  assert.equal(await recall(ev,{state:{...s,enabled:false},env:{[s.variable]:KEY},fetchImpl}),null);
  assert.equal(await recall(ev,{state:s,env:{},fetchImpl,readStored:()=>''}),null);
  assert.equal(await recall({...ev,prompt:'Sort these: b, a'},{state:s,env:{[s.variable]:KEY},fetchImpl}),null);
  assert.equal(calls,0);
});

test('hook: one heurchain_search call with both headers, labelled output', async () => {
  const s=state('codex');let seen;
  const fetchImpl=async(url,init)=>{seen={url,headers:init.headers,body:JSON.parse(init.body)};
    return new Response(JSON.stringify({jsonrpc:'2.0',id:1,result:{content:[{type:'text',text:JSON.stringify({results:[{id:'r1',agent_id:'a',text:'Rollout canary 17 minutes',bm25_rank:1},{id:'r2',text:'weak',qdrant_score:0.2}]})}]}}),{headers:{'content-type':'application/json'}});};
  const out=await recall({hook_event_name:'UserPromptSubmit',prompt:'What rollout did we agree?'},{state:s,env:{[s.variable]:KEY},fetchImpl});
  assert.equal(seen.url,'http://127.0.0.1:8080/mcp/sse');
  assert.equal(seen.headers['X-HeurChain-API-Key'],KEY);assert.equal(seen.headers['X-HeurChain-Tenant'],TENANT);
  assert.equal(seen.body.params.name,'heurchain_search');
  const ctx=out.hookSpecificOutput.additionalContext;
  assert.match(ctx,/initiator="hook"/);assert.match(ctx,/\[r1 by a\]/);assert.doesNotMatch(ctx,/weak/);
});

test('hook: a failing broker yields no output instead of an error', async () => {
  const s=state('claude');
  assert.equal(await recall({hook_event_name:'UserPromptSubmit',prompt:'previously agreed rollout'},{state:s,env:{[s.variable]:KEY},fetchImpl:async()=>new Response('',{status:503})}),null);
  assert.equal(formatContext([],{maxChars:2000}),'');
});

test('bridge: X-HeurChain-API-Key headers, and a clear error without a key', async () => {
  assert.deepEqual(headersFor({tenant_id:TENANT},KEY),{'X-HeurChain-API-Key':KEY,'X-HeurChain-Tenant':TENANT});
  assert.deepEqual(headersFor({tenant_id:TENANT},''),{'X-HeurChain-Tenant':TENANT});
  const [r]=await forward('{"jsonrpc":"2.0","id":7,"method":"tools/list"}',{state:{endpoint:'http://x/mcp/sse',tenant_id:TENANT},key:''});
  assert.equal(r.id,7);assert.match(r.error.message,/key not found/);
});

test('hook merge keeps unrelated hooks and settings, is idempotent, and removes only ours', async () => {
  const script='/home/u/.heurchain/local/claude/heurchain-recall-hook.mjs';
  const entries=hookEntries(await bundled.template('claude'),{command:commandLine(script,'/home/u/.heurchain/local/claude/client.json',{node})});
  const hosted={hooks:[{type:'command',command:'node "/home/u/.heurchain/claude/heurchain-recall-hook.mjs" --state "/x"'}]};
  const other={matcher:'',hooks:[{type:'command',command:'echo other'}]};
  const original=JSON.stringify({model:'x',hooks:{UserPromptSubmit:[other,hosted],Stop:[other]}},null,2)+'\n';
  const once=mergeHooks(original,entries,{script});
  validateHooks(once,original,{script,expectOurs:true});
  const data=JSON.parse(once);
  assert.equal(data.model,'x');assert.equal(data.hooks.UserPromptSubmit.length,3);assert.deepEqual(data.hooks.Stop,[other]);
  assert.equal(mergeHooks(once,entries,{script}),once,'repeat run is byte-identical');
  // Upgrade: a different command for our script is replaced, not duplicated.
  const upgraded=mergeHooks(once.replace('--state','--old-flag --state'),entries,{script});
  assert.equal(JSON.parse(upgraded).hooks.UserPromptSubmit.length,3);
  const removed=mergeHooks(once,entries,{script,remove:true});
  validateHooks(removed,once,{script,expectOurs:false});
  assert.deepEqual(JSON.parse(removed).hooks.UserPromptSubmit,[other,hosted],'hosted HeurChain hook is not ours to remove');
});

test('validation refuses a merge that would touch entries setup does not own', () => {
  const script='/s/heurchain-recall-hook.mjs';
  const before=JSON.stringify({hooks:{Stop:[{hooks:[{type:'command',command:'echo'}]}]}});
  assert.throws(()=>validateHooks(JSON.stringify({hooks:{}}),before,{script,expectOurs:false}),/does not own/);
  assert.throws(()=>validateHooks(before,before,{script,expectOurs:true}),/exactly the expected/);
});

test('mergeHooks refuses malformed hook settings', () => {
  assert.throws(()=>mergeHooks('{"hooks":[]}',{UserPromptSubmit:[]},{script:'/s'}),/not an object/);
  assert.throws(()=>mergeHooks('{"hooks":{"UserPromptSubmit":{}}}',{UserPromptSubmit:[]},{script:'/s'}),/not a list/);
  assert.throws(()=>mergeHooks('[1]',{UserPromptSubmit:[]},{script:'/s'}),/JSON object/);
});

test('transact validates before writing, backs up, and rolls back', async () => {
  const {home}=await profile();
  try{
    const file=join(home,'settings.json');await writeFile(file,'{"a":1}\n');
    await assert.rejects(transact(file,()=>'{"a":2}\n',{apply:true,validate:()=>{throw Error('invalid');}}),/invalid/);
    assert.equal(await readFile(file,'utf8'),'{"a":1}\n','refused merge leaves the file untouched');
    const dry=await transact(file,()=>'{"a":2}\n');assert.deepEqual(dry,{changed:true,applied:false});
    const r=await transact(file,()=>'{"a":2}\n',{apply:true});
    assert.equal(await readFile(file,'utf8'),'{"a":2}\n');
    await rollback(file,r.backup);assert.equal(await readFile(file,'utf8'),'{"a":1}\n');
  }finally{await rm(home,{recursive:true,force:true});}
});

test('install, repeat, opt-out and uninstall on a disposable profile', async () => {
  const {home,env}=await profile();
  try{
    const where=locations('claude',{home,env});
    await mkdir(join(home,'.claude'),{recursive:true});
    const unrelated={permissions:{allow:['Bash(ls)']},hooks:{UserPromptSubmit:[{hooks:[{type:'command',command:'echo mine'}]}]}};
    await writeFile(where.hooksFile,JSON.stringify(unrelated,null,2)+'\n');
    const args={client:'claude',state:state('claude'),home,env,node,platform:'linux'};
    const preview=await installPieces({...args,recallHook:true});
    assert.ok(preview.results.every(r=>!r.applied));assert.equal(existsSync(where.hookScript),false,'dry run writes nothing');
    const first=await installPieces({...args,recallHook:true,apply:true});
    assert.equal(first.recall_hook.version,'1.3.0-oss.1');
    const settings=JSON.parse(await readFile(where.hooksFile,'utf8'));
    assert.deepEqual(settings.permissions,unrelated.permissions);assert.equal(settings.hooks.UserPromptSubmit.length,2);
    assert.ok(!(await readFile(where.stateFile,'utf8')).includes('hc_'),'state is secret-free');
    const again=await installPieces({...args,recallHook:true,apply:true});
    assert.ok(again.results.every(r=>!r.changed),'repeat setup changes nothing');
    const off=await installPieces({...args,state:{...state('claude'),enabled:false},recallHook:false,apply:true});
    assert.ok(off.results.some(r=>r.piece==='recall-hook-registration'&&r.applied));
    assert.deepEqual(JSON.parse(await readFile(where.hooksFile,'utf8')).hooks,unrelated.hooks,'opt-out removes only our hook');
    assert.equal(existsSync(where.hookScript),false);
    await installPieces({...args,recallHook:true,apply:true});
    await removePieces({client:'claude',home,env,apply:true});
    assert.deepEqual(JSON.parse(await readFile(where.hooksFile,'utf8')),unrelated,'uninstall restores the unrelated settings exactly');
    assert.equal(existsSync(where.skillFile),false);assert.equal(existsSync(where.bridgeScript),false);
  }finally{await rm(home,{recursive:true,force:true});}
});

test('skill install refuses to replace a different skill at the same path', async () => {
  const {home,env}=await profile();
  try{
    const where=locations('codex',{home,env});
    await mkdir(join(home,'.agents','skills','heurchain-memory'),{recursive:true});
    await writeFile(where.skillFile,'---\nname: something-else\n---\n');
    await assert.rejects(installPieces({client:'codex',state:state('codex'),recallHook:false,apply:true,home,env,node,platform:'linux'}),/different skill/);
  }finally{await rm(home,{recursive:true,force:true});}
});

test('recall defaults: on only for qualified builds and runtimes', () => {
  assert.equal(recallDefault('claude','2.1.329','windows').enabled,true);
  assert.equal(recallDefault('claude','2.1.329','macos').enabled,false);
  assert.equal(recallDefault('codex','0.169.0','windows').enabled,false);
  assert.equal(recallDefault('codex',null,'windows').enabled,false);
});

test('MCP registration: identical entry is a no-op, a changed one is replaced', async () => {
  const calls=[];const server={command:node,args:['/b','--state','/s']};
  let current={command:node,args:['/old']};
  const runImpl=async(name,args)=>{calls.push(args.slice(0,2).join(' '));
    if(args[1]==='get')return current?{code:0,stdout:JSON.stringify({transport:{type:'stdio',...current}})}:{code:1,stdout:''};
    if(args[1]==='remove'){current=null;return {code:0};}
    if(args[1]==='add'){current={command:args[4],args:args.slice(5)};return {code:0};}
    return {code:1};};
  const r=await registerMcp('codex',server,{apply:true,env:{},runImpl});
  assert.equal(r.replaced,true);assert.deepEqual(current,server);
  calls.length=0;
  assert.equal((await registerMcp('codex',server,{apply:true,env:{},runImpl})).changed,false);
  assert.deepEqual(calls,['mcp get']);
});

test('CLI arguments: strict options and safe URLs', () => {
  assert.throws(()=>parse(['connect','--client','muse']),/claude .* or codex/);
  assert.throws(()=>parse(['connect','--recall-hook','--no-recall-hook']),/not both/);
  assert.throws(()=>parse(['connect','--key','x']),/Unknown/);
  assert.equal(endpointFor('http://127.0.0.1:8080/').endpoint,'http://127.0.0.1:8080/mcp/sse');
  assert.throws(()=>endpointFor('http://broker.lan:8080'),/https/);
  assert.throws(()=>endpointFor('http://u:p@127.0.0.1:8080'),/without credentials/);
});

test('key storage on POSIX: private file, presence check, removal', async () => {
  const {home}=await profile();
  try{
    const v='HEURCHAIN_LOCAL_CODEX_KEY';
    await assert.rejects(persistKey(v,'not-a-key',{platform:'linux',home}),/unexpected/);
    await persistKey(v,KEY,{platform:'linux',home});
    assert.equal(await hasKey(v,{platform:'linux',home}),true);
    if(process.platform!=='win32'){const {mode}=await (await import('node:fs/promises')).stat(join(home,'.heurchain','credentials.env'));assert.equal(mode&0o777,0o600);}
    await removeKey(v,{platform:'linux',home});
    assert.equal(await hasKey(v,{platform:'linux',home}),false);
  }finally{await rm(home,{recursive:true,force:true});}
});

test('the copied runtimes contain no hosted endpoint', async () => {
  for(const text of [await bundled.hook(),await bundled.bridge()])assert.doesNotMatch(text,/api\.heurchain\.com|hc_client_/);
  const files=await readdir(new URL('./templates/',import.meta.url));assert.deepEqual(files.sort(),['recall-hook.claude.json','recall-hook.codex.json']);
});
