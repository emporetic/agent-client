import { parseAdapterPaths } from './adapter-args.mjs';
import { event, readJSON, runAgent } from './adapter-runtime.mjs';
import { checkpoint, gitFor, GitHub, loadCheckpoint, publishDelivery, preflightPush, repository, withFailureLog } from './github-publisher.mjs';

const [taskPath, workspace, output] = parseAdapterPaths(process.argv.slice(2), ['task', 'workspace', 'output']);
const task = readJSON(taskPath);
const upstream = repository(task.repo_url);
await withFailureLog(output, async () => {
  const github = new GitHub();
  let state = loadCheckpoint(output, task);
  if (!state) {
    const target = await github.prepare(upstream);
    preflightPush(workspace, task, target, github.token);
    event('agent', 'running', 'Running Pi in the prepared checkout');
    const prompt = [
      `You are working inside a checkout of ${upstream} at ${task.base_commit}.`,
      `Task: ${task.title}`, task.requirement,
      'Edit files only in this checkout. Run relevant checks. Preserve unrelated content.',
      'Never push, create branches, alter remotes or create a PR: the harness publishes through the user-owned fork.',
      'Committing is fine. Do not access credentials or print secrets. Finish with a short change and verification summary.',
    ].join('\n\n');
    const args = ['--print', '--mode', 'json', '--no-session'];
    if (process.env.EMPORETIC_PI_MODEL) args.push('--model', process.env.EMPORETIC_PI_MODEL);
    const summary = await runAgent('pi', [...args, '--', prompt], workspace, output, 'agent', true);
    const git = gitFor(workspace, github.token);
    git(['add', '-A']);
    if (git(['diff', '--cached', '--name-only'])) git(['-c', 'user.name=Emporetic Local Agent', '-c', 'user.email=emporetic.local.agent@example.invalid', 'commit', '-q', '-m', `Emporetic delivery ${task.id}`]);
    const head = git(['rev-parse', 'HEAD']);
    if (!/^[a-f0-9]{40}$/.test(head) || head === task.base_commit) throw new Error('Pi produced no usable delivery commit');
    git(['merge-base', '--is-ancestor', task.base_commit, head]);
    state = checkpoint(output, task, head, summary, target);
    event('agent', 'completed', `Code and verification finished; delivery commit ${head} saved`);
  }
  await publishDelivery(task, workspace, output, state, github);
});
