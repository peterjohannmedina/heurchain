// Stores a client's broker API key where the bridge, headers helper and recall
// hook find it after a restart: a per-client user environment variable,
// HEURCHAIN_LOCAL_<CLIENT>_KEY. On Windows that is HKCU\Environment; elsewhere a
// private file, ~/.heurchain/credentials.env (mode 0600). The value is passed on
// stdin or written to the private file, never on a command line, and never
// printed or logged.
import {spawn} from 'node:child_process';
import {mkdir,writeFile,readFile,chmod} from 'node:fs/promises';
import {join,dirname} from 'node:path';
import os from 'node:os';

export const VARIABLE=/^HEURCHAIN_LOCAL_[A-Z0-9]{2,24}_KEY$/;
// Keys minted by POST /admin/tenants on the 0.6.1-oss.1 broker.
export const KEY=/^hc_[0-9a-f]{32}$/;
export const variableFor=client=>'HEURCHAIN_LOCAL_'+client.toUpperCase()+'_KEY';

function powershell(script,{variable,input}){
  return new Promise((resolve,reject)=>{
    const p=spawn('powershell.exe',['-NoProfile','-NonInteractive','-Command',script],
      {env:{...process.env,HC_VARIABLE_NAME:variable},stdio:[input===undefined?'ignore':'pipe','pipe','ignore'],windowsHide:true});
    let out='';p.stdout.on('data',d=>out+=d);
    p.on('error',reject);p.on('close',code=>code===0?resolve(out):reject(Error('Could not update the Windows user environment.')));
    if(input!==undefined)p.stdin.end(input+'\n');
  });
}
// [Environment]::SetEnvironmentVariable(..., 'User') writes HKCU\Environment and
// broadcasts the change, so newly started processes see it.
const setUser=(variable,value)=>powershell("$v=[Console]::In.ReadLine(); [Environment]::SetEnvironmentVariable($env:HC_VARIABLE_NAME,$v,'User'); if([Environment]::GetEnvironmentVariable($env:HC_VARIABLE_NAME,'User') -ne $v){exit 3}",{variable,input:value});
const removeUser=variable=>powershell("[Environment]::SetEnvironmentVariable($env:HC_VARIABLE_NAME,$null,'User'); if($null -ne [Environment]::GetEnvironmentVariable($env:HC_VARIABLE_NAME,'User')){exit 3}",{variable});
const hasUser=async variable=>(await powershell("if($null -ne [Environment]::GetEnvironmentVariable($env:HC_VARIABLE_NAME,'User')){'yes'}",{variable})).trim()==='yes';

const envFile=home=>join(home,'.heurchain','credentials.env');

export async function persistKey(variable,value,{platform=process.platform,home=os.homedir(),setUserVariable=setUser}={}){
  if(!VARIABLE.test(variable)||!KEY.test(value))throw Error('Refusing to store an unexpected variable name or key format (expected hc_ followed by 32 hex characters).');
  if(platform==='win32'){
    await setUserVariable(variable,value);
    return {method:'windows-user-environment',location:'HKCU\\Environment\\'+variable,
      note:'Readable by programs running as your Windows user. Programs started from a terminal opened before setup keep their old environment; the bridge, headers helper and hook read the registry directly.'};
  }
  const file=envFile(home);
  await mkdir(dirname(file),{recursive:true,mode:0o700});
  let lines=[];try{lines=(await readFile(file,'utf8')).split('\n').filter(l=>l&&!l.startsWith(`export ${variable}=`));}catch(e){if(e.code!=='ENOENT')throw e;}
  lines.push(`export ${variable}='${value}'`);
  await writeFile(file,lines.join('\n')+'\n',{mode:0o600});await chmod(file,0o600);
  return {method:'private-env-file',location:file,note:'File mode 0600. The bridge, headers helper and hook read this file directly; no shell profile change is needed.'};
}

export async function removeKey(variable,{platform=process.platform,home=os.homedir(),removeUserVariable=removeUser}={}){
  if(!VARIABLE.test(variable))throw Error('Refusing to remove an unexpected variable name.');
  if(platform==='win32'){await removeUserVariable(variable);return {removed:true,location:'HKCU\\Environment\\'+variable};}
  const file=envFile(home);let text;
  try{text=await readFile(file,'utf8');}catch(e){if(e.code==='ENOENT')return {removed:false,location:file};throw e;}
  const lines=text.split('\n').filter(Boolean),kept=lines.filter(l=>!l.startsWith(`export ${variable}=`));
  if(kept.length!==lines.length){await writeFile(file,kept.length?kept.join('\n')+'\n':'',{mode:0o600});await chmod(file,0o600);}
  return {removed:kept.length!==lines.length,location:file};
}

// Presence only; the value is never returned.
export async function hasKey(variable,{platform=process.platform,home=os.homedir(),hasUserVariable=hasUser}={}){
  if(!VARIABLE.test(variable))return false;
  if(platform==='win32')return hasUserVariable(variable);
  try{return (await readFile(envFile(home),'utf8')).split('\n').some(l=>l.startsWith(`export ${variable}=`));}catch{return false;}
}
