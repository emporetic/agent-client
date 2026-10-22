# Adapter examples

These scripts accept either the positional arguments below or the corresponding
named arguments. Do not mix the two forms. Named arguments may be reordered and
may use `--name=value`; missing, repeated, and unknown arguments are errors.

| Script | Positional paths | Named paths |
| --- | --- | --- |
| `adapter.mjs`, `pi-executor.mjs` | `task workspace output` | `--task <path> --workspace <path> --output <path>` |
| `pi-picker.mjs` | `screening output` | `--screening <path> --output <path>` |
| `pi-judge.mjs` | `task output` | `--task <judging.json path> --output <path>` |

The client substitutes placeholders such as `{task}` and `{screening}`; it does
not convert flags into positional arguments. For example, both picker commands
are valid:

```json
["node", "/abs/path/examples/pi-picker.mjs", "{screening}", "{output}"]
["node", "/abs/path/examples/pi-picker.mjs", "--screening", "{screening}", "--output", "{output}"]
```

Copying the complete `examples/` directory includes the shared argument parser.
When copying individual scripts into an installed `bin/` directory, also copy
`adapter-args.mjs` into that same directory. The existing client normally creates
the output directory before running an adapter. For manual picker runs, create it
first. The picker writes `picks.json` only after Pi exits successfully and reads
candidate ids only from Pi's standard output.

Run the offline regression tests with Node 18 or newer:

```sh
node --test examples/adapter-args.test.mjs examples/adapters.test.mjs
```

The tests use a local Pi stub and temporary fixtures; they do not invoke a model,
read GitHub credentials, or publish anything.
