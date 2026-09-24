import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import load from '@commitlint/load';
import lint from '@commitlint/lint';
import { format } from '@commitlint/format';

export function parseCommits(input) {
  const fields = input.split('\0');
  if (fields.pop() !== '' || fields.length === 0 || fields.length % 2 !== 0) {
    throw new Error('Expected a nonempty, NUL-delimited Git commit range');
  }
  const commits = [];
  for (let index = 0; index < fields.length; index += 2) {
    if (!/^[a-f0-9]{40}$/.test(fields[index]) || !fields[index + 1].trim()) {
      throw new Error('Malformed Git commit identity or empty message');
    }
    commits.push({ sha: fields[index], message: fields[index + 1] });
  }
  return commits;
}

export async function lintCommits(commits) {
  const config = await load({}, { file: '/config/commitlint.config.mjs' });
  const options = {
    parserOpts: config.parserPreset?.parserOpts ?? {},
    plugins: config.plugins ?? {},
    ignores: config.ignores ?? [],
    defaultIgnores: config.defaultIgnores ?? true,
  };
  return Promise.all(commits.map(async ({ sha, message }) => ({
    sha,
    result: await lint(message, config.rules, options),
  })));
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const results = await lintCommits(parseCommits(readFileSync(0, 'utf8')));
    const report = format({ results: results.map(({ result }) => result) }, { color: false });
    if (report) console.log(report);
    const failures = results.filter(({ result }) => !result.valid);
    if (failures.length) {
      console.error(`Commit validation failed: ${failures.map(({ sha }) => sha).join(', ')}`);
      process.exitCode = 1;
    } else {
      console.log(`Validated all ${results.length} commits in the release range.`);
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
