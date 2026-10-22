import fs from 'node:fs';
import os from 'node:os';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { parseAdapterPaths } from './adapter-args.mjs';

const [taskPath, workspace, output] = parseAdapterPaths(process.argv.slice(2), ['task', 'workspace', 'output']);
const task = JSON.parse(fs.readFileSync(taskPath, 'utf8'));

const scrub = (value) =>
  String(value)
    .replace(/gh[pousr_][A-Za-z0-9]{16,}/g, '***')
    .replace(/https:\/\/[^@\s]+:[^@\s]+@/g, 'https://***@')
    .replace(/\bBearer \S+/gi, 'Bearer ***');

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', maxBuffer: 24 * 1024 * 1024, ...opts });
  if (r.error) throw new Error(`${cmd} failed: ${scrub(r.error.message)}`);
  if (r.status !== 0)
    throw new Error(`${cmd} exited ${r.status}: ${scrub((r.stderr || r.stdout || '').slice(0, 400))}`);
  return r.stdout;
}

const repoMatch = /^https:\/\/(?:www\.)?github\.com\/([^/]+)\/([^/.]+)(?:\.git)?\/?$/.exec(
  task.repo_url.trim(),
);
if (!repoMatch) throw new Error(`Unsupported repository URL in task input: ${task.repo_url}`);
const [, owner, repo] = repoMatch;

const credentialLine = fs
  .readFileSync(join(os.homedir(), '.git-credentials'), 'utf8')
  .split('\n')
  .find((line) => /^https:\/\/[^:]+:[^@\s]+@.*github/i.test(line));
if (!credentialLine) throw new Error('No GitHub HTTPS credential found in ~/.git-credentials');
const cred = /^https:\/\/([^:]+):([^@]+)@/.exec(credentialLine);
const ghUser = decodeURIComponent(cred[1]);
const ghToken = cred[2];
const apiHeaders = {
  authorization: `Bearer ${ghToken}`,
  accept: 'application/vnd.github+json',
  'user-agent': 'emporetic-local-agent',
};
const pushUrl = `https://${encodeURIComponent(ghUser)}:${encodeURIComponent(ghToken)}@github.com/${owner}/${repo}.git`;

fs.mkdirSync(output, { recursive: true });

const prompt = [
  `You are working inside a git checkout of ${repo} at commit ${task.base_commit}. The client already checked it out for you.`,
  '',
  'Task title:',
  task.title,
  '',
  'Requirement (the whole requirement text, follow every acceptance criterion in it):',
  task.requirement,
  '',
  'Rules:',
  '- Edit files in this checkout only. Never run git push, git remote add, or create branches: the harness publishes your changes.',
  '- Committing your work is fine (the harness pushes whatever ends up on HEAD); just leave nothing untracked.',
  '- Do not read or write ~/.git-credentials and do not print tokens into any file you touch.',
  '- Keep unrelated content untouched. Commit nothing that is generated noise (caches, editor swap files).',
  '- Finish with a short summary of what you changed and how you verified it.',
].join('\n');

const piArgs = ['--print', '--no-session'];
if (process.env.EMPORETIC_PI_MODEL) piArgs.push('--model', process.env.EMPORETIC_PI_MODEL);
const assistantText = run('pi', [...piArgs, '--', prompt], { cwd: workspace });
fs.writeFileSync(join(output, 'agent.log'), scrub(assistantText), { encoding: 'utf8' });

const git = (args, opts = {}) => run('git', ['-c', 'advice.detachedHead=false', ...args], { cwd: workspace, ...opts });
git(['add', '-A']);
// Pi may finish with a clean tree because it committed its own work: staged changes or a
// delivery commit on top of the base commit are both acceptable, nothing at all is not.
const hasDeliveryCommit = git(['rev-parse', 'HEAD']).trim() !== task.base_commit.trim();
if (git(['diff', '--cached', '--name-only']).trim()) {
  git([
    '-c', 'user.name=Emporetic Local Agent',
    '-c', 'user.email=emporetic.local.agent@example.invalid',
    'commit', '-q', '-m', `Emporetic delivery ${task.id}`,
  ]);
} else if (!hasDeliveryCommit) {
  throw new Error('Pi produced neither staged changes nor a delivery commit; refusing to publish an empty delivery');
}
const headSha = git(['rev-parse', 'HEAD']).trim();
if (!/^[0-9a-f]{40}$/.test(headSha) || headSha === task.base_commit.trim())
  throw new Error(`Delivery commit is unusable: ${headSha}`);

const branch = `emporetic/${task.id}`;
git(['push', '-q', pushUrl, `HEAD:refs/heads/${branch}`]);

const repoInfo = await (await fetch(`https://api.github.com/repos/${owner}/${repo}`, { headers: apiHeaders })).json();
const defaultBranch = repoInfo.default_branch;
if (!defaultBranch) throw new Error(`Cannot resolve default branch for ${owner}/${repo}`);
const created = await fetch(`https://api.github.com/repos/${owner}/${repo}/pulls`, {
  method: 'POST',
  headers: apiHeaders,
  body: JSON.stringify({ title: `Emporetic delivery ${task.id}`, head: branch, base: defaultBranch }),
});
if (!created.ok) throw new Error(`Creating the pull request failed (${created.status}): ${scrub((await created.text()).slice(0, 300))}`);
const pr = await created.json();

fs.writeFileSync(
  join(output, 'pr.json'),
  JSON.stringify({ pr_url: pr.html_url, head_sha: headSha }, null, 2),
  { encoding: 'utf8' },
);
fs.writeFileSync(join(output, 'summary.txt'), scrub(assistantText).slice(0, 4000).trim(), { encoding: 'utf8' });
console.log(`Published ${pr.html_url} on ${branch} @ ${headSha}`);
