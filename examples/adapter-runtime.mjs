import fs from 'node:fs';
import { spawn, spawnSync } from 'node:child_process';
import { join } from 'node:path';

const secrets = new Set();
export function hideSecret(value) { if (value) secrets.add(value); return value; }
for (const key of ['EMPORETIC_GITHUB_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN']) hideSecret(process.env[key]);
for (const [key, value] of Object.entries(process.env)) if (/(?:TOKEN|SECRET|PASSWORD|API_KEY|ACCESS_KEY)$/.test(key) && value.length >= 8) hideSecret(value);
export function scrub(value) {
  let text = String(value);
  for (const secret of secrets) text = text.split(secret).join('[redacted]');
  return text.replace(/github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9_]+/g, '[redacted]')
    .replace(/https?:\/\/[^\s/@]+:[^\s/@]+@/g, 'https://[redacted]@')
    .replace(/\b(?:Bearer|Basic)\s+\S+/gi, '[redacted]');
}
export function event(step, status, message, level = 'info') {
  const parts = scrub(message).split('\n').flatMap(line => line.match(/[\s\S]{1,900}/gu) || ['']);
  for (let i = 0; i < parts.length; i++) process.stdout.write(`EMPORETIC_EVENT ${JSON.stringify({ step, status: i === 0 ? status : '', level, message: parts[i] })}\n`);
}
export function saveJSON(path, value) {
  const staging = `${path}.${process.pid}.tmp`;
  fs.writeFileSync(staging, JSON.stringify(value, null, 2), { mode: 0o600 });
  fs.renameSync(staging, path);
}
export function readJSON(path) { return JSON.parse(fs.readFileSync(path, 'utf8')); }
export function command(cmd, args, opts = {}) {
  const result = spawnSync(cmd, args, { encoding: 'utf8', maxBuffer: 24 * 1024 * 1024, ...opts });
  if (result.error || result.status !== 0) {
    const error = new Error(scrub(`${cmd} failed: ${result.error?.message || result.stderr || result.stdout || `exit ${result.status}`}`).slice(0, 1200));
    error.retryable = /timed out|timeout|could not resolve|connection reset|connection refused|HTTP (?:429|5\d\d)/i.test(error.message);
    throw error;
  }
  return result.stdout;
}
export function runAgent(cmd, args, workspace, output, step = 'agent', jsonMode = false) {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { cwd: workspace, env: process.env, stdio: ['ignore', 'pipe', 'pipe'] });
    const log = fs.openSync(join(output, 'agent.log'), 'a', 0o600);
    let answer = '', settled = false, aborted = false;
    function channel(stream, isAnswer) {
      let pending = '', dropping = false;
      stream.setEncoding('utf8');
      const writePublic = (line, level = 'tool') => { const safe = scrub(line); fs.writeSync(log, `${safe}\n`); if (safe.trim()) event(step, '', safe, level); };
      const write = (line) => {
        if (!jsonMode || !isAnswer) { writePublic(line); return; }
        let record; try { record = JSON.parse(line); } catch { writePublic('[invalid Pi JSON record omitted]', 'error'); return; }
        // Strict allowlist: never persist raw message/agent events, thinking
        // blocks, provider snapshots, compaction summaries or usage payloads.
        if (record.type === 'tool_execution_start') writePublic(`Tool ${record.toolName}: ${record.args?.command || record.args?.path || ''}`);
        if (record.type === 'tool_execution_end') {
          writePublic(`Tool ${record.toolName} ${record.isError ? 'failed' : 'completed'}`, record.isError ? 'error' : 'tool');
          for (const part of record.result?.content || []) if (part.type === 'text') writePublic(part.text, record.isError ? 'error' : 'tool');
        }
        if (record.type === 'message_end' && record.message?.role === 'assistant') {
          answer = (record.message.content || []).filter(part => part.type === 'text').map(part => part.text).join('\n');
          if (answer) writePublic(answer, 'info');
          if (record.message.errorMessage) writePublic(record.message.errorMessage, 'error');
        }
        if (record.type === 'auto_retry_start') writePublic(`Model retry ${record.attempt}: ${record.errorMessage || ''}`);
        if (record.type === 'agent_settled' && record.aborted) aborted = true;
      };
      stream.on('data', chunk => {
        if (isAnswer && !jsonMode) answer = (answer + chunk).slice(-24000);
        pending += chunk;
        let index;
        while ((index = pending.indexOf('\n')) >= 0) {
          const line = pending.slice(0, index); pending = pending.slice(index + 1);
          if (!dropping) write(line); dropping = false;
        }
        if (pending.length > 1048576) { writePublic('[long output record omitted]'); pending = ''; dropping = true; }
      });
      stream.on('end', () => { if (pending && !dropping) write(pending); });
    }
    channel(child.stdout, true); channel(child.stderr, false);
    const finish = (error) => { if (settled) return; settled = true; fs.closeSync(log); error ? reject(error) : resolve(scrub(answer)); };
    child.on('error', error => finish(new Error(scrub(`Local Agent failed: ${error.message}`))));
    child.on('close', (code, signal) => finish(code === 0 && !aborted ? null : new Error(`Local Agent failed (${signal || (aborted ? 'aborted' : `exit ${code}`)})`)));
  });
}
