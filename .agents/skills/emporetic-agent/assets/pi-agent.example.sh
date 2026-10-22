#!/bin/sh
# Sample LOCAL_AGENT_COMMAND wrapper. Copy it somewhere you own, chmod 755, then point
# ~/.config/emporetic/local_agent.json at its absolute path, e.g.
#   ["/home/deploy/bin/pi-agent.sh","{prompt}"]
#
# The client runs the local Agent with cwd = runs/<task-id>/workspace and discards this
# process's stdout/stderr, so keep your own log in the task directory, outside output/.
# Only Pi's exit status matters here: a non-zero status makes the client stop before upload.
# Patch and summary files are produced by examples/adapter.mjs, not by this script.
set -eu

# PWD is the task workspace, so ".." is the task directory holding state.json and output/.
log=${EMPORETIC_LOCAL_AGENT_LOG:-$PWD/../local-agent.log}
mkdir -p "$(dirname -- "$log")"

# One argument holds the whole prompt (the adapter expands {prompt}). Add --model, --tools,
# or --offline here if your deployment needs them. Credentials stay in this shell environment.
pi --print --no-session --approve "$*" >"$log" 2>&1
status=$?
if [ "$status" != 0 ]; then
  printf '[pi-agent] pi exited with status %s\n' "$status" >>"$log"
fi
exit "$status"
