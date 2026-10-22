import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import os from 'node:os';
import { join } from 'node:path';
import { GitHub, checkpoint, loadCheckpoint, publishDelivery, gitFor, repository } from './github-publisher.mjs';
import { hideSecret, runAgent, scrub } from './adapter-runtime.mjs';

const sha = 'a'.repeat(40), base = 'b'.repeat(40);
const task = { id: 'tsk_test', repo_url: 'https://github.com/upstream/project', base_commit: base, title: 'Fix the issue' };
const upstream = { id: 1, name: 'project', full_name: 'upstream/project', owner: { login: 'upstream' }, default_branch: 'main' };
const fork = { id: 2, full_name: 'worker/project', name: 'project', owner: { login: 'worker' }, source: { id: 1 }, permissions: { push: true } };
const target = { user: 'worker', fork: 'worker/project', upstream: 'upstream/project', base: 'main' };
function fixture(t) { const dir = fs.mkdtempSync(join(os.tmpdir(), 'emp-publisher-')); t.after(() => fs.rmSync(dir, { recursive: true, force: true })); return dir; }
const response = value => ({ ok: true, status: 200, json: async () => value });

test('preparation uses the authenticated account and reuses only a verified fork', async () => {
  const calls = [];
  const github = new GitHub('fixture-token', async url => { calls.push(url); if (url.endsWith('/user')) return response({ login: 'worker' }); if (url.endsWith('/repos/upstream/project')) return response(upstream); if (url.endsWith('/repos/worker/project')) return response(fork); throw new Error(`Unexpected ${url}`); });
  assert.deepEqual(await github.prepare('upstream/project'), target);
  assert.equal(calls.length, 3);
});
test('unrelated same-name repositories are never used as a fork', async () => {
  let created;
  const alternate = { ...fork, full_name: 'worker/project-emporetic-upstream' };
  const github = new GitHub('fixture-token', async (url, init) => {
    if (url.endsWith('/user')) return response({ login: 'worker' });
    if (url.endsWith('/repos/upstream/project')) return response(upstream);
    if (url.endsWith('/repos/worker/project')) return response({ ...fork, id: 99, source: undefined });
    if (url.includes('/forks?')) return response([]);
    if (url.endsWith('/forks')) { created = JSON.parse(init.body); return response(alternate); }
    if (url.endsWith('/repos/worker/project-emporetic-upstream')) return response(alternate);
    throw new Error(`Unexpected ${url}`);
  });
  assert.equal((await github.prepare('upstream/project')).fork, alternate.full_name);
  assert.equal(created.name, 'project-emporetic-upstream');
});
test('publication pushes only to the user fork and opens the PR on upstream', async t => {
  const output = fixture(t), calls = []; let published = false, prBody;
  const state = checkpoint(output, task, sha, 'Checks passed', target);
  const pr = { html_url: 'https://github.com/upstream/project/pull/12', state: 'open', head: { sha, ref: state.branch, repo: { full_name: target.fork } }, base: { repo: { full_name: target.upstream } } };
  const github = { token: 'fixture-token', prepare: async () => target, api: async (path, method, body) => { if (method === 'POST') { prBody = body; return pr; } return []; } };
  const git = (cmd, args, opts) => {
    calls.push({ args, opts });
    if (args.includes('rev-parse')) return sha;
    if (args.includes('ls-remote')) return published ? `${sha}\trefs/heads/${state.branch}` : '';
    if (args.includes('push')) published = true;
    return '';
  };
  await publishDelivery(task, '/fixture', output, state, github, git);
  assert.ok(calls.some(call => call.args.includes('push')));
  for (const call of calls.filter(call => call.args.includes('push') || call.args.includes('ls-remote'))) {
    assert.ok(call.args.includes('https://github.com/worker/project.git'));
    assert.ok(!JSON.stringify(call.args).includes(github.token));
    assert.match(call.opts.env.GIT_CONFIG_VALUE_0, /^AUTHORIZATION: basic /);
  }
  assert.equal(prBody.head, `worker:${state.branch}`);
  assert.equal(prBody.base, 'main');
  assert.equal(JSON.parse(fs.readFileSync(join(output, 'pr.json'))).head_sha, sha);
  // Simulate a lost response after publication: neither push nor PR is repeated.
  calls.length = 0; github.api = async (path, method) => { assert.notEqual(method, 'POST'); return [pr]; };
  await publishDelivery(task, '/fixture', output, loadCheckpoint(output, task), github, git);
  assert.ok(!calls.some(call => call.args.includes('push')));
});
test('publication refuses changed heads and existing unrelated remote commits', async t => {
  const output = fixture(t), state = checkpoint(output, task, sha, 'done', target);
  const github = { token: 'fixture-token', prepare: async () => target };
  await assert.rejects(publishDelivery(task, '/', output, state, github, () => base), /Workspace changed/);
  const git = (_, args) => args.includes('rev-parse') ? sha : args.includes('ls-remote') ? base : '';
  await assert.rejects(publishDelivery(task, '/', output, state, github, git), /refusing to overwrite/);
});
test('GitHub distinguishes transient failures from credential failures and scrubs tokens', async () => {
  hideSecret('fixture-secret');
  assert.equal(scrub('Bearer fixture-secret https://user:fixture-secret@github.com'), '[redacted] https://[redacted]@github.com');
  for (const [status, retryable] of [[401, false], [403, false], [429, true], [503, true]]) {
    const github = new GitHub('fixture-secret', async () => ({ ok: false, status, text: async () => 'fixture-secret' }));
    await assert.rejects(github.api('/user'), error => error.retryable === retryable && !error.message.includes('fixture-secret'));
  }
  assert.equal(repository('https://github.com/user/project.name.git'), 'user/project.name');
  assert.throws(() => repository('https://github.com@evil.invalid/user/repo'));
});
test('Pi JSON logs retain tool evidence and public text, excluding internal thinking', async t => {
  const output = fixture(t);
  const records = [
    { type: 'message_update', assistantMessageEvent: { type: 'thinking_delta', delta: 'PRIVATE_REASONING' } },
    { type: 'tool_execution_start', toolName: 'bash', args: { command: 'go test ./...' } },
    { type: 'tool_execution_end', toolName: 'bash', result: { content: [{ type: 'text', text: 'PASS' }] } },
    { type: 'message_end', message: { role: 'assistant', content: [{ type: 'thinking', thinking: 'PRIVATE_REASONING' }, { type: 'text', text: 'Completed and tested' }] } },
  ];
  const script = `process.stdout.write(${JSON.stringify(records.map(record => JSON.stringify(record)).join('\n') + '\n')})`;
  const answer = await runAgent(process.execPath, ['-e', script], output, output, 'agent', true);
  const log = fs.readFileSync(join(output, 'agent.log'), 'utf8');
  assert.equal(answer, 'Completed and tested'); assert.match(log, /go test|PASS/); assert.doesNotMatch(log, /PRIVATE_REASONING/);
});
