#!/usr/bin/env node
// Prints the prebuilt broker image tag for the committed broker source:
// "src-" + the first 12 hex of sha256(git tree of broker/ + git blob of
// deploy/Dockerfile). The image workflow pushes this tag; the npm bundle pins
// compose.yaml to it, so a package pulls exactly the image built from the
// source it ships. Prints nothing (exit 1) outside a git checkout or when
// broker/ or the Dockerfile has uncommitted changes, so a local build is used.
import {execFileSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {dirname,join} from 'node:path';
import {fileURLToPath} from 'node:url';

export function imageTag(root=join(dirname(fileURLToPath(import.meta.url)),'..')){
  const git=(...args)=>execFileSync('git',args,{cwd:root,encoding:'utf8',stdio:['ignore','pipe','ignore']}).trim();
  try{
    if(git('status','--porcelain','--','broker','deploy/Dockerfile'))return null;
    const tree=git('rev-parse','HEAD:broker'),dockerfile=git('rev-parse','HEAD:deploy/Dockerfile');
    return 'src-'+createHash('sha256').update(tree+'\n'+dockerfile+'\n').digest('hex').slice(0,12);
  }catch{return null;}
}

if(process.argv[1]&&/image-tag\.mjs$/.test(process.argv[1].replaceAll('\\','/'))){
  const tag=imageTag();
  if(!tag){console.error('image-tag: not a clean git checkout of broker/ and deploy/Dockerfile');process.exit(1);}
  console.log(tag);
}
