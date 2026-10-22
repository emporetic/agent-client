import { parseAdapterPaths } from './adapter-args.mjs';
import { event, readJSON, runAgent } from './adapter-runtime.mjs';
import { checkpoint, gitFor, GitHub, loadCheckpoint, publishDelivery, preflightPush, repository, withFailureLog } from './github-publisher.mjs';

const [taskPath, workspace, output] = parseAdapterPaths(process.argv.slice(2), ['task', 'workspace', 'output']);
const task = readJSON(taskPath);
const command = JSON.parse(process.env.LOCAL_AGENT_COMMAND || '[]');
if (!Array.isArray(command) || !command.length || !command.every(value => typeof value === 'string')) throw new Error('Set LOCAL_AGENT_COMMAND to a JSON executable/arguments array');
await withFailureLog(output, async () => {
  const github = new GitHub();
  let state = loadCheckpoint(output, task);
  if (!state) {
    const target = await github.prepare(repository(task.repo_url));
    preflightPush(workspace, task, target, github.token);
    const prompt = `Complete the task in this checkout. Preserve unrelated work.\nTitle: ${task.title}\nRequirement: ${task.requirement}\nBase commit: ${task.base_commit}\nRun relevant checks. Do not push, alter remotes or create a PR; the harness publishes to the user-owned fork. Finish with a short summary.`;
    const argv = command.map(value => value.replaceAll('{prompt}', prompt).replaceAll('{workspace}', workspace));
    event('agent', 'running', 'Running the configured local Agent');
    const summary = await runAgent(argv[0], argv.slice(1), workspace, output);
    const git = gitFor(workspace, github.token);
    git(['add', '-A']);
    if (git(['diff', '--cached', '--name-only'])) git(['-c', 'user.name=Emporetic Agent', '-c', 'user.email=agent@localhost', 'commit', '-q', '-m', task.title]);
    const head = git(['rev-parse', 'HEAD']);
    if (!/^[a-f0-9]{40}$/.test(head) || head === task.base_commit) throw new Error('Local Agent produced no usable delivery commit');
    git(['merge-base', '--is-ancestor', task.base_commit, head]);
    state = checkpoint(output, task, head, summary, target);
    event('agent', 'completed', `Code and verification finished; delivery commit ${head} saved`);
  }
  await publishDelivery(task, workspace, output, state, github);
});
