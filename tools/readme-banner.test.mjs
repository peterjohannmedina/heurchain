// The "Latest update" banner on the GitHub README is mirrored to the npm page
// (npm/README.md). This fails when the two copies drift apart.
import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';

const block=async path=>{
  const text=(await readFile(new URL('../'+path,import.meta.url),'utf8')).replace(/\r\n/g,'\n');
  const m=text.match(/<!-- latest-update:start[^>]*-->\n[\s\S]*?<!-- latest-update:end -->/);
  assert.ok(m,`${path} has no latest-update banner`);
  return m[0];
};

test('README and npm README carry the same latest-update banner', async () => {
  const repo=await block('README.md'),npm=await block('npm/README.md');
  assert.equal(npm,repo);
  // npmjs.com does not resolve repository-relative links.
  assert.doesNotMatch(repo,/\]\((?!https:\/\/)/,'banner links must be absolute');
});
