// Copies everything a user needs to run self-hosted HeurChain into the npm
// package directory, from the repository it is packed in. Runs as `prepack`,
// so `npm pack` and `npm publish` always ship the repository's current broker,
// Compose stack, agent setup tool, skill and docs. The copies are gitignored
// (npm/.gitignore); the repository root stays the only source.
import {cpSync,rmSync,existsSync,readFileSync,writeFileSync,statSync} from 'node:fs';
import {dirname,join,relative,basename,sep} from 'node:path';
import {fileURLToPath,pathToFileURL} from 'node:url';

const pkg=join(dirname(fileURLToPath(import.meta.url)),'..'),root=join(pkg,'..');
// [source in the repository, destination in the package]
const COPY=[
  ['broker','broker'],
  ['deploy','deploy'],
  ['tools/setup','tools/setup'],
  ['skills','skills'],
  ['docs/install.md','docs/install.md'],
  ['docs/agent-setup.md','docs/agent-setup.md'],
  ['docs/charts/token-savings.png','docs/charts/token-savings.png'],
];
// Never ship local secrets, databases, binaries or test-only files.
const SKIP=[/(^|[\\/])\.env$/,/\.env\.local$/,/\.db(-wal|-shm)?$/,/\.exe$/,/_test\.go$/,/[\\/]testdata([\\/]|$)/,/\.hc-setup/,/[\\/]node_modules([\\/]|$)/];
const BINARIES=new Set(['broker','heurchain-broker']);

for(const [from,to] of COPY){
  const src=join(root,from),dst=join(pkg,to);
  if(!existsSync(src))throw Error(`bundle: ${from} is missing; pack from a full repository checkout.`);
  rmSync(dst,{recursive:true,force:true});
  cpSync(src,dst,{recursive:true,filter:p=>{
    const rel=relative(root,p);
    if(SKIP.some(re=>re.test(rel)))return false;
    if(statSync(p).isFile()&&BINARIES.has(basename(p))&&rel.startsWith('broker'+sep))return false;
    return true;
  }});
}
// npm drops files named .gitignore and .dockerignore is harmless; keep the
// deploy secret guard under a name npm ships, and restore it at init.
if(existsSync(join(pkg,'deploy','.gitignore'))){
  writeFileSync(join(pkg,'deploy','gitignore.template'),readFileSync(join(pkg,'deploy','.gitignore')));
  rmSync(join(pkg,'deploy','.gitignore'));
}
// Pin the prebuilt broker image to the one built from exactly this broker
// source (deploy/image-tag.mjs). Without a clean checkout, use a tag that does
// not exist, so Compose builds the shipped source instead of pulling another one.
const {imageTag}=await import(pathToFileURL(join(root,'deploy','image-tag.mjs')).href);
const tag=imageTag(root)||'unpublished-local-build';
const composeFile=join(pkg,'deploy','compose.yaml');
const compose=readFileSync(composeFile,'utf8'),pinned=compose.replace('${HEURCHAIN_BROKER_TAG:-master}','${HEURCHAIN_BROKER_TAG:-'+tag+'}');
if(pinned===compose)throw Error('bundle: compose.yaml has no HEURCHAIN_BROKER_TAG default to pin.');
writeFileSync(composeFile,pinned);
console.log('bundle: broker image tag '+tag);
for(const must of ['broker/main.go','broker/go.mod','broker/schema.sql','deploy/compose.yaml','deploy/Dockerfile','deploy/.env.example','tools/setup/cli.mjs','skills/heurchain-memory/SKILL.md'])
  if(!existsSync(join(pkg,must)))throw Error('bundle: expected '+must+' in the package.');
console.log('bundle: copied broker, deploy, tools/setup, skills and docs into the package');
