import assert from 'node:assert/strict';
import test from 'node:test';
import { lintCommits, parseCommits } from './lint-release-commits.mjs';

const sha = 'a'.repeat(40);
const encode = (message) => `${sha}\0${message}\0`;

test('parses complete multiline messages, not just titles', () => {
  const message = 'feat(modules)!: publish versions\n\nBREAKING CHANGE: immutable authoring.\n';
  assert.deepEqual(parseCommits(encode(message)), [{ sha, message }]);
});

test('rejects empty dispatch ranges and malformed input', () => {
  for (const input of ['', '\0', sha, `${sha}\0`, encode(''), `invalid\0feat: x\0`]) {
    assert.throws(() => parseCommits(input));
  }
});

test('valid wrapped breaking changes pass the actual repository rules', async () => {
  const results = await lintCommits(parseCommits(encode(
    'feat(modules)!: publish versions\n\nBREAKING CHANGE: immutable authoring.\nUse explicit promotion.',
  )));
  assert.equal(results[0].result.valid, true);
});

test('an earlier invalid footer fails even when the latest commit is valid', async () => {
  const bad = 'feat(modules)!: add versions\n\nBREAKING CHANGE: ' + 'x'.repeat(101);
  const results = await lintCommits(parseCommits(encode(bad) + encode('chore(release): prepare publication')));
  assert.equal(results[0].result.valid, false);
  assert.ok(results[0].result.errors.some(({ name }) => name === 'footer-max-line-length'));
  assert.equal(results[1].result.valid, true);
});

test('validates beyond the first API page of commits', async () => {
  const input = encode('fix(modules): retain history').repeat(101) + encode('invalid commit message');
  const results = await lintCommits(parseCommits(input));
  assert.equal(results.length, 102);
  assert.ok(results.slice(0, 101).every(({ result }) => result.valid));
  assert.equal(results[101].result.valid, false);
});
