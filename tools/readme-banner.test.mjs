// Marked blocks on the GitHub README (the "Latest update" banner, the research
// chart) are mirrored to the npm page (npm/README.md). This fails when a copy
// is missing, the two drift apart, or a link would not resolve on npmjs.com.
import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';

const blocks=async path=>{
  const text=(await readFile(new URL('../'+path,import.meta.url),'utf8')).replace(/\r\n/g,'\n');
  return Object.fromEntries([...text.matchAll(/<!-- ([a-z-]+):start[^>]*-->\n[\s\S]*?<!-- \1:end -->/g)].map(m=>[m[1],m[0]]));
};

test('README and npm README carry the same mirrored blocks', async () => {
  const repo=await blocks('README.md'),npm=await blocks('npm/README.md');
  for(const name of ['latest-update','research'])assert.ok(repo[name],`README.md has no ${name} block`);
  assert.deepEqual(Object.keys(npm).sort(),Object.keys(repo).sort());
  for(const [name,text] of Object.entries(repo)){
    assert.equal(npm[name],text,`${name} differs between README.md and npm/README.md`);
    // npmjs.com does not resolve repository-relative links or images.
    assert.doesNotMatch(text,/\]\((?!https:\/\/)/,`${name}: links and images must be absolute`);
  }
});
