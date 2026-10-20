import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import test from 'node:test';
import { parseAdapterPaths } from './adapter-args.mjs';

for (const names of [['task', 'workspace', 'output'], ['screening', 'output'], ['task', 'output']]) {
  const label = names.join(', ');
  const values = names.map((name) => `fixtures with spaces/${name}`);
  const flags = names.flatMap((name, i) => [`--${name}`, values[i]]);
  const expected = values.map((value) => resolve(value));

  test(`${label}: accepts positional paths and named paths in any order`, () => {
    assert.deepEqual(parseAdapterPaths(values, names), expected);
    assert.deepEqual(parseAdapterPaths(flags, names), expected);
    assert.deepEqual(parseAdapterPaths(names.map((name, i) => `--${name}=${values[i]}`).reverse(), names), expected);
  });

  test(`${label}: rejects invalid argument contracts before resolving paths`, () => {
    const cases = [
      [[], /Wrong number/],
      [values.slice(1), /Wrong number/],
      [[...values, 'extra'], /Wrong number/],
      [['', ...values.slice(1)], /Wrong number/],
      [flags.slice(2), /Missing arguments/],
      [flags.slice(0, -1), /Missing path/],
      [[`--${names[0]}`, ...flags.slice(2)], /Missing path/],
      [[`--${names[0]}=`, ...flags.slice(2)], /Missing path/],
      [[...flags, `--${names[0]}`, values[0]], /Duplicate argument/],
      [[...flags, '--unknown', 'path'], /Unknown argument/],
      [[...flags, '-x', 'path'], /Unknown argument/],
      [[values[0], ...flags.slice(2)], /Cannot mix/],
      [[...flags, 'extra'], /Cannot mix/],
    ];
    for (const [args, error] of cases) {
      assert.throws(() => parseAdapterPaths(args, names), error, JSON.stringify(args));
    }
  });
}
