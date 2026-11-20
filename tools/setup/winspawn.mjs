// Start a client CLI (claude, codex) with arguments that arrive intact on Windows.
//
// npm installs `claude` as claude.cmd; other installers ship .exe or .cmd launchers. Node cannot run
// a .cmd without cmd.exe, and `shell: true` passes arguments to cmd.exe without
// quoting, so JSON lost its quotes ("Invalid configuration: not a JSON object")
// and "X-HeurChain-Tenant: <id>" split at the space (2026-11-19 live Claude Code
// setup). This resolves the launcher on PATH and, for batch files, builds one
// cmd.exe command line with every argument quoted for the C runtime and
// caret-escaped for cmd.exe (twice for batch files, which re-parse it): the
// scheme cross-spawn uses. Elsewhere it is a plain spawn with no shell.
import {spawn} from 'node:child_process';
import {existsSync,statSync} from 'node:fs';
import {join,delimiter,extname} from 'node:path';

const META=/([()\][%!^"`<>&|;, *?])/g;
export function escapeCommand(command){return command.replace(META,'^$1');}
export function escapeArgument(arg,doubleEscape=false){
  let a=String(arg);
  // C runtime rules: backslashes before a quote are doubled, the quote escaped;
  // trailing backslashes are doubled so the closing quote stays a quote.
  a=a.replace(/(\\*)"/g,'$1$1\\"').replace(/(\\*)$/,'$1$1');
  a='"'+a+'"';
  a=a.replace(META,'^$1');
  if(doubleEscape)a=a.replace(META,'^$1');
  return a;
}

// First match on PATH in PATHEXT order, so claude.exe beats claude.cmd.
export function resolveCommand(name,{env=process.env,platform=process.platform}={}){
  if(platform!=='win32')return name;
  if(/[\\/]/.test(name)&&existsSync(name))return name;
  const exts=(env.PATHEXT||'.COM;.EXE;.BAT;.CMD').split(';').filter(Boolean).map(e=>e.toLowerCase());
  for(const dir of String(env.PATH||env.Path||'').split(delimiter).filter(Boolean)){
    for(const ext of exts){
      const candidate=join(dir,name+ext);
      try{if(existsSync(candidate)&&statSync(candidate).isFile())return candidate;}catch{}
    }
  }
  return null;
}

// Returns a ChildProcess. Rejects nothing itself: a missing command surfaces as
// an 'error' event (ENOENT), like spawn.
export function spawnClient(name,args,{env=process.env,platform=process.platform,stdio=['ignore','pipe','pipe']}={}){
  if(platform!=='win32')return spawn(name,args,{env,stdio,windowsHide:true});
  const file=resolveCommand(name,{env,platform})||name;
  const ext=extname(file).toLowerCase();
  if(ext==='.exe'||ext==='.com')return spawn(file,args,{env,stdio,windowsHide:true});
  const batch=ext==='.cmd'||ext==='.bat';
  const line=[escapeCommand(file),...args.map(a=>escapeArgument(a,batch))].join(' ');
  return spawn(env.ComSpec||env.COMSPEC||'cmd.exe',['/d','/s','/c','"'+line+'"'],{env,stdio,windowsHide:true,windowsVerbatimArguments:true});
}
