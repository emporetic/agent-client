import fs from 'node:fs';
import os from 'node:os';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { parseAdapterPaths } from './adapter-args.mjs';
import { event, scrub, runAgent } from './adapter-runtime.mjs';
import { gitFor, githubToken } from './github-publisher.mjs';

const [briefPath, output] = parseAdapterPaths(process.argv.slice(2), ['task', 'output']);
const brief = JSON.parse(fs.readFileSync(briefPath, 'utf8'));
const delivery = brief.delivery;
if (!delivery || !delivery.pr_url || !delivery.head_sha) throw new Error('Incomplete delivery in judging.json');

function run(cmd, args, opts = {}) {
  if (cmd === 'git') {
    try { return { code: 0, out: gitFor(process.cwd(), ghToken)(args, true) }; }
    catch (error) { return { code: 1, out: error.message }; }
  }
  event('judge', '', `Running ${cmd} ${args.join(' ')}`, 'tool');
  const result = spawnSync(cmd, args, { encoding: 'utf8', maxBuffer: 24 * 1024 * 1024, ...opts });
  const out = scrub(`${result.stdout || ''}${result.stderr || ''}`);
  for (const line of out.split('\n')) if (line.trim()) event('judge', '', line, 'tool');
  return { code: result.status ?? 1, out };
}

const repoMatch = /^https:\/\/(?:www\.)?github\.com\/([^/]+)\/([^/.]+)(?:\.git)?\/?$/.exec(
  String(brief.repo_url).trim(),
);
if (!repoMatch) throw new Error(`Unsupported repository URL: ${brief.repo_url}`);
const [, owner, repo] = repoMatch;
const prNumber = /\/pull\/(\d+)/.exec(delivery.pr_url)?.[1];
if (!prNumber) throw new Error(`Cannot read a pull request number from ${delivery.pr_url}`);

const ghToken = githubToken();
const cloneUrl = `https://github.com/${owner}/${repo}.git`;

process.on('uncaughtException', (error) => {
  try {
    fs.writeFileSync(join(output, 'judge-error.txt'), scrub(error && error.stack ? error.stack : String(error)), { encoding: 'utf8' });
  } catch {
  }
  process.exitCode = 1;
});

fs.mkdirSync(output, { recursive: true, mode: 0o700 });
const sandbox = mkdtempSync(join(output, 'judge-checkout-'));
const checkout = join(sandbox, 'work');
try {
  let step = run('git', ['clone', '--quiet', cloneUrl, checkout]);
  if (step.code !== 0) throw new Error(`Clone failed: ${scrub(step.out.slice(0, 300))}`);
  step = run('git', ['-C', checkout, 'fetch', '--quiet', 'origin', `refs/pull/${prNumber}/head`]);
  if (step.code !== 0) throw new Error(`Fetching the pull request ref failed: ${scrub(step.out.slice(0, 300))}`);
  const fetched = run('git', ['-C', checkout, 'rev-parse', 'FETCH_HEAD']).out.trim();
  if (fetched.toLowerCase() !== delivery.head_sha.toLowerCase())
    throw new Error(`Fetched head ${fetched} does not match the delivered head ${delivery.head_sha}`);
  step = run('git', ['-C', checkout, 'checkout', '-q', 'FETCH_HEAD']);
  if (step.code !== 0) throw new Error(`Checkout failed: ${step.out}`);

  let tests = 'This task carries no Go test suite; judge on the requirement text and the diff.';
  if (fs.existsSync(join(checkout, 'go.mod'))) {
    const t = run('go', ['test', '-json', '-count=1', '-p=1', '-timeout', '2m', './...'], { cwd: checkout });
    const lines = t.out.split('\n').filter((l) => l.includes('"Action"')).slice(-40).join('\n');
    const verdict = /"Action":"pass"/.test(t.out) && !/"Action":"fail"/.test(t.out);
    tests = `go test exit=${t.code}, overall ${verdict ? 'PASS' : 'FAIL'}\n${lines.slice(0, 6000)}`;
  }
  const diff = run('git', [
    '-C', checkout, 'diff', '--binary', `${String(brief.base_commit).trim()}..${delivery.head_sha}`,
  ]).out;

  const prompt = [
    'You are the publisher-side acceptance judge for a Emporetic bounty. The candidate delivery is already checked out in your working directory.',
    '',
    'Requirement (the whole requirement text):',
    brief.requirement,
    '',
    'Developer summary:',
    String(delivery.summary || '(none)'),
    '',
    'Objective test evidence:',
    tests,
    '',
    'The full diff of the delivery is below. Judge only against the requirement.',
    diff.slice(0, 60000),
    '',
    'Answer with exactly one JSON object and nothing else, using this shape:',
    '{"status":"completed","conclusion":{"items":[{"requirement":"<one criterion from the requirement>","outcome":"met","evidence":["<file, symbol, or test line that proves it>"]}]},"reason":"<one sentence>"}',
    'Platform vocabulary is fixed: every item outcome must be "met" or "not_met", never pass/fail/ok. Use "status":"incomplete" as soon as any item is not_met.',
  ].join('\n');

  const piArgs = ['--print', '--mode', 'json', '--no-session'];
  if (process.env.EMPORETIC_PI_MODEL) piArgs.push('--model', process.env.EMPORETIC_PI_MODEL);
  const answer = { code: 0, out: await runAgent('pi', [...piArgs, '--', prompt], checkout, output, 'judge', true) };
  if (answer.code !== 0) throw new Error(`Pi judgement run exited ${answer.code}: ${scrub(answer.out.slice(0, 300))}`);
  fs.writeFileSync(join(output, 'judge.log'), scrub(answer.out), { encoding: 'utf8' });

  const parsed = parseJson(answer.out);
  if (!parsed || (parsed.status !== 'completed' && parsed.status !== 'incomplete'))
    throw new Error(`Judge answer is not a usable verdict: ${scrub(answer.out.slice(0, 240))}`);
  if (!Array.isArray(parsed.conclusion?.items) || parsed.conclusion.items.length === 0)
    throw new Error('Structured conclusion needs at least one itemised finding');
  const items = parsed.conclusion.items.map((i) => {
    const outcome = String(i.outcome || '').trim().toLowerCase();
    if (outcome === 'met' || outcome === 'pass') return { requirement: String(i.requirement || '').slice(0, 500), outcome: 'met', evidence: evidenceList(i.evidence) };
    if (outcome === 'not_met' || outcome === 'not met' || outcome === 'fail') return { requirement: String(i.requirement || '').slice(0, 500), outcome: 'not_met', evidence: evidenceList(i.evidence) };
    throw new Error(`Item outcome must be met/not_met, got ${JSON.stringify(i.outcome)}`);
  });
  const allMet = items.every((i) => i.outcome === 'met');
  const status = parsed.status === 'completed' && allMet ? 'completed' : 'incomplete';
  fs.writeFileSync(
    join(output, 'verdict.json'),
    JSON.stringify({ status, conclusion: { items }, reason: String(parsed.reason || '').slice(0, 1000) }, null, 2),
    { encoding: 'utf8' },
  );
} finally {
  rmSync(sandbox, { recursive: true, force: true });
}

function evidenceList(raw) {
  const list = Array.isArray(raw) ? raw : [raw ?? ''];
  return list.slice(0, 20).map((e) => String(e).slice(0, 1200));
}

function extractJson(text) {
  const source = String(text);
  for (let start = 0; start < source.length; start++) {
    if (source[start] !== '{') continue;
    let depth = 0;
    let inString = false;
    let escaped = false;
    for (let i = start; i < source.length; i++) {
      const ch = source[i];
      if (inString) {
        if (escaped) escaped = false;
        else if (ch === '\\') escaped = true;
        else if (ch === '"') inString = false;
        continue;
      }
      if (ch === '"') inString = true;
      else if (ch === '{') depth++;
      else if (ch === '}') {
        depth--;
        if (depth === 0) {
          try {
            return JSON.parse(source.slice(start, i + 1));
          } catch {
            return null;
          }
        }
      }
    }
  }
  return null;
}

function parseJson(text) {
  return (
    extractJson(text) ||
    (() => {
      const fenced = /```json\s*([\s\S]*?)```/.exec(String(text));
      if (!fenced) return null;
      try {
        return JSON.parse(fenced[1]);
      } catch {
        return null;
      }
    })()
  );
}
