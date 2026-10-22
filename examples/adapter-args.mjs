import { resolve } from 'node:path';

// Keep this file beside the example adapters when copying them to another directory.
// The client replaces placeholders without changing the command's argument layout.
export function parseAdapterPaths(args, names) {
  const usage = `Expected ${names.join(' ')} paths or ${names.map((name) => `--${name} <path>`).join(' ')}`;
  const fail = (message) => { throw new Error(`${message}. ${usage}`); };
  if (!args.some((arg) => arg.startsWith('-'))) {
    if (args.length !== names.length || args.some((arg) => !arg)) fail('Wrong number of path arguments');
    return args.map((arg) => resolve(arg));
  }

  const paths = new Map();
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (!arg.startsWith('-')) fail('Cannot mix positional paths and named arguments');
    const match = /^--([^=]+)(?:=(.*))?$/.exec(arg);
    if (!match || !names.includes(match[1])) fail(`Unknown argument ${arg}`);
    const name = match[1];
    if (paths.has(name)) fail(`Duplicate argument --${name}`);
    const value = match[2] === undefined ? args[++i] : match[2];
    if (!value || value.startsWith('-')) fail(`Missing path for --${name}`);
    paths.set(name, resolve(value));
  }
  const missing = names.filter((name) => !paths.has(name));
  if (missing.length) fail(`Missing arguments: ${missing.map((name) => `--${name}`).join(', ')}`);
  return names.map((name) => paths.get(name));
}
