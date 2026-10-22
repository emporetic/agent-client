import fs from 'node:fs';
import os from 'node:os';
import { createHash } from 'node:crypto';
import { join } from 'node:path';
import { command, event, hideSecret, readJSON, saveJSON, scrub } from './adapter-runtime.mjs';

export function repository(url) {
  const match = /^https:\/\/github\.com\/([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+?)\/?$/.exec(String(url).trim());
  if (!match) throw new Error(`Unsupported repository URL in task input: ${url}`);
  return `${match[1]}/${match[2].replace(/\.git$/, '')}`;
}
export function githubToken() {
  let token = process.env.EMPORETIC_GITHUB_TOKEN || process.env.GH_TOKEN || process.env.GITHUB_TOKEN;
  const file = process.env.EMPORETIC_GITHUB_TOKEN_FILE || join(process.env.EMPORETIC_HOME || join(os.homedir(), '.emporetic'), 'github-token');
  if (!token && fs.existsSync(file)) token = fs.readFileSync(file, 'utf8').trim();
  if (!token) { try { token = command('gh', ['auth', 'token', '--hostname', 'github.com']).trim(); } catch { /* optional CLI login */ } }
  if (!token) {
    try {
      for (const line of fs.readFileSync(join(os.homedir(), '.git-credentials'), 'utf8').split('\n')) {
        try { const url = new URL(line); if (url.protocol === 'https:' && url.hostname === 'github.com' && url.password) { token = decodeURIComponent(url.password); break; } } catch { /* not a credential URL */ }
      }
    } catch { /* optional legacy store */ }
  }
  if (!token || /\s/.test(token)) throw new Error('Configure GitHub authorization with gh auth login, EMPORETIC_GITHUB_TOKEN, or ~/.emporetic/github-token (0600).');
  return hideSecret(token);
}
export class GitHub {
  constructor(token = githubToken(), fetcher = globalThis.fetch) { this.token = hideSecret(token); this.fetcher = fetcher; }
  async api(path, method = 'GET', body, missing = false) {
    let response;
    try {
      response = await this.fetcher(`https://api.github.com${path}`, {
        method, redirect: 'error', signal: AbortSignal.timeout(20000),
        headers: { authorization: `Bearer ${this.token}`, accept: 'application/vnd.github+json', 'content-type': 'application/json', 'user-agent': 'emporetic-agent' },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
    } catch (cause) { const error = new Error(scrub(`GitHub request failed: ${cause.message}`)); error.retryable = true; throw error; }
    if (missing && response.status === 404) return null;
    if (!response.ok) {
      const error = new Error(scrub(`GitHub ${method} ${path} returned ${response.status}: ${(await response.text()).slice(0, 500)}`));
      error.retryable = response.status === 429 || response.status >= 500 || (response.status === 403 && response.headers?.get('x-ratelimit-remaining') === '0');
      throw error;
    }
    return response.json();
  }
  async prepare(upstream) {
    event('github', 'running', 'Checking GitHub identity and upstream access');
    const user = await this.api('/user');
    if (!/^[A-Za-z0-9-]+$/.test(user.login)) throw new Error('GitHub returned no usable account identity');
    const original = await this.api(`/repos/${upstream}`);
    if (!original.default_branch || !original.id) throw new Error('Cannot resolve upstream repository and default branch');
    if (this.token.startsWith('github_pat_') && original.owner?.login?.toLowerCase() !== user.login.toLowerCase()) throw new Error('Fine-grained tokens cannot contribute to arbitrary third-party public repositories. Use gh auth login or a classic token with public_repo for upstream PRs.');
    event('github', 'completed', `GitHub @${user.login}; upstream ${upstream} is readable`);
    event('fork', 'running', `Preparing a fork owned by ${user.login}`);
    const network = original.source?.id || original.id;
    const valid = fork => fork && fork.owner?.login?.toLowerCase() === user.login.toLowerCase() && (fork.source?.id || fork.id) === network;
    let fork;
    if (original.owner.login.toLowerCase() === user.login.toLowerCase()) fork = original;
    else {
      const named = await this.api(`/repos/${user.login}/${original.name}`, 'GET', undefined, true);
      if (valid(named)) fork = named;
      if (!fork) {
        for (let page = 1; page <= 10 && !fork; page++) {
          const list = await this.api(`/repos/${upstream}/forks?per_page=100&page=${page}`);
          const candidate = list.find(item => item.owner?.login?.toLowerCase() === user.login.toLowerCase());
          if (candidate) { const info = await this.api(`/repos/${candidate.full_name}`); if (valid(info)) fork = info; }
          if (list.length < 100) break;
        }
      }
      if (!fork) {
        const name = named ? `${original.name}-emporetic-${original.owner.login}`.slice(0, 100) : original.name;
        const created = await this.api(`/repos/${upstream}/forks`, 'POST', { name, default_branch_only: false });
        for (let attempt = 0; attempt < 30; attempt++) {
          const info = await this.api(`/repos/${created.full_name}`, 'GET', undefined, true);
          if (valid(info) && info.permissions?.push) { fork = info; break; }
          await new Promise(resolve => setTimeout(resolve, 1000));
        }
        if (!fork) { const error = new Error('GitHub fork is not ready yet; retry preparation'); error.retryable = true; throw error; }
      }
    }
    if (!valid(fork) || !fork.permissions?.push) throw new Error('GitHub authorization cannot push to the account-owned fork; check token permissions and repository selection');
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(fork.full_name)) throw new Error('GitHub returned an invalid fork name');
    event('fork', 'completed', `Fork ready: ${fork.full_name}`);
    return { user: user.login, fork: fork.full_name, base: original.default_branch, upstream };
  }
}
export function gitFor(workspace, token, exec = command) {
  return (args, authenticated = false) => {
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('GIT_')));
    Object.assign(env, { GIT_TERMINAL_PROMPT: '0', GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: os.devNull });
    if (authenticated) Object.assign(env, { GIT_CONFIG_COUNT: '1', GIT_CONFIG_KEY_0: 'http.https://github.com/.extraheader', GIT_CONFIG_VALUE_0: `AUTHORIZATION: basic ${Buffer.from(`x-access-token:${token}`).toString('base64')}` });
    return exec('git', ['-c', `core.hooksPath=${os.devNull}`, '-c', 'credential.helper=', '-c', 'commit.gpgsign=false', ...args], { cwd: workspace, env }).trim();
  };
}
export function checkpoint(output, task, head, summary, target) {
  const suffix = process.env.EMPORETIC_INSTANCE ? `-${createHash('sha256').update(process.env.EMPORETIC_INSTANCE).digest('hex').slice(0, 8)}` : '';
  const state = { version: 1, task_id: task.id, repo_url: task.repo_url, base_commit: task.base_commit, head_sha: head, summary: Buffer.from(scrub(summary)).subarray(0, 18000).toString('utf8'), target, branch: `emporetic/${task.id}${suffix}` };
  saveJSON(join(output, 'publish-checkpoint.json'), state);
  return state;
}
export function preflightPush(workspace, task, target, token) {
  const git = gitFor(workspace, token);
  git(['push', '--dry-run', '--porcelain', `https://github.com/${target.fork}.git`, `HEAD:refs/heads/emporetic/preflight-${task.id}`], true);
  event('fork', '', `Push authorization verified for ${target.fork}`);
}
export function loadCheckpoint(output, task) {
  const path = join(output, 'publish-checkpoint.json');
  if (!fs.existsSync(path)) {
    if (process.env.EMPORETIC_PUBLISH_ONLY === '1') throw new Error('No publication checkpoint; refusing to run the model during publication recovery');
    return null;
  }
  const state = readJSON(path);
  if (state.version !== 1 || state.task_id !== task.id || state.repo_url !== task.repo_url || state.base_commit !== task.base_commit || !/^[a-f0-9]{40}$/.test(state.head_sha)) throw new Error('Publication checkpoint does not match this task');
  return state;
}
export async function publishDelivery(task, workspace, output, state, github, exec) {
  const git = gitFor(workspace, github.token, exec);
  if (git(['rev-parse', 'HEAD']) !== state.head_sha || git(['status', '--porcelain'])) throw new Error('Workspace changed after the delivery checkpoint; inspect it before publishing');
  git(['merge-base', '--is-ancestor', task.base_commit, state.head_sha]);
  const target = await github.prepare(repository(task.repo_url));
  if (target.fork.toLowerCase() !== state.target.fork.toLowerCase() || target.user.toLowerCase() !== state.target.user.toLowerCase()) throw new Error('Publication checkpoint belongs to another GitHub account or fork');
  const url = `https://github.com/${target.fork}.git`;
  event('push', 'running', `Publishing ${state.head_sha} to ${target.fork}:${state.branch}`);
  const remote = git(['ls-remote', url, `refs/heads/${state.branch}`], true).split(/\s/)[0];
  if (remote && remote !== state.head_sha) throw new Error('Remote task branch already points to a different commit; refusing to overwrite it');
  if (!remote) git(['push', '--quiet', url, `${state.head_sha}:refs/heads/${state.branch}`], true);
  const pushed = git(['ls-remote', url, `refs/heads/${state.branch}`], true).split(/\s/)[0];
  if (pushed !== state.head_sha) throw new Error('Published branch does not match the fixed delivery commit');
  event('push', 'completed', `Published branch in ${target.fork}`);
  event('pr', 'running', `Opening a pull request against ${target.upstream}`);
  const head = `${target.user}:${state.branch}`;
  const pulls = await github.api(`/repos/${target.upstream}/pulls?state=all&head=${encodeURIComponent(head)}&base=${encodeURIComponent(state.target.base)}`);
  let pr = pulls.find(item => item.head?.repo?.full_name?.toLowerCase() === target.fork.toLowerCase() && item.head?.ref === state.branch);
  if (!pr) pr = await github.api(`/repos/${target.upstream}/pulls`, 'POST', { title: task.title, body: `Emporetic task ${task.id}\n\n${state.summary}`, head, base: state.target.base });
  if (pr.state === 'closed' && !pr.merged_at) throw new Error('The existing task pull request was closed without merging; inspect it before recovery');
  if (pr.head?.sha !== state.head_sha || pr.base?.repo?.full_name?.toLowerCase() !== target.upstream.toLowerCase() || pr.head?.repo?.full_name?.toLowerCase() !== target.fork.toLowerCase() || !/^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*$/.test(pr.html_url)) throw new Error('Pull request does not match the expected upstream, fork and commit');
  saveJSON(join(output, 'pr.json'), { pr_url: pr.html_url, head_sha: state.head_sha });
  fs.writeFileSync(join(output, 'summary.txt'), state.summary, { mode: 0o600 });
  fs.rmSync(join(output, 'publish-error.json'), { force: true });
  event('pr', 'completed', `Pull request ready: ${pr.html_url}`);
  return pr;
}
export async function withFailureLog(output, work) {
  fs.mkdirSync(output, { recursive: true, mode: 0o700 });
  try { return await work(); }
  catch (error) {
    fs.writeFileSync(join(output, 'adapter-error.log'), scrub(error.stack || error.message || String(error)), { mode: 0o600 });
    saveJSON(join(output, 'publish-error.json'), { retryable: error.retryable === true, message: scrub(error.message).slice(0, 2000) });
    event('agent', '', scrub(error.message), 'error');
    process.exitCode = 1;
    console.error(scrub(error.message));
  }
}
