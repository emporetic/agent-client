import fs from 'node:fs';
import { spawnSync } from 'node:child_process';
import { resolve } from 'node:path';
import { parseAdapterPaths } from './adapter-args.mjs';

const [briefPath, output] = parseAdapterPaths(process.argv.slice(2), ['screening', 'output']);
const brief = JSON.parse(fs.readFileSync(briefPath, 'utf8'));
const candidates = Array.isArray(brief.candidates) ? brief.candidates : [];
if (!candidates.length) throw new Error('Nothing survived the mechanical prescreen; do not ask the model');

const table = candidates.map((c) => `${c.id}\t${c.bounty_credits} credits\t${c.agents_working} in flight\t${c.repo_url}\t${c.title}`).join('\n');
const prompt = `You pick work for this machine. Intent of the owner:\n${brief.intent ?? '(no intent written)'}\n\nOpen bounties:\n${table}\n\nReturn only the ids you would actually take, one per line (${candidates[0].id.constructor.name} pattern like tsk_xxxxx). Skip anything whose repository, language stack, or remaining time you cannot serve well.`;
const r = spawnSync('pi', ['--print', '--no-session', '--', prompt], { encoding: 'utf8', maxBuffer: 8 * 1024 * 1024 });
if (r.error) throw new Error(`Pi title screening failed: ${r.error.message}`);
if (r.status !== 0) throw new Error(`Pi title screening failed (exit ${r.status}, signal ${r.signal || 'none'})`);
const text = r.stdout || '';
const known = new Set(candidates.map((c) => c.id));
const ids = [...new Set([...text.matchAll(/tsk_[A-Za-z0-9_-]{1,100}/g)].map((m) => m[0]).filter((id) => known.has(id)))];
fs.writeFileSync(resolve(output, 'picks.json'), JSON.stringify({ ids, skipped: [] }, null, 2), { encoding: 'utf8' });
console.log(`Title screening kept ${ids.length} of ${candidates.length}`);
