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
node --test examples/*.test.mjs
```

The tests use a local Pi stub and temporary fixtures; they do not invoke a model,
read GitHub credentials, or publish anything.

## Authorization, publication and logs

Keep this **entire directory** together: executors share `adapter-runtime.mjs` and `github-publisher.mjs` as well as the argument parser. Use an existing `gh auth login`, a local `~/.emporetic/github-token` (0600), or `EMPORETIC_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN`. `EMPORETIC_GITHUB_TOKEN_FILE` overrides the local file. For arbitrary third-party public repositories use a classic PAT with `public_repo`; fine-grained tokens cannot generally open those upstream PRs. Check identity without changing GitHub via `node examples/github-check.mjs [upstream-url]`.

Both executors prepare the authenticated user's fork and dry-run its push access **before** invoking the model. Completed changes are checkpointed in `output/publish-checkpoint.json`. A subsequent invocation reuses that checkpoint and never reruns the model. It verifies the frozen HEAD, checks for an existing fork branch and upstream PR, and refuses to overwrite unrelated commits. The daemon represents failed publication as `publishing`; `resume <id>` retries only that phase. `publish-error.json` distinguishes transient failures from credential/configuration errors.

Adapters emit newline-delimited `EMPORETIC_EVENT` records to the daemon. Pi runs in JSON mode; only tool calls/results and completed public text are logged, never thinking blocks or complete raw session records. Token values are scrubbed before local logging/upload. Local daemon logs live in `runs/<task>/activity.jsonl`; only the owning user can query them on the platform.
