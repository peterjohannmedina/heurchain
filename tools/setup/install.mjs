// Files setup owns beyond the MCP entry: the heurchain-memory skill, the copied
// bridge and recall hook runtimes, their secret-free state, and the hook
// registration in the client's hook settings. Every write goes through
// configure.mjs transact(): dry run by default, lock, validation, private
// backup + receipt, atomic rename. Nothing here reads, prints or stores a
// credential value.
import {readFile,unlink,rmdir} from 'node:fs/promises';
import {existsSync} from 'node:fs';
import {join,dirname,resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import os from 'node:os';
import {transact} from './configure.mjs';

const here=dirname(fileURLToPath(import.meta.url));
export const HOOK_FILE='heurchain-recall-hook.mjs',BRIDGE_FILE='heurchain-mcp-bridge.mjs',STATE_FILE='client.json';
export const CLIENTS=['claude','codex'];
const sha=text=>createHash('sha256').update(text).digest('hex');
const slash=p=>p.replaceAll('\\','/');

// Self-hosted files live under ~/.heurchain/local/<client>/, apart from any
// hosted HeurChain setup in ~/.heurchain/<client>/, so neither replaces the other.
export function locations(client,{home=os.homedir(),env=process.env}={}){
  const dir=join(home,'.heurchain','local',client);
  const common={dir,hookScript:join(dir,HOOK_FILE),bridgeScript:join(dir,BRIDGE_FILE),stateFile:join(dir,STATE_FILE)};
  if(client==='claude'){
    const config=env.CLAUDE_CONFIG_DIR||join(home,'.claude');
    return {...common,skillFile:join(config,'skills','heurchain-memory','SKILL.md'),hooksFile:join(config,'settings.json')};
  }
  // Codex documents $HOME/.agents/skills as the personal skill folder.
  if(client==='codex')return {...common,skillFile:join(home,'.agents','skills','heurchain-memory','SKILL.md'),hooksFile:join(env.CODEX_HOME||join(home,'.codex'),'hooks.json')};
  throw Error('Unsupported client: '+client);
}

async function firstReadable(names){
  for(const name of names){try{return (await readFile(resolve(here,name),'utf8')).replace(/\r\n/g,'\n');}catch(e){if(e.code!=='ENOENT')throw e;}}
  throw Error('Bundled file not found: '+names[0]);
}
export const bundled={
  skill:()=>firstReadable(['../../skills/heurchain-memory/SKILL.md']),
  hook:()=>firstReadable(['recall-hook.mjs']),
  bridge:()=>firstReadable(['mcp-bridge.mjs']),
  template:client=>firstReadable([`templates/recall-hook.${client}.json`]),
};

// One command string that runs unchanged in every shell the clients use for a
// hook or headersHelper `command`: Git Bash or PowerShell for Claude Code on
// Windows, PowerShell for Codex CLI on Windows, sh elsewhere. A quoted first
// token is a string literal in PowerShell, so the program is an unquoted token:
// the absolute Node path when it has no spaces or shell metacharacters,
// otherwise `node` from PATH. Arguments are double-quoted, which bash, sh and
// PowerShell all read as one literal word once ", $ and ` are excluded.
const BARE=/^[A-Za-z0-9_\-.:/~]+$/;
const dq=value=>{const p=slash(value);if(/["$`\r\n]/.test(p))throw Error('Path contains a character that cannot be quoted the same way in bash and PowerShell.');return '"'+p+'"';};
export function nodeOnPath(env=process.env,platform=process.platform){
  const names=platform==='win32'?['node.exe']:['node'];
  for(const dir of String(env.PATH||env.Path||'').split(platform==='win32'?';':':').filter(Boolean))
    for(const name of names)if(existsSync(join(dir,name)))return join(dir,name);
  return null;
}
export function nodeProgram({node=process.execPath,env=process.env,platform=process.platform}={}){
  const absolute=slash(node);
  if(BARE.test(absolute))return absolute;
  if(nodeOnPath(env,platform))return 'node';
  throw Error('Node is installed under a path with spaces and is not on PATH; add Node to PATH so the command runs in every client shell.');
}
export function commandLine(script,state,{node,env,platform,extra=''}={}){
  return `${nodeProgram({node,env,platform})} ${dq(script)} --state ${dq(state)}${extra}`;
}

export function hookEntries(template,{command}){
  return JSON.parse(JSON.stringify(JSON.parse(template).hooks).replaceAll('{{COMMAND}}',JSON.stringify(command).slice(1,-1)));
}

// A hook group is ours when every hook in it runs this setup's copied runtime
// (the exact script path). Hosted HeurChain hooks and anything else are kept.
const groupOf=(group,script)=>Array.isArray(group?.hooks)&&group.hooks.length>0&&group.hooks.every(h=>typeof h?.command==='string'&&slash(h.command).includes(slash(script)));
const otherHeurChain=(group,script)=>Array.isArray(group?.hooks)&&group.hooks.some(h=>typeof h?.command==='string'&&h.command.includes(HOOK_FILE)&&!slash(h.command).includes(slash(script)));

// Replace only this setup's UserPromptSubmit group. Unrelated hooks, other
// events and every other setting are preserved; byte-identical input is kept
// when nothing semantic changed, so repeat runs are no-ops.
export function mergeHooks(original,entries,{script,remove=false}){
  const data=original.trim()?JSON.parse(original):{};
  if(!data||typeof data!=='object'||Array.isArray(data))throw Error('Hook settings must be a JSON object.');
  if(data.hooks!==undefined&&(typeof data.hooks!=='object'||data.hooks===null||Array.isArray(data.hooks)))throw Error('Existing "hooks" is not an object; review it manually.');
  const hooks=data.hooks||{};
  for(const [event,groups] of Object.entries(entries)){
    if(hooks[event]!==undefined&&!Array.isArray(hooks[event]))throw Error(`Existing ${event} hooks are not a list; review them manually.`);
    const kept=(hooks[event]||[]).filter(group=>!groupOf(group,script));
    if(!remove)kept.push(...groups);
    if(kept.length)hooks[event]=kept;else delete hooks[event];
  }
  if(Object.keys(hooks).length)data.hooks=hooks;else delete data.hooks;
  if(!original.trim()&&remove)return original;
  const next=JSON.stringify(data,null,2)+'\n';
  return original.trim()&&JSON.stringify(JSON.parse(original))===JSON.stringify(data)?original:next;
}

// Checked before the merged hook file replaces the old one: it parses, every
// top-level setting and every hook that is not ours is unchanged, and ours
// appears exactly once (or not at all after removal).
export function validateHooks(after,before,{script,expectOurs}){
  const a=after.trim()?JSON.parse(after):{},b=before.trim()?JSON.parse(before):{};
  const strip=data=>{const copy=JSON.parse(JSON.stringify(data));for(const [event,groups] of Object.entries(copy.hooks||{})){if(Array.isArray(groups))copy.hooks[event]=groups.filter(g=>!groupOf(g,script));if(Array.isArray(copy.hooks[event])&&!copy.hooks[event].length)delete copy.hooks[event];}if(copy.hooks&&!Object.keys(copy.hooks).length)delete copy.hooks;return copy;};
  if(JSON.stringify(strip(a))!==JSON.stringify(strip(b)))throw Error('Merged hook settings would change entries setup does not own; nothing was written.');
  const count=Object.values(a.hooks||{}).flat().filter(g=>groupOf(g,script)).length;
  if(count!==(expectOurs?1:0))throw Error('Merged hook settings do not hold exactly the expected HeurChain entry; nothing was written.');
}

export function otherHeurChainHooks(text,script){
  try{const data=text.trim()?JSON.parse(text):{};return Object.values(data.hooks||{}).flat().filter(g=>otherHeurChain(g,script)).length;}catch{return 0;}
}

export function clientState({client,endpoint,tenantId,variable,enabled=true}){
  return {schema_version:1,client,endpoint,tenant_id:tenantId,variable,enabled,timeout_ms:2500,limit:3,min_dense_score:0.55,max_chars:2000};
}

// The skill path is shared with a hosted HeurChain setup. Replace a
// heurchain-memory skill (with a backup); refuse any other file there.
function skillTransform(text){
  return before=>{
    if(before&&!/^---\s*\nname:\s*heurchain-memory\s*\n/.test(before.replace(/\r\n/g,'\n')))throw Error('A different skill exists at the heurchain-memory path; review it manually.');
    return text;
  };
}

export async function installPieces({client,state,skill=true,recallHook,apply=false,home,env,node,platform}){
  const where=locations(client,{home,env}),results=[];
  const step=async(piece,file,transform,validate)=>{const r=await transact(file,transform,{apply,createParents:true,validate});results.push({piece,file,...r});};
  const stateText=JSON.stringify(state,null,2)+'\n';
  if(/\bhc_[0-9a-f]{32}\b/i.test(stateText))throw Error('Refusing to write a credential into client state.');
  // Runtimes and their state come before the entries that run them.
  const bridge=await bundled.bridge(),hook=await bundled.hook();
  await step('mcp-bridge-runtime',where.bridgeScript,()=>bridge);
  await step('client-state',where.stateFile,()=>stateText);
  if(skill)await step('skill',where.skillFile,skillTransform(await bundled.skill()));
  const template=await bundled.template(client),command=commandLine(where.hookScript,where.stateFile,{node,env,platform});
  const entries=hookEntries(template,{command});
  let recall=null;
  if(recallHook){
    const version=/RECALL_HOOK_VERSION = '([^']+)'/.exec(hook)?.[1];
    if(!version)throw Error('Bundled recall hook has no version.');
    await step('recall-hook-runtime',where.hookScript,()=>hook);
    await step('recall-hook-registration',where.hooksFile,text=>mergeHooks(text,entries,{script:where.hookScript}),(after,before)=>validateHooks(after,before,{script:where.hookScript,expectOurs:true}));
    recall={version,sha256:sha(hook),event:'UserPromptSubmit',command,review:JSON.parse(template).review};
  }else{
    // Opting out unregisters a hook an earlier run installed; the copied runtime is removed below.
    await step('recall-hook-registration',where.hooksFile,text=>text?mergeHooks(text,entries,{script:where.hookScript,remove:true}):text,(after,before)=>validateHooks(after,before,{script:where.hookScript,expectOurs:false}));
    if(apply&&existsSync(where.hookScript)){await unlink(where.hookScript);results.push({piece:'recall-hook-runtime',file:where.hookScript,changed:true,applied:true,removed:true});}
  }
  let others=0;try{others=otherHeurChainHooks(await readFile(where.hooksFile,'utf8'),where.hookScript);}catch{}
  return {results,recall_hook:recall,other_heurchain_hooks:others};
}

// Uninstall: unregister the hook, then remove the files setup copied. The skill
// is removed only when it is byte-identical to the bundled copy.
export async function removePieces({client,apply=false,home,env}){
  const where=locations(client,{home,env}),results=[];
  const template=await bundled.template(client),entries=hookEntries(template,{command:'node'});
  const r=await transact(where.hooksFile,text=>text?mergeHooks(text,entries,{script:where.hookScript,remove:true}):text,{apply,createParents:true,validate:(after,before)=>validateHooks(after,before,{script:where.hookScript,expectOurs:false})});
  results.push({piece:'recall-hook-registration',file:where.hooksFile,...r});
  const skill=await bundled.skill();
  let skillNow=null;try{skillNow=(await readFile(where.skillFile,'utf8')).replace(/\r\n/g,'\n');}catch{}
  const files=[where.hookScript,where.bridgeScript,where.stateFile];
  if(skillNow!==null){if(skillNow===skill)files.push(where.skillFile);else results.push({piece:'skill',file:where.skillFile,changed:false,kept:'differs from the bundled skill; left in place'});}
  for(const file of files){
    if(!existsSync(file))continue;
    if(apply)await unlink(file);
    results.push({piece:file===where.skillFile?'skill':'runtime-or-state',file,changed:true,applied:apply,removed:apply});
  }
  if(apply){for(const d of [dirname(where.skillFile),where.dir])await rmdir(d).catch(()=>{});}
  return {results};
}
