import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { parseAdapterPaths } from './adapter-args.mjs';

const [taskPath, workspace, output] = parseAdapterPaths(process.argv.slice(2), ['task', 'workspace', 'output']);
const task = JSON.parse(readFileSync(taskPath, 'utf8'));
const command = JSON.parse(process.env.LOCAL_AGENT_COMMAND || '[]');
if (!Array.isArray(command) || !command.length || !command.every((v) => typeof v === 'string')) {
  throw new Error('Set LOCAL_AGENT_COMMAND to a JSON executable/arguments array');
}
const prompt = `Complete the following task in the current repository. Preserve unrelated work.
Title: ${task.title}
Requirement (the whole requirement text): ${task.requirement}
Base commit: ${task.base_commit}
Deadline: ${task.deadline_at}
Make the code changes and run relevant local checks. Independent platform verification is separate.`;
const argv = command.map((v) =>
  v.replaceAll('{prompt}', prompt).replaceAll('{workspace}', workspace),
);
const result = spawnSync(argv[0], argv.slice(1), {
  cwd: workspace,
  stdio: 'inherit',
  shell: false,
});
if (result.error || result.status !== 0) throw new Error('Local Agent failed');

const branch = `emporetic/${task.id}`;
function git(...args) {
  const r = spawnSync('git', ['-c', 'advice.detachedHead=false', ...args], { cwd: workspace, maxBuffer: 12 * 1024 * 1024 });
  if (r.error || r.status !== 0) throw new Error(`Git ${args[0]} failed: ${r.stdout || r.stderr || ''}`);
  return r.stdout;
}
function gh(...args) {
  const r = spawnSync('gh', args, { cwd: workspace, maxBuffer: 12 * 1024 * 1024 });
  if (r.error || r.status !== 0) throw new Error(`GitHub CLI ${args.join(' ')} failed: ${r.stdout || r.stderr || ''}`);
  return r.stdout;
}
const index = join(output, 'adapter.index');
const env = { ...process.env, GIT_INDEX_FILE: index };
function gitTemp(...args) {
  const r = spawnSync('git', args, { cwd: workspace, env, maxBuffer: 12 * 1024 * 1024 });
  if (r.error || r.status !== 0) throw new Error(`Git ${args[0]} failed`);
  return r.stdout;
}
try {
  git('checkout', '-q', '-B', branch);
  git('add', '-A');
  git('-c', 'user.name=Emporetic Agent', '-c', 'user.email=agent@localhost', '-c', 'commit.gpgsign=false', 'commit', '-q', '-m', task.title);
  const headSha = String(git('rev-parse', 'HEAD')).trim();
  writeFileSync(join(output, 'pr.json'), JSON.stringify({ pr_url: '', head_sha: headSha }));
  git('push', 'origin', `HEAD:${branch}`);
  const created = JSON.parse(String(gh('pr', 'create', '--head', branch, '--title', task.title, '--body', `Task ${task.id}\n\n${task.requirement}`, '--json', 'url,headRefOid')));
  if (!created.url || !created.headRefOid) throw new Error('Pull Request creation returned no reference');
  writeFileSync(join(output, 'pr.json'), JSON.stringify({ pr_url: created.url, head_sha: created.headRefOid }));
  const stat = String(git('diff', '--no-ext-diff', '--stat', `${task.base_commit}..${headSha}`));
  writeFileSync(
    join(output, 'summary.txt'),
    `Local Agent completed the requested changes.\n${stat}\nLocal checks are not an independent acceptance result.\n`,
  );
  void gitTemp;
} finally {
  rmSync(index, { force: true });
}
