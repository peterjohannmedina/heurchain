// Qualifies the recall hook and MCP entry on the INSTALLED Claude Code and Codex
// CLI against a real, DISPOSABLE self-hosted broker. Not part of `node --test`.
//
//   HEURCHAIN_API_KEY=... node tools/setup/native-check.mjs --url http://127.0.0.1:8080 \
//       --tenant TENANT_ID --output DIR [--client claude|codex]
//
// It stores one synthetic record (heurchain_store) in that tenant, so never point
// it at a broker that holds real memories. Each case installs into a disposable
// profile with the real setup flow (cli.mjs connect --apply), runs the client
// against a loopback model stand-in that records every model request, and sends
// the copied hook and bridge through a loopback proxy in front of the broker
// that counts hook lookups. No vendor model call is made. The synthetic key is
// kept in the disposable profile (not the user environment) and passed to the
// client process; profiles are deleted after each case. The receipt holds no key.
import {createServer,request as httpRequest} from 'node:http';
import {spawn,spawnSync} from 'node:child_process';
import {mkdtemp,mkdir,writeFile,readFile,rm} from 'node:fs/promises';
import {existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join,resolve} from 'node:path';
import {createHash,randomUUID} from 'node:crypto';
import zlib from 'node:zlib';
import {main as setup} from './cli.mjs';
import {locations} from './install.mjs';
import {persistKey,removeKey,hasKey} from './credentials.mjs';
import {storedKey} from './mcp-bridge.mjs';

const args=process.argv.slice(2),opt=n=>args.includes(n)?args[args.indexOf(n)+1]:null;
const BASE=opt('--url'),TENANT=opt('--tenant'),OUT=opt('--output'),ONLY=opt('--client'),KEY=process.env.HEURCHAIN_API_KEY||'';
if(!BASE||!TENANT||!OUT||!/^hc_[0-9a-f]{32}$/.test(KEY)){console.error('Use HEURCHAIN_API_KEY=... --url URL --tenant ID --output DIR [--client claude|codex] against a disposable broker.');process.exit(2);}
const FACT='QZ9-'+randomUUID().slice(0,6).toUpperCase(),DURATION='23 minutes',MARK='initiator="hook"',RECORD='native-check-'+FACT.toLowerCase();
// A per-run codename keeps this run's record distinct from earlier runs' records.
const WORKER='atlas '+randomUUID().replaceAll('-','').slice(0,8);
const NEEDED=`Draft the rollout checklist we agreed for the ${WORKER} worker.`;
const SELF_CONTAINED='Alphabetize these words: pine, lilac, oak.';
const sha=v=>createHash('sha256').update(v).digest('hex');
// File-backed key store inside each disposable profile.
const fileStore={persistKey:(v,k,o)=>persistKey(v,k,{...o,platform:'linux'}),removeKey:(v,o)=>removeKey(v,{...o,platform:'linux'}),hasKey:(v,o)=>hasKey(v,{...o,platform:'linux'}),readKey:(v,o)=>storedKey(v,{...o,platform:'linux'})};

function listen(handler){return new Promise(r=>{const s=createServer(handler);s.listen(0,'127.0.0.1',()=>r(s));});}
async function body(req){const chunks=[];for await(const c of req)chunks.push(c);let b=Buffer.concat(chunks);if(req.headers['content-encoding']==='zstd')b=zlib.zstdDecompressSync(b);return b;}
async function mcp(method,params){
  const r=await fetch(BASE.replace(/\/$/,'')+'/mcp/sse',{method:'POST',headers:{'Content-Type':'application/json',Accept:'application/json, text/event-stream','X-HeurChain-API-Key':KEY,'X-HeurChain-Tenant':TENANT},body:JSON.stringify({jsonrpc:'2.0',id:1,method,params})});
  const t=await r.text();const d=/event-stream/.test(r.headers.get('content-type')||'')?t.split(/\r?\n/).filter(l=>l.startsWith('data:')).map(l=>l.slice(5)).pop():t;return JSON.parse(d);
}

// Loopback proxy in front of the real broker. Hook requests are identified by
// their User-Agent; mode "fail" answers them with 503 (broker outage for the hook).
async function proxy(mode,log){
  const target=new URL(BASE);
  return listen(async(req,res)=>{
    const raw=await body(req),ua=String(req.headers['user-agent']||''),hook=ua.startsWith('heurchain-recall-hook/');
    let tool=null;try{const j=JSON.parse(raw.toString('utf8'));tool=j.method==='tools/call'?j.params?.name:j.method;}catch{}
    log.push({at:Date.now(),source:hook?'hook':ua.startsWith('heurchain-mcp-bridge/')?'bridge':ua.split('/')[0]||'unknown',path:req.url,tool});
    if(hook&&mode==='fail'){res.writeHead(503).end('{}');return;}
    const headers={...req.headers,host:target.host};delete headers['content-length'];
    const up=httpRequest({hostname:target.hostname,port:target.port,path:req.url,method:req.method,headers:{...headers,'content-length':raw.length}},u=>{res.writeHead(u.statusCode,u.headers);u.pipe(res);});
    up.on('error',()=>{res.writeHead(502).end();});up.end(raw);
  });
}

function observe(text){return {evidence:text.includes(FACT)&&text.includes(DURATION),hook_label:text.includes(MARK)||text.includes(JSON.stringify(MARK).slice(1,-1))};}

// Anthropic Messages stand-in. mode "tool": request 1 answers with a tool_use of
// the discovered HeurChain search tool; the client runs it over MCP.
async function anthropic(requests,prompt,mode){
  return listen(async(req,res)=>{
    const raw=(await body(req)).toString('utf8');
    if(!req.url.includes('/messages')||req.url.includes('count_tokens')){res.writeHead(200,{'content-type':'application/json'}).end('{"input_tokens":9}');return;}
    const b=JSON.parse(raw),messages=JSON.stringify(b.messages||[]),text=messages+JSON.stringify(b.system||[]);
    const tools=(b.tools||[]).map(t=>t.name).filter(n=>/heurchain/i.test(n));
    const toolResult=/"type":"tool_result"/.test(messages);
    requests.push({ordinal:requests.length+1,...observe(text),evidence_roles:(b.messages||[]).filter(m=>JSON.stringify(m).includes(FACT)).map(m=>m.role),heurchain_tools:tools.length,has_tool_result:toolResult,tool_result_has_fact:toolResult&&messages.includes(FACT)});
    const search=tools.find(n=>/heurchain_search$/.test(n));
    let content,stop='end_turn';
    if(mode==='tool'&&!toolResult&&search){content=[{type:'tool_use',id:'toolu_'+randomUUID().replaceAll('-','').slice(0,20),name:search,input:{text:WORKER+' worker rollout checklist',limit:3}}];stop='tool_use';}
    else{const ev=observe(text).evidence||(toolResult&&messages.includes(FACT));content=[{type:'text',text:ev?`Canary ${DURATION}; verification ${FACT}.`:prompt===SELF_CONTAINED?'lilac, oak, pine':'No stored decision available.'}];}
    const message={id:'msg_'+randomUUID(),type:'message',role:'assistant',model:b.model,content,stop_reason:stop,stop_sequence:null,usage:{input_tokens:30,output_tokens:12}};
    if(!b.stream){res.writeHead(200,{'content-type':'application/json'}).end(JSON.stringify(message));return;}
    const block=content[0],frames=[{type:'message_start',message:{...message,content:[],stop_reason:null}}];
    if(block.type==='text')frames.push({type:'content_block_start',index:0,content_block:{type:'text',text:''}},{type:'content_block_delta',index:0,delta:{type:'text_delta',text:block.text}});
    else frames.push({type:'content_block_start',index:0,content_block:{type:'tool_use',id:block.id,name:block.name,input:{}}},{type:'content_block_delta',index:0,delta:{type:'input_json_delta',partial_json:JSON.stringify(block.input)}});
    frames.push({type:'content_block_stop',index:0},{type:'message_delta',delta:{stop_reason:stop,stop_sequence:null},usage:{output_tokens:12}},{type:'message_stop'});
    res.writeHead(200,{'content-type':'text/event-stream'}).end(frames.map(f=>`event: ${f.type}\ndata: ${JSON.stringify(f)}\n\n`).join(''));
  });
}

// OpenAI Responses stand-in (SSE).
async function responses(requests,prompt){
  return listen(async(req,res)=>{
    if(req.method!=='POST'){res.writeHead(404).end();return;}
    const b=JSON.parse((await body(req)).toString('utf8')),text=JSON.stringify(b.input||[])+JSON.stringify(b.instructions||'');
    const tools=(b.tools||[]).flatMap(t=>t.type==='namespace'?(t.tools||[]).map(x=>(t.name||'')+'.'+(x.name||'')):[t.name||t.server_label||t.type||'']).filter(n=>/heurchain/i.test(n));
    requests.push({ordinal:requests.length+1,tool_names_sample:requests.length?undefined:(b.tools||[]).map(t=>[t.type,t.name||t.server_label||''].join(':')).slice(0,60),...observe(text),evidence_roles:(b.input||[]).filter(m=>JSON.stringify(m).includes(FACT)).map(m=>m.role),heurchain_tools:tools.length});
    const answer=observe(text).evidence?`Canary ${DURATION}; verification ${FACT}.`:prompt===SELF_CONTAINED?'lilac, oak, pine':'No stored decision available.';
    const item={type:'message',id:'msg_1',role:'assistant',content:[{type:'output_text',text:answer,annotations:[]}],status:'completed'};
    const resp={id:'resp_'+requests.length,object:'response',created_at:1,status:'completed',model:'synthetic-local',output:[item],usage:{input_tokens:1,output_tokens:1,total_tokens:2}};
    const ev=[{type:'response.created',response:{...resp,output:[],status:'in_progress'}},{type:'response.output_item.added',output_index:0,item:{...item,content:[]}},{type:'response.output_text.delta',output_index:0,content_index:0,item_id:'msg_1',delta:answer},{type:'response.output_item.done',output_index:0,item},{type:'response.completed',response:resp}];
    res.writeHead(200,{'content-type':'text/event-stream'}).end(ev.map(v=>`event: ${v.type}\ndata: ${JSON.stringify(v)}\n\n`).join(''));
  });
}

function baseEnv(home){
  const keep=['SystemRoot','SYSTEMROOT','WINDIR','COMSPEC','ComSpec','PATHEXT','PATH','Path','PROCESSOR_ARCHITECTURE','NUMBER_OF_PROCESSORS'];
  const env=Object.fromEntries(keep.filter(k=>process.env[k]).map(k=>[k,process.env[k]]));
  return {...env,HOME:home,USERPROFILE:home,APPDATA:join(home,'app'),LOCALAPPDATA:join(home,'local'),TEMP:join(home,'tmp'),TMP:join(home,'tmp'),
    HTTP_PROXY:'http://127.0.0.1:9',HTTPS_PROXY:'http://127.0.0.1:9',ALL_PROXY:'http://127.0.0.1:9',NO_PROXY:'127.0.0.1,localhost'};
}
function run(exe,argv,{env,cwd,timeout=90000}){
  return new Promise(r=>{
    const p=spawn(exe,argv,{env,cwd,stdio:['ignore','pipe','pipe'],windowsHide:true});let o='',e='';
    const timer=setTimeout(()=>p.kill(),timeout);
    p.stdout.on('data',d=>o+=d);p.stderr.on('data',d=>e+=d);p.on('close',code=>{clearTimeout(timer);r({code,stdout:o,stderr:e});});
  });
}
// Codex does not pass arbitrary environment variables to stdio MCP servers, so
// its bridge relies on the stored key: on Windows that is the real user
// environment. Codex cases therefore use the real store, refuse to overwrite an
// existing variable, and remove the synthetic key afterwards.
const realStore={persistKey,removeKey,hasKey,readKey:(v,o)=>storedKey(v,o)};
async function connect(client,home,env,url,extra=[],store=fileStore){
  let output='';
  await setup(['connect','--client',client,'--tenant',TENANT,'--url',url,'--apply',...extra],{env:{...env,HEURCHAIN_API_KEY:KEY},home,out:t=>output+=t,keyStore:store});
  const j=JSON.parse(output);if(JSON.stringify(j).includes(KEY))throw Error('setup output contained the key');return j;
}
async function profile(client){
  const home=await mkdtemp(join(tmpdir(),`hc-native-${client}-`)),env=baseEnv(home);
  for(const d of ['tmp','app','local','work','.claude','.codex'])await mkdir(join(home,d),{recursive:true});
  if(client==='claude')env.CLAUDE_CONFIG_DIR=join(home,'.claude');else env.CODEX_HOME=join(home,'.codex');
  return {home,env};
}
const hookCount=log=>log.filter(l=>l.source==='hook').length;
const bridgeTools=log=>[...new Set(log.filter(l=>l.source!=='hook'&&l.path.startsWith('/mcp')).map(l=>l.source+':'+l.tool))];

const claudeExe=process.env.HEURCHAIN_NATIVE_CLAUDE||join(process.env.APPDATA||'','npm/node_modules/@anthropic-ai/claude-code/bin/claude.exe');
const codexExe=process.env.HEURCHAIN_NATIVE_CODEX||join(process.env.LOCALAPPDATA||'','Programs/OpenAI/Codex/bin/codex.exe');

// variant: default | opt-out | env-off | upgrade | tool
async function claudeCase(version,prompt,{mode='ok',variant='default'}={}){
  const {home,env}=await profile('claude'),log=[],requests=[],px=await proxy(mode,log),model=await anthropic(requests,prompt,variant==='tool'?'tool':'answer');
  const url=`http://127.0.0.1:${px.address().port}`;
  try{
    // A user settings file with an unrelated hook and setting, which setup must keep.
    const where=locations('claude',{home,env});
    await writeFile(where.hooksFile,JSON.stringify({env:{EXAMPLE_SETTING:'kept'},hooks:{Stop:[{hooks:[{type:'command',command:'node -e "0"'}]}]}},null,2)+'\n');
    const first=await connect('claude',home,env,url);
    let setupResult=first,upgrade=null;
    if(variant==='upgrade'){
      // Simulate an older install: an old runtime and a stale hook command, then re-run setup.
      await writeFile(where.hookScript,"export const RECALL_HOOK_VERSION = '1.2.0-oss.0';\n");
      const s=JSON.parse(await readFile(where.hooksFile,'utf8'));s.hooks.UserPromptSubmit[0].hooks[0].command+=' --legacy';await writeFile(where.hooksFile,JSON.stringify(s,null,2)+'\n');
      setupResult=await connect('claude',home,env,url);
      const after=JSON.parse(await readFile(where.hooksFile,'utf8'));
      upgrade={runtime_sha256_matches:sha((await readFile(where.hookScript,'utf8')))===first.recall_hook.sha256,registrations:after.hooks.UserPromptSubmit.length,stale_flag_gone:!JSON.stringify(after).includes('--legacy')};
    }
    if(variant==='opt-out')setupResult=await connect('claude',home,env,url,['--no-recall-hook']);
    const settings=JSON.parse(await readFile(where.hooksFile,'utf8'));
    const bash=['C:/Program Files/Git/bin/bash.exe',process.env.HEURCHAIN_NATIVE_GIT_BASH].filter(Boolean).find(existsSync);
    const runEnv={...env,HEURCHAIN_LOCAL_CLAUDE_KEY:KEY,ANTHROPIC_BASE_URL:`http://127.0.0.1:${model.address().port}`,ANTHROPIC_API_KEY:'synthetic-local-key',
      DISABLE_TELEMETRY:'1',DISABLE_ERROR_REPORTING:'1',DISABLE_AUTOUPDATER:'1',ENABLE_CLAUDEAI_MCP_SERVERS:'false',CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:'1',CLAUDE_CODE_DISABLE_AUTO_MEMORY:'1',
      ...(variant==='env-off'?{HEURCHAIN_RECALL_HOOK:'off'}:{}),...(bash?{CLAUDE_CODE_GIT_BASH_PATH:bash}:{})};
    const extra=variant==='tool'?['--allowedTools','mcp__heurchain-local__heurchain_search']:[];
    const r=await run(claudeExe,['-p',prompt,'--output-format','json','--no-session-persistence','--setting-sources','user','--tools','',...extra,'--model','claude-sonnet-4-6'],{env:runEnv,cwd:join(home,'work')});
    let result=null;try{result=JSON.parse(r.stdout).result;}catch{}
    const list=variant==='default'&&prompt===NEEDED&&mode==='ok'?await run(claudeExe,['mcp','list'],{env:runEnv,cwd:join(home,'work')}):null;
    return {client:'claude',version,shell:bash?'git-bash':'client-default',case:variant==='default'?(mode==='fail'?'heurchain-unavailable':prompt===NEEDED?'memory-needed':'self-contained'):variant==='tool'?'client-tool-call':variant,
      exit:r.code,recall_enabled:setupResult.recall.enabled,recall_source:setupResult.recall.source,hook_version:first.recall_hook?.version??null,hook_sha256:first.recall_hook?.sha256??null,
      unrelated_settings_kept:settings.env?.EXAMPLE_SETTING==='kept'&&settings.hooks?.Stop?.length===1,hook_registrations:(settings.hooks?.UserPromptSubmit||[]).length,
      lookups:hookCount(log),bridge_calls:bridgeTools(log),first_request:requests[0]||null,requests:requests.length,second_request:requests[1]||null,answer_has_fact:result?.includes(FACT)??null,
      mcp_list:list?(list.stdout.split(/\r?\n/).find(l=>/heurchain-local/.test(l))||'').replace(/127\.0\.0\.1:\d+/,'127.0.0.1:<proxy>').trim():undefined,upgrade};
  }finally{px.close();model.close();await rm(home,{recursive:true,force:true}).catch(()=>{});}
}

async function codexTrust(exe,{env,work,where,config}){
  // Stand-in for the user's /hooks review in this disposable profile only: read
  // the hashes Codex itself computes and mark exactly those trusted.
  const a=spawn(exe,['--no-daemon','app-server','--stdio'],{cwd:work,env,stdio:['pipe','pipe','ignore'],windowsHide:true});
  const lines=[];let buf='';a.stdout.on('data',d=>{buf+=d;let i;while((i=buf.indexOf('\n'))>=0){try{lines.push(JSON.parse(buf.slice(0,i)));}catch{}buf=buf.slice(i+1);}});
  const wait=async id=>{for(let n=0;n<300;n++){const m=lines.find(l=>l.id===id);if(m)return m;await new Promise(r=>setTimeout(r,50));}throw Error('app-server did not answer');};
  const send=o=>a.stdin.write(JSON.stringify(o)+'\n'),q=v=>JSON.stringify(v),reviewed=[];
  try{
    send({id:1,method:'initialize',params:{clientInfo:{name:'hc-native',version:'1'},capabilities:{experimentalApi:true}}});await wait(1);
    send({method:'initialized',params:{}});send({id:2,method:'hooks/list',params:{cwds:[work]}});
    const list=await wait(2);
    for(const h of list.result.data[0].hooks){
      if(resolve(h.sourcePath)!==resolve(where.hooksFile))continue;
      reviewed.push({event:h.eventName,hash:h.currentHash,label:h.statusMessage??null});
      config+=`\n[hooks.state.${q(h.key)}]\ntrusted_hash=${q(h.currentHash)}\n`;
    }
  }finally{a.kill();}
  return {config,reviewed};
}

async function codexCase(version,prompt,{mode='ok',variant='default',trusted=true}={}){
  const {home,env}=await profile('codex'),log=[],requests=[],px=await proxy(mode,log),model=await responses(requests,prompt);
  const url=`http://127.0.0.1:${px.address().port}`,work=join(home,'work'),q=v=>JSON.stringify(v);
  try{
    let config=`model_provider="localwire"\nmodel="synthetic-local"\napproval_policy="never"\nsandbox_mode="read-only"\ncheck_for_update_on_startup=false\n[analytics]\nenabled=false\n[feedback]\nenabled=false\n[model_providers.localwire]\nname="Local fixture"\nbase_url="http://127.0.0.1:${model.address().port}/v1"\nwire_api="responses"\nsupports_websockets=false\nrequest_max_retries=0\nstream_max_retries=0\n[projects.${q(work)}]\ntrust_level="trusted"\n`;
    await writeFile(join(env.CODEX_HOME,'config.toml'),config);
    const where=locations('codex',{home,env});
    await writeFile(where.hooksFile,JSON.stringify({hooks:{Stop:[{hooks:[{type:'command',command:'node -e "0"'}]}]}},null,2)+'\n');
    if(process.platform==='win32'&&await hasKey('HEURCHAIN_LOCAL_CODEX_KEY'))throw Error('HEURCHAIN_LOCAL_CODEX_KEY already exists in the user environment; run native-check on a machine or account without a real Codex setup.');
    const store=process.platform==='win32'?realStore:fileStore;
    let setupResult=await connect('codex',home,env,url,[],store);
    if(variant==='opt-out')setupResult=await connect('codex',home,env,url,['--no-recall-hook'],store);
    // codex mcp add appended its table to config.toml; keep it and add trust state.
    config=await readFile(join(env.CODEX_HOME,'config.toml'),'utf8');
    const mcpTable=/\[mcp_servers\.heurchain_local\]/.test(config);
    let reviewed=[];
    if(trusted&&variant!=='opt-out'){const t=await codexTrust(codexExe,{env,work,where,config});config=t.config;reviewed=t.reviewed;await writeFile(join(env.CODEX_HOME,'config.toml'),config);}
    const hooks=JSON.parse(await readFile(where.hooksFile,'utf8'));
    const r=await run(codexExe,['--no-daemon','exec','--json','--ephemeral','--skip-git-repo-check','--ignore-rules',prompt],{env:{...env,HEURCHAIN_LOCAL_CODEX_KEY:KEY},cwd:work});
    const answers=r.stdout.split('\n').filter(Boolean).map(l=>{try{return JSON.parse(l);}catch{return null;}}).filter(e=>e?.type==='item.completed'&&e.item?.type==='agent_message').map(e=>e.item.text);
    return {client:'codex',version,case:(trusted?'':'untrusted-')+(variant==='default'?(mode==='fail'?'heurchain-unavailable':prompt===NEEDED?'memory-needed':'self-contained'):variant),
      exit:r.code,recall_enabled:setupResult.recall.enabled,recall_source:setupResult.recall.source,trusted_hooks:reviewed,hook_version:setupResult.recall_hook?.version??null,
      mcp_table_written:mcpTable,unrelated_hooks_kept:hooks.hooks?.Stop?.length===1,hook_registrations:(hooks.hooks?.UserPromptSubmit||[]).length,
      lookups:hookCount(log),bridge_calls:bridgeTools(log),first_request:requests[0]||null,requests:requests.length,answer_has_fact:answers.some(a=>a.includes(FACT)),
      diagnostics:/hook/i.test(r.stderr)?r.stderr.split('\n').filter(l=>/hook/i.test(l)).slice(0,4).map(l=>l.slice(0,300)):[]};
  }finally{px.close();model.close();if(process.platform==='win32')await removeKey('HEURCHAIN_LOCAL_CODEX_KEY').catch(()=>{});await rm(home,{recursive:true,force:true}).catch(()=>{});}
}

function verdict(row){
  const f=row.first_request,c=row.case;
  if(row.client==='claude'&&!row.unrelated_settings_kept)return false;
  if(row.client==='codex'&&!row.unrelated_hooks_kept)return false;
  if(c==='opt-out')return row.exit===0&&!row.recall_enabled&&row.hook_registrations===0&&row.lookups===0&&f&&!f.evidence;
  if(c==='env-off')return row.exit===0&&row.hook_registrations===1&&row.lookups===0&&f&&!f.evidence;
  if(c.startsWith('untrusted'))return row.lookups===0&&f&&!f.evidence;
  if(c==='client-tool-call')return row.exit===0&&row.lookups===0&&row.second_request?.tool_result_has_fact&&row.answer_has_fact;
  if(c==='upgrade')return row.exit===0&&row.upgrade.runtime_sha256_matches&&row.upgrade.registrations===1&&row.upgrade.stale_flag_gone&&row.lookups===1&&f?.evidence;
  if(c==='memory-needed')return row.exit===0&&row.recall_enabled&&row.lookups===1&&f?.evidence&&f?.hook_label&&row.answer_has_fact&&f.heurchain_tools>0;
  if(c==='self-contained')return row.exit===0&&row.lookups===0&&f&&!f.evidence;
  return row.exit===0&&row.lookups===1&&f&&!f.evidence; // hook-side outage: prompt proceeds without evidence
}

// Seed the synthetic record the memory-needed prompt should recall.
const stored=await mcp('tools/call',{name:'heurchain_store',arguments:{id:RECORD,text:`${WORKER} worker rollout decision (native-check synthetic): canary for ${DURATION}, then verification code ${FACT}.`,agent_id:'native-check',tags:['decision','synthetic']}});
if(stored.error||stored.result?.isError)throw Error('Could not seed the synthetic record.');
await new Promise(r=>setTimeout(r,1500));
const health=await (await fetch(BASE.replace(/\/$/,'')+'/health')).json();
const rows=[];
if(ONLY!=='codex'&&existsSync(claudeExe)){
  const version=spawnSync(claudeExe,['--version'],{encoding:'utf8'}).stdout.trim();
  rows.push(await claudeCase(version,NEEDED));
  rows.push(await claudeCase(version,SELF_CONTAINED));
  rows.push(await claudeCase(version,NEEDED,{mode:'fail'}));
  rows.push(await claudeCase(version,NEEDED,{variant:'opt-out'}));
  rows.push(await claudeCase(version,NEEDED,{variant:'env-off'}));
  rows.push(await claudeCase(version,NEEDED,{variant:'upgrade'}));
  rows.push(await claudeCase(version,SELF_CONTAINED,{variant:'tool'}));
}
if(ONLY!=='claude'&&existsSync(codexExe)){
  const version=spawnSync(codexExe,['--version'],{encoding:'utf8'}).stdout.trim();
  rows.push(await codexCase(version,NEEDED));
  rows.push(await codexCase(version,SELF_CONTAINED));
  rows.push(await codexCase(version,NEEDED,{mode:'fail'}));
  rows.push(await codexCase(version,NEEDED,{trusted:false}));
  rows.push(await codexCase(version,NEEDED,{variant:'opt-out'}));
}
for(const r of rows)r.pass=Boolean(verdict(r));
const receipt={schema_version:1,generated_at:new Date().toISOString(),platform:`${process.platform}-${process.arch}`,node:process.version,
  broker:{version:health.version,edition:health.edition,sha256:health.sha256},synthetic_record:RECORD,
  boundary:'real disposable broker behind a loopback counting proxy; loopback model stand-ins (no vendor model call); synthetic key held in each disposable profile; profiles deleted after each case',
  executables:{claude:existsSync(claudeExe)?{sha256:sha(await readFile(claudeExe))}:null,codex:existsSync(codexExe)?{sha256:sha(await readFile(codexExe))}:null},cases:rows};
const text=JSON.stringify(receipt,null,2)+'\n';
if(text.includes(KEY))throw Error('receipt contained the key');
await mkdir(OUT,{recursive:true});await writeFile(join(OUT,'native-receipts.json'),text);
for(const r of rows)console.log(`${r.pass?'PASS':'FAIL'} ${r.client} ${r.version} ${r.case}: lookups=${r.lookups} first_request_evidence=${r.first_request?.evidence} hook_label=${r.first_request?.hook_label} heurchain_tools=${r.first_request?.heurchain_tools} recall=${r.recall_enabled}/${r.recall_source} answer_fact=${r.answer_has_fact} exit=${r.exit}`);
// Remove the synthetic record so later runs start clean.
const got=await mcp('tools/call',{name:'heurchain_memory_get',arguments:{id:RECORD}}).catch(()=>null);
const rev=(()=>{try{const p=JSON.parse(got.result.content[0].text);return (p.memory||p).revision;}catch{return null;}})();
const forgot=rev?await mcp('tools/call',{name:'heurchain_memory_forget',arguments:{id:RECORD,expected_revision:rev,confirm:true}}).catch(()=>null):null;
console.log('cleanup: '+(forgot&&!forgot.result?.isError?'forgot '+RECORD:'record left in place: '+RECORD));
process.exitCode=rows.length&&rows.every(r=>r.pass)?0:1;
