#!/usr/bin/env node
// heurchain: install and run self-hosted HeurChain from the npm package.
//
//   npx heurchain init [DIR] [--port N] [--embedder bundled|host-ollama|openai]
//                                     unpack the server (broker source, Compose stack, agent
//                                     setup, skill, docs) and write a private deploy/.env
//   npx heurchain up [DIR] [--build]  pull the prebuilt broker image (or build it), start the
//                                     stack, prepare the embedding model, wait until ready
//   npx heurchain tenant [DIR] --name N --email E
//                                     create a tenant and API key (the key is shown once)
//   npx heurchain connect ...         agent setup (MCP entry, skill, recall hook);
//                                     same options as tools/setup/cli.mjs connect
//   npx heurchain status|down [DIR]   broker health / stop the stack (data is kept)
//
// Embedders: bundled (default) runs Ollama in the stack (about 3.3 GB to download).
// host-ollama uses Ollama already running on this machine (--ollama-url, default
// http://host.docker.internal:11434). openai uses an OpenAI-compatible endpoint
// (--embed-url and --embed-model; key, if needed, from HEURCHAIN_EMBED_API_KEY).
//
// Needs Node 20+ and Docker Engine with Compose 2.20+ (or Podman with `podman compose`).
// Nothing here prints the admin password; it stays in DIR/deploy/.env (mode 0600).
import {cpSync,existsSync,mkdirSync,readFileSync,writeFileSync,chmodSync,readdirSync,copyFileSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {randomBytes} from 'node:crypto';
import {dirname,join,resolve} from 'node:path';
import {fileURLToPath} from 'node:url';

const PKG=join(dirname(fileURLToPath(import.meta.url)),'..');
const VERSION=JSON.parse(readFileSync(join(PKG,'package.json'),'utf8')).version;
const MARK='.heurchain-server';
const PARTS=['broker','deploy','tools','skills','docs'];
const KEEP=['deploy/.env','deploy/broker.yaml']; // user-owned after init
const EMBEDDERS=['bundled','host-ollama','openai'];
const DEFAULT_MODEL='nomic-embed-text';

function fail(message,code=1){console.error('heurchain: '+message);process.exit(code);}
function parse(argv){
  const pos=[],opt={};
  for(let i=0;i<argv.length;i++){
    const a=argv[i];
    if(a.startsWith('--')){const k=a.slice(2);if(['json','build'].includes(k))opt[k]=true;else{if(argv[i+1]===undefined||argv[i+1].startsWith('--'))fail('missing value for '+a);opt[k]=argv[++i];}}
    else pos.push(a);
  }
  return {pos,opt};
}
const dirOf=pos=>resolve(pos[0]||'heurchain-server');
const envPath=dir=>join(dir,'deploy','.env');
function readEnv(dir){
  if(!existsSync(envPath(dir)))fail(`${envPath(dir)} not found. Run "heurchain init ${dir}" first.`);
  return Object.fromEntries(readFileSync(envPath(dir),'utf8').split(/\r?\n/).filter(l=>/^[A-Z_][A-Z0-9_]*=/.test(l)).map(l=>[l.slice(0,l.indexOf('=')),l.slice(l.indexOf('=')+1)]));
}
const baseUrl=env=>`http://127.0.0.1:${env.HEURCHAIN_PORT||'8080'}`;
// Set KEY=value, replacing an existing or commented-out line; append otherwise.
export function setEnv(text,key,value){
  const line=`${key}=${value}`,re=new RegExp(`^#?\\s*${key}=.*$`,'m');
  return re.test(text)?text.replace(re,line):text.replace(/\n*$/,'\n')+line+'\n';
}
// Replace the embed: block of broker.yaml (the strict loader accepts these keys).
export function embedBlock(kind,{ollamaUrl,model,url}={}){
  if(kind==='bundled')return `embed:\n  provider: ollama\n  ollama_url: "http://ollama:11434"\n  ollama_model: "${model||DEFAULT_MODEL}"\n`;
  if(kind==='host-ollama')return `embed:\n  provider: ollama\n  ollama_url: ${JSON.stringify(ollamaUrl)}\n  ollama_model: ${JSON.stringify(model||DEFAULT_MODEL)}\n`;
  return `embed:\n  provider: openai\n  ollama_url: ""\n  openai_url: ${JSON.stringify(url)}\n  openai_model: ${JSON.stringify(model)}\n`;
}
export function setEmbed(yaml,block){
  const text=yaml.replace(/\r\n/g,'\n'),re=/^embed:\n(?:(?:[ \t]+\S.*|[ \t]*)\n)*/m;
  if(!re.test(text))fail('deploy/broker.yaml has no embed: section to update; edit it by hand.');
  return text.replace(re,block+'\n');
}
function httpUrl(value,name){let u;try{u=new URL(value);}catch{fail(name+' must be an http(s) URL.');}if(!/^https?:$/.test(u.protocol)||u.username||u.password)fail(name+' must be an http(s) URL without credentials.');return u.href.replace(/\/$/,'');}

function compose(){
  for(const [cmd,pre] of [['docker',['compose']],['podman',['compose']],['docker-compose',[]]]){
    const r=spawnSync(cmd,[...pre,'version'],{encoding:'utf8',windowsHide:true});
    if(r.status===0)return {cmd,pre};
  }
  fail('Docker Compose v2 was not found. Install Docker Engine (or Docker Desktop), or Podman with "podman compose", then retry.');
}
function run(c,dir,args){
  const r=spawnSync(c.cmd,[...c.pre,'-f',join(dir,'deploy','compose.yaml'),...args],{cwd:dir,stdio:'inherit',windowsHide:true});
  if(r.status!==0)fail(`"${[c.cmd,...c.pre,...args].join(' ')}" failed (exit ${r.status}).`);
}

function configureEmbedder(dir,opt){
  const kind=opt.embedder;
  if(!EMBEDDERS.includes(kind))fail('--embedder must be bundled, host-ollama or openai.');
  let env=readFileSync(envPath(dir),'utf8'),block;
  if(kind==='bundled'){block=embedBlock('bundled',{model:opt['embed-model']});env=setEnv(env,'HEURCHAIN_EMBEDDER_COMPOSE','compose.ollama.yaml');}
  else if(kind==='host-ollama'){
    block=embedBlock('host-ollama',{ollamaUrl:httpUrl(opt['ollama-url']||'http://host.docker.internal:11434','--ollama-url'),model:opt['embed-model']});
    env=setEnv(env,'HEURCHAIN_EMBEDDER_COMPOSE','compose.no-embedder.yaml');
  }else{
    if(!opt['embed-url']||!opt['embed-model'])fail('--embedder openai needs --embed-url (the full /v1/embeddings URL) and --embed-model.');
    block=embedBlock('openai',{url:httpUrl(opt['embed-url'],'--embed-url'),model:opt['embed-model']});
    env=setEnv(env,'HEURCHAIN_EMBEDDER_COMPOSE','compose.no-embedder.yaml');
    const key=(process.env.HEURCHAIN_EMBED_API_KEY||'').trim();
    // The broker requires a non-empty key for provider openai (broker/embed.go),
    // even for keyless local endpoints (TEI, vLLM, Ollama's /v1/embeddings), which
    // ignore the Authorization header. Without HEURCHAIN_EMBED_API_KEY a
    // placeholder is written; a real OpenAI endpoint rejects it with a clear 401.
    if(/[\r\n]/.test(key))fail('HEURCHAIN_EMBED_API_KEY must be a single line.');
    if(key)env=setEnv(env,'OPENAI_API_KEY',key);
    else if(!/^OPENAI_API_KEY=\S/m.test(env)){env=setEnv(env,'OPENAI_API_KEY','unused-keyless-endpoint');console.log('  No HEURCHAIN_EMBED_API_KEY given: wrote a placeholder key (fine for keyless local endpoints).');}
  }
  env=setEnv(env,'HEURCHAIN_EMBEDDER',kind);
  writeFileSync(envPath(dir),env,{mode:0o600});try{chmodSync(envPath(dir),0o600);}catch{}
  const yamlFile=join(dir,'deploy','broker.yaml');
  writeFileSync(yamlFile,setEmbed(readFileSync(yamlFile,'utf8'),block));
  return kind;
}

function init(dir,opt){
  const existing=existsSync(dir)&&readdirSync(dir).length>0;
  if(existing&&!existsSync(join(dir,MARK)))fail(`${dir} is not empty and was not created by "heurchain init". Choose an empty or new directory.`);
  mkdirSync(dir,{recursive:true});
  // Keep user-owned files across upgrades; everything else is replaced by this version.
  const kept={};for(const f of KEEP)if(existsSync(join(dir,f)))kept[f]=readFileSync(join(dir,f));
  for(const part of PARTS)if(existsSync(join(PKG,part)))cpSync(join(PKG,part),join(dir,part),{recursive:true,force:true});
  const shippedYaml=readFileSync(join(dir,'deploy','broker.yaml'));
  for(const [f,data] of Object.entries(kept))writeFileSync(join(dir,f),data);
  if(kept['deploy/broker.yaml']&&!kept['deploy/broker.yaml'].equals(shippedYaml))writeFileSync(join(dir,'deploy','broker.yaml.new'),shippedYaml);
  if(existsSync(join(dir,'deploy','gitignore.template')))copyFileSync(join(dir,'deploy','gitignore.template'),join(dir,'deploy','.gitignore'));
  copyFileSync(join(PKG,'LICENSE'),join(dir,'LICENSE'));
  let created=false;
  if(!kept['deploy/.env']){
    let text=readFileSync(join(dir,'deploy','.env.example'),'utf8');
    text=text.replace(/^ADMIN_USER=.*$/m,'ADMIN_USER=admin').replace(/^ADMIN_PASS=.*$/m,'ADMIN_PASS='+randomBytes(24).toString('base64url'));
    if(opt.port){if(!/^[0-9]{2,5}$/.test(opt.port))fail('--port must be a port number.');text=setEnv(text,'HEURCHAIN_PORT',opt.port);}
    writeFileSync(envPath(dir),text,{mode:0o600});try{chmodSync(envPath(dir),0o600);}catch{}
    created=true;
  }
  const embedder=opt.embedder?configureEmbedder(dir,opt):(readEnv(dir).HEURCHAIN_EMBEDDER||'bundled');
  writeFileSync(join(dir,MARK),JSON.stringify({package:'heurchain',version:VERSION,updated_at:new Date().toISOString()},null,2)+'\n');
  const port=readEnv(dir).HEURCHAIN_PORT||'8080';
  console.log(`HeurChain ${VERSION} ${existing?'updated':'unpacked'} in ${dir}`);
  console.log(created?`  deploy/.env created with a random admin password (mode 0600; it is not printed).`:`  deploy/.env kept.`);
  console.log(`  embedder: ${embedder}${embedder==='bundled'?' (Ollama in the stack)':''}`);
  if(opt.embedder&&existing)console.log('  Changing the embedder of a store that already holds memories leaves their vectors in the old model\'s space; re-store them or start a new store.');
  if(existsSync(join(dir,'deploy','broker.yaml.new')))console.log('  deploy/broker.yaml kept; this version\'s default is in deploy/broker.yaml.new.');
  console.log(`\nNext:\n  npx heurchain up ${dir}\n  npx heurchain tenant ${dir} --name "My team" --email me@example.com\n  npx heurchain connect --client claude --tenant <tenant_id> --url http://127.0.0.1:${port} --apply`);
}

async function waitReady(base,seconds=300){
  const until=Date.now()+seconds*1000;
  while(Date.now()<until){
    try{const r=await fetch(base+'/ready',{signal:AbortSignal.timeout(3000)});if(r.ok){const j=await r.json();if(j.status==='ready')return j;}}catch{}
    await new Promise(r=>setTimeout(r,2000));
  }
  fail(`the broker at ${base} did not become ready within ${seconds}s. Check: docker compose -f deploy/compose.yaml logs broker`);
}

// host-ollama: make sure the model is in the Ollama on this machine. The broker
// reaches it as host.docker.internal; this process reaches it on loopback.
async function hostOllamaModel(dir){
  const yaml=readFileSync(join(dir,'deploy','broker.yaml'),'utf8');
  const url=(yaml.match(/^\s*ollama_url:\s*"?([^"\n]+)"?/m)||[])[1]||'http://host.docker.internal:11434';
  const model=(yaml.match(/^\s*ollama_model:\s*"?([^"\n]+)"?/m)||[])[1]||DEFAULT_MODEL;
  const local=url.replace('host.docker.internal','127.0.0.1');
  let tags;
  try{tags=await (await fetch(local+'/api/tags',{signal:AbortSignal.timeout(5000)})).json();}
  catch{fail(`no Ollama answered at ${local}. Start Ollama on this machine (ollama serve), or use --embedder bundled.`);}
  if((tags.models||[]).some(m=>m.name===model||m.name===model+':latest')){console.log(`Ollama on this machine has ${model}.`);return;}
  console.log(`Pulling ${model} into the Ollama on this machine...`);
  const r=await fetch(local+'/api/pull',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:model,stream:false}),signal:AbortSignal.timeout(30*60*1000)});
  if(!r.ok)fail(`Ollama could not pull ${model} (HTTP ${r.status}).`);
}

async function main(){
  const [command,...rest]=process.argv.slice(2);
  if(!command||['-h','--help','help'].includes(command)){console.log(readFileSync(fileURLToPath(import.meta.url),'utf8').split('\n').slice(1,21).map(l=>l.replace(/^\/\/ ?/,'')).join('\n'));return;}
  if(['-v','--version'].includes(command)){console.log(VERSION);return;}
  if(Number(process.versions.node.split('.')[0])<20)fail('Node 20 or later is required.');
  if(command==='connect'){
    const r=spawnSync(process.execPath,[join(PKG,'tools','setup','cli.mjs'),'connect',...rest],{stdio:'inherit'});
    process.exit(r.status??1);
  }
  const {pos,opt}=parse(rest),dir=dirOf(pos);
  if(command==='init')return init(dir,opt);
  if(command==='up'){
    const env=readEnv(dir),c=compose(),embedder=env.HEURCHAIN_EMBEDDER||'bundled';
    if(embedder==='host-ollama')await hostOllamaModel(dir);
    // Without --build, Compose pulls the prebuilt broker image and builds from
    // source only if it cannot be pulled.
    run(c,dir,opt.build?['up','-d','--build']:['up','-d']);
    if(embedder==='bundled')run(c,dir,['exec','-T','ollama','ollama','pull',DEFAULT_MODEL]);
    const ready=await waitReady(baseUrl(env));
    console.log(`HeurChain is ready at ${baseUrl(env)} (${JSON.stringify(ready)}).\nNext: npx heurchain tenant ${dir} --name "My team" --email me@example.com`);
    return;
  }
  if(command==='down'){run(compose(),dir,['down']);return;}
  if(command==='status'){
    const env=readEnv(dir),base=baseUrl(env);
    try{const h=await (await fetch(base+'/health',{signal:AbortSignal.timeout(5000)})).json();console.log(JSON.stringify({url:base,embedder:env.HEURCHAIN_EMBEDDER||'bundled',...h}));}
    catch{fail(`no broker answered at ${base}. Start it with: npx heurchain up ${dir}`);}
    return;
  }
  if(command==='tenant'){
    if(!opt.name||!opt.email)fail('tenant needs --name and --email.');
    const env=readEnv(dir),base=baseUrl(env);
    if(!env.ADMIN_USER||!env.ADMIN_PASS)fail('ADMIN_USER and ADMIN_PASS are not set in deploy/.env.');
    const r=await fetch(base+'/admin/tenants',{method:'POST',headers:{'Content-Type':'application/json',Authorization:'Basic '+Buffer.from(env.ADMIN_USER+':'+env.ADMIN_PASS).toString('base64')},body:JSON.stringify({name:opt.name,email:opt.email})}).catch(()=>null);
    if(!r)fail(`no broker answered at ${base}. Start it with: npx heurchain up ${dir}`);
    const j=await r.json().catch(()=>({}));
    if(!r.ok||!j.api_key)fail(`tenant creation failed (HTTP ${r.status}): ${j.error||'unknown error'}`);
    if(opt.json){console.log(JSON.stringify({tenant_id:j.tenant_id,api_key:j.api_key}));return;}
    console.log(`Tenant created. Save the API key now; it is shown only once.\n  tenant_id: ${j.tenant_id}\n  api_key:   ${j.api_key}\n\nConnect an agent (the key is read from the environment, never from arguments):\n  HEURCHAIN_API_KEY=<api_key> npx heurchain connect --client claude --tenant ${j.tenant_id} --url ${base} --apply`);
    return;
  }
  fail(`unknown command "${command}". Run "npx heurchain --help".`);
}
if(process.argv[1]&&/heurchain(\.mjs)?$/.test(process.argv[1].replaceAll('\\','/')))main();
