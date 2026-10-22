import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const examples = dirname(fileURLToPath(import.meta.url));

function fixture(t) {
  const root = fs.realpathSync(fs.mkdtempSync(join(tmpdir(), 'emporetic adapters ')));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const bin = join(root, 'bin');
  fs.mkdirSync(bin);
  fs.mkdirSync(join(root, 'output dir'));
  fs.mkdirSync(join(root, 'workspace dir'));
  // Absolute Node shebang plus a PATH containing only this directory prevents
  // fallback to a real Pi or external commands. HOME contains no credentials.
  fs.writeFileSync(join(bin, 'pi'), `#!${process.execPath}
const fs = require('node:fs');
fs.writeFileSync(process.env.PI_CALL_LOG, JSON.stringify({ args: process.argv.slice(2), cwd: process.cwd() }));
process.stdout.write(process.env.PI_STDOUT || '');
process.stderr.write(process.env.PI_STDERR || '');
if (process.env.PI_SIGNAL) process.kill(process.pid, process.env.PI_SIGNAL);
else process.exit(Number(process.env.PI_EXIT || 0));
`, { mode: 0o755 });
  const env = { HOME: root, PATH: bin, PI_CALL_LOG: join(root, 'pi-call.json') };
  return {
    root,
    env,
    write(name, value) { fs.writeFileSync(join(root, name), JSON.stringify(value)); },
    run(script, args, extraEnv = {}) {
      return spawnSync(process.execPath, [join(examples, script), ...args], {
        cwd: root, env: { ...env, ...extraEnv }, encoding: 'utf8', timeout: 10000,
      });
    },
    call() { return JSON.parse(fs.readFileSync(env.PI_CALL_LOG, 'utf8')); },
    picks() { return JSON.parse(fs.readFileSync(join(root, 'output dir', 'picks.json'), 'utf8')); },
  };
}

function argumentsFor(form, names, values) {
  if (form === 'positional') return values;
  return names.map((name, i) => [`--${name}`, values[i]]).reverse().flat();
}

const brief = {
  intent: 'fixture intent',
  candidates: [
    { id: 'tsk_stdout', title: 'stdout candidate', repo_url: 'https://example.invalid/one', bounty_credits: 10, agents_working: 0 },
    { id: 'tsk_stderr', title: 'stderr candidate', repo_url: 'https://example.invalid/two', bounty_credits: 20, agents_working: 0 },
  ],
};

for (const form of ['positional', 'named']) {
  test(`picker ${form} arguments reach Pi and parse only successful stdout`, (t) => {
    const f = fixture(t);
    f.write('screening input.json', brief);
    const result = f.run('pi-picker.mjs', argumentsFor(form, ['screening', 'output'], ['screening input.json', 'output dir']), {
      PI_STDOUT: 'tsk_stdout\ntsk_stdout\ntsk_unknown\n',
      PI_STDERR: 'diagnostic referring to tsk_stderr\n',
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(f.picks(), { ids: ['tsk_stdout'], skipped: [] });
    assert.match(result.stdout, /Title screening kept 1 of 2/);
    const call = f.call();
    assert.deepEqual(call.args.slice(0, 3), ['--print', '--no-session', '--']);
    assert.match(call.args[3], /fixture intent/);
    assert.match(call.args[3], /stdout candidate/);
  });

  test(`executor ${form} arguments read the intended task before credential access`, (t) => {
    const f = fixture(t);
    f.write('task input.json', { repo_url: 'fixture://unsupported-repository' });
    const result = f.run('pi-executor.mjs', argumentsFor(form, ['task', 'workspace', 'output'], ['task input.json', 'workspace dir', 'output dir']));
    assert.ifError(result.error);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /Unsupported repository URL in task input: fixture:\/\/unsupported-repository/);
    assert.doesNotMatch(result.stderr, /ENOENT|git-credentials/);
    assert.equal(fs.existsSync(f.env.PI_CALL_LOG), false);
  });

  test(`judge ${form} arguments read the intended input before credential access`, (t) => {
    const f = fixture(t);
    f.write('judging input.json', { delivery: {} });
    const result = f.run('pi-judge.mjs', argumentsFor(form, ['task', 'output'], ['judging input.json', 'output dir']));
    assert.ifError(result.error);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /Incomplete delivery in judging.json/);
    assert.doesNotMatch(result.stderr, /ENOENT|git-credentials/);
  });

  test(`generic adapter ${form} checks GitHub credentials before spending a model run`, (t) => {
    const f = fixture(t);
    f.write('task input.json', { title: 'fixture task', requirement: 'fixture requirement' });
    const result = f.run('adapter.mjs', argumentsFor(form, ['task', 'workspace', 'output'], ['task input.json', 'workspace dir', 'output dir']), {
      LOCAL_AGENT_COMMAND: JSON.stringify(['pi', '{prompt}', '{workspace}']),
      PI_EXIT: '23', // Stop before any Git or GitHub operations.
    });
    assert.ifError(result.error);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /Configure GitHub authorization/);
    assert.equal(fs.existsSync(f.env.PI_CALL_LOG), false);
  });
}

for (const [label, env, error] of [
  ['nonzero exit', { PI_EXIT: '7', PI_STDOUT: 'tsk_stdout' }, /exit 7/],
  ['signal termination', { PI_SIGNAL: 'SIGTERM', PI_STDOUT: 'tsk_stdout' }, /signal SIGTERM/],
  ['spawn failure', { PATH: '' }, /Pi title screening failed:.*ENOENT/],
]) {
  test(`picker rejects ${label} without writing picks`, (t) => {
    const f = fixture(t);
    f.write('screening input.json', brief);
    const result = f.run('pi-picker.mjs', ['--screening', 'screening input.json', '--output', 'output dir'], env);
    assert.ifError(result.error);
    assert.equal(result.status, 1);
    assert.match(result.stderr, error);
    assert.equal(fs.existsSync(join(f.root, 'output dir', 'picks.json')), false);
  });
}

for (const script of ['adapter.mjs', 'pi-executor.mjs', 'pi-picker.mjs', 'pi-judge.mjs']) {
  test(`${script} rejects invalid flags before opening any input`, (t) => {
    const f = fixture(t);
    const result = f.run(script, ['--unknown', 'input.json']);
    assert.ifError(result.error);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /Unknown argument --unknown/);
    assert.doesNotMatch(result.stderr, /ENOENT/);
    assert.equal(fs.existsSync(f.env.PI_CALL_LOG), false);
  });
}
