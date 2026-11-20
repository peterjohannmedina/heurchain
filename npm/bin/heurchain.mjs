#!/usr/bin/env node
// heurchain: install and run self-hosted HeurChain from the npm package.
//
//   npx heurchain init [DIR]          unpack the server (broker source, Compose stack,
//                                     agent setup, skill, docs) and write a private .env
//   npx heurchain up [DIR]            build and start the stack, pull the embedding model,
//                                     wait until the broker is ready
//   npx heurchain tenant [DIR] --name N --email E
//                                     create a tenant and API key (the key is shown once)
//   npx heurchain connect ...         agent setup (MCP entry, skill, recall hook);
//                                     same options as tools/setup/cli.mjs connect
//   npx heurchain status|down [DIR]   broker health / stop the stack (data is kept)
//
// Needs Node 20+ and Docker Engine with Compose v2 (or Podman with `podman compose`).
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

function fail(message,code=1){console.error('heurchain: '+message);process.exit(code);}
function parse(argv){
  const pos=[],opt={};
  for(let i=0;i<argv.length;i++){
    const a=argv[i];
    if(a.startsWith('--')){const k=a.slice(2);if(['json','yes'].includes(k))opt[k]=true;else{if(argv[i+1]===undefined)fail('missing value for '+a);opt[k]=argv[++i];}}
    else pos.push(a);
  }
  return {pos,opt};
}
const dirOf=pos=>resolve(pos[0]||'heurchain-server');
function readEnv(dir){
  const file=join(dir,'deploy','.env');
  if(!existsSync(file))fail(`${file} not found. Run "heurchain init ${dir}" first.`);
  return Object.fromEntries(readFileSync(file,'utf8').split(/\r?\n/).filter(l=>/^[A-Z_][A-Z0-9_]*=/.test(l)).map(l=>[l.slice(0,l.indexOf('=')),l.slice(l.indexOf('=')+1)]));
}
const baseUrl=env=>`http://127.0.0.1:${env.HEURCHAIN_PORT||'8080'}`;

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
  const envFile=join(dir,'deploy','.env');let created=false;
  if(!kept['deploy/.env']){
    let text=readFileSync(join(dir,'deploy','.env.example'),'utf8');
    text=text.replace(/^ADMIN_USER=.*$/m,'ADMIN_USER=admin').replace(/^ADMIN_PASS=.*$/m,'ADMIN_PASS='+randomBytes(24).toString('base64url'));
    if(opt.port){if(!/^[0-9]{2,5}$/.test(opt.port))fail('--port must be a port number.');text=text.replace(/^# HEURCHAIN_PORT=.*$/m,'HEURCHAIN_PORT='+opt.port);if(!/^HEURCHAIN_PORT=/m.test(text))text+=`\nHEURCHAIN_PORT=${opt.port}\n`;}
    writeFileSync(envFile,text,{mode:0o600});try{chmodSync(envFile,0o600);}catch{}
    created=true;
  }
  writeFileSync(join(dir,MARK),JSON.stringify({package:'heurchain',version:VERSION,updated_at:new Date().toISOString()},null,2)+'\n');
  const port=readEnv(dir).HEURCHAIN_PORT||'8080';
  console.log(`HeurChain ${VERSION} ${existing?'updated':'unpacked'} in ${dir}`);
  console.log(created?`  deploy/.env created with a random admin password (mode 0600; it is not printed).`:`  deploy/.env kept.`);
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

async function main(){
  const [command,...rest]=process.argv.slice(2);
  if(!command||['-h','--help','help'].includes(command)){console.log(readFileSync(fileURLToPath(import.meta.url),'utf8').split('\n').slice(1,15).map(l=>l.replace(/^\/\/ ?/,'')).join('\n'));return;}
  if(['-v','--version'].includes(command)){console.log(VERSION);return;}
  if(Number(process.versions.node.split('.')[0])<20)fail('Node 20 or later is required.');
  if(command==='connect'){
    const r=spawnSync(process.execPath,[join(PKG,'tools','setup','cli.mjs'),'connect',...rest],{stdio:'inherit'});
    process.exit(r.status??1);
  }
  const {pos,opt}=parse(rest),dir=dirOf(pos);
  if(command==='init')return init(dir,opt);
  if(command==='up'){
    const env=readEnv(dir),c=compose();
    run(c,dir,['up','-d','--build']);
    run(c,dir,['exec','-T','ollama','ollama','pull','nomic-embed-text']);
    const ready=await waitReady(baseUrl(env));
    console.log(`HeurChain is ready at ${baseUrl(env)} (${JSON.stringify(ready)}).\nNext: npx heurchain tenant ${dir} --name "My team" --email me@example.com`);
    return;
  }
  if(command==='down'){run(compose(),dir,['down']);return;}
  if(command==='status'){
    const env=readEnv(dir),base=baseUrl(env);
    try{const h=await (await fetch(base+'/health',{signal:AbortSignal.timeout(5000)})).json();console.log(JSON.stringify({url:base,...h}));}
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
main();
