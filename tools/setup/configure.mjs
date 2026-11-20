// Secret-free configuration writer. Every target is explicit; conflicts fail closed.
//
// transact() applies a pure text transform to one file under a lock, validates
// the merged result before anything is replaced, keeps a private backup and a
// receipt, and swaps the file in with an atomic rename only if the target did
// not change underneath. rollback() restores a backup taken by transact().
import {readFile,lstat,realpath,open,rename,unlink,mkdir} from 'node:fs/promises';
import {resolve,dirname,basename} from 'node:path';
import {createHash,randomUUID} from 'node:crypto';

// Hook settings, the skill, the copied runtimes and their secret-free state.
// MCP entries are written by each client's own `mcp add` command, not here.
export const SUPPORTED_TARGETS=['settings.json','hooks.json','SKILL.md','heurchain-recall-hook.mjs','heurchain-mcp-bridge.mjs','client.json'];
const digest=value=>createHash('sha256').update(value).digest('hex');

async function snapshot(file){
  try{const stat=await lstat(file);if(!stat.isFile()||stat.isSymbolicLink()||stat.nlink!==1)throw Error('Target must be a regular, unlinked file.');return {exists:true,text:await readFile(file,'utf8'),mode:stat.mode&0o777};}
  catch(error){if(error.code==='ENOENT')return {exists:false,text:'',mode:0o600};throw error;}
}
async function exclusive(file,text,mode=0o600){const handle=await open(file,'wx',mode);try{await handle.writeFile(text);await handle.sync();}finally{await handle.close();}}
async function target(file){
  const absolute=resolve(file);
  // Refuse indirect targets (a symlinked parent) and anything outside the allowlist.
  if(resolve(await realpath(dirname(absolute)))!==dirname(absolute))throw Error('Symlinked parent requires manual review.');
  if(!SUPPORTED_TARGETS.includes(basename(absolute)))throw Error('Use an explicit supported configuration filename.');
  return absolute;
}

// validate(after, before) throws to stop the change before the file is touched.
export async function transact(file,transform,{apply=false,createParents=false,validate}={}){
  if(createParents&&apply)await mkdir(dirname(resolve(file)),{recursive:true});
  if(!apply&&createParents){
    try{await lstat(dirname(resolve(file)));}
    catch(e){if(e.code==='ENOENT'){const after=transform('');validate?.(after,'');return {changed:after!=='',applied:false};}throw e;}
  }
  file=await target(file);
  const lock=file+'.hc-setup.lock',id=randomUUID(),backup=file+'.hc-setup-'+id+'.bak',receipt=backup+'.json',temporary=file+'.hc-setup-'+id+'.tmp';
  let handle;
  try{handle=await open(lock,'wx',0o600);}catch(e){if(e.code==='EEXIST')throw Error('Another setup run holds '+lock+'; if none is running, delete that lock file.');throw e;}
  try{
    const before=await snapshot(file),after=transform(before.text);
    if(after===before.text)return {changed:false};
    validate?.(after,before.text);
    if(!apply)return {changed:true,applied:false};
    await exclusive(backup,before.text); // Backups may hold preexisting secrets: mode 0600, never printed.
    await exclusive(receipt,JSON.stringify({file,beforeExists:before.exists,beforeHash:digest(before.text),afterHash:digest(after),mode:before.mode})+'\n');
    await exclusive(temporary,after,before.mode);
    const current=await snapshot(file);
    if(current.exists!==before.exists||digest(current.text)!==digest(before.text))throw Error('Configuration changed concurrently; target left untouched.');
    await rename(temporary,file);
    return {changed:true,applied:true,backup};
  }finally{await handle.close();await unlink(temporary).catch(e=>{if(e.code!=='ENOENT')throw e;});await unlink(lock);}
}

export async function rollback(file,backup){
  file=await target(file);backup=resolve(backup);
  if(dirname(backup)!==dirname(file)||!basename(backup).startsWith(basename(file)+'.hc-setup-')||!backup.endsWith('.bak'))throw Error('Backup must belong to this exact target.');
  const lock=file+'.hc-setup.lock',handle=await open(lock,'wx',0o600),temporary=file+'.hc-rollback-'+randomUUID()+'.tmp';
  try{
    const record=JSON.parse((await snapshot(backup+'.json')).text),saved=await snapshot(backup),current=await snapshot(file);
    if(record.file!==file||!saved.exists||digest(saved.text)!==record.beforeHash||!current.exists||digest(current.text)!==record.afterHash)throw Error('Target or backup changed; rollback requires manual review.');
    if(record.beforeExists){await exclusive(temporary,saved.text,record.mode);await rename(temporary,file);}else await unlink(file);
    return {restored:true};
  }finally{await handle.close();await unlink(temporary).catch(e=>{if(e.code!=='ENOENT')throw e;});await unlink(lock);}
}
