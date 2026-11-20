// node --test npm/test/  (not shipped in the package)
import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {setEnv,embedBlock,setEmbed} from '../bin/heurchain.mjs';

const yaml=readFileSync(new URL('../../deploy/broker.yaml',import.meta.url),'utf8');

test('setEnv replaces, uncomments or appends', () => {
  assert.equal(setEnv('A=1\n# B=2\n','B','3'),'A=1\nB=3\n');
  assert.equal(setEnv('A=1\n','A','2'),'A=2\n');
  assert.equal(setEnv('A=1','C','x'),'A=1\nC=x\n');
});

test('embed block swaps only the embed: section of the shipped broker.yaml', () => {
  for(const [kind,opts] of [['host-ollama',{ollamaUrl:'http://host.docker.internal:11434'}],['openai',{url:'http://emb:8090/v1/embeddings',model:'BAAI/bge-base-en-v1.5'}],['bundled',{}]]){
    const out=setEmbed(yaml,embedBlock(kind,opts));
    const before=yaml.replace(/\r\n/g,'\n').split('\n').filter(l=>!/^embed:|^  (provider|ollama_url|ollama_model|openai_url|openai_model):/.test(l));
    const after=out.split('\n').filter(l=>!/^embed:|^  (provider|ollama_url|ollama_model|openai_url|openai_model):/.test(l));
    assert.deepEqual(after,before,kind+': everything outside embed: is unchanged');
    assert.equal((out.match(/^embed:/gm)||[]).length,1);
  }
  const o=setEmbed(yaml,embedBlock('openai',{url:'http://e/v1/embeddings',model:'m'}));
  assert.match(o,/provider: openai\n  ollama_url: ""\n  openai_url: "http:\/\/e\/v1\/embeddings"\n  openai_model: "m"/);
  assert.equal(setEmbed(yaml,embedBlock('bundled')),yaml.replace(/\r\n/g,'\n'),'bundled restores the shipped block exactly');
});
