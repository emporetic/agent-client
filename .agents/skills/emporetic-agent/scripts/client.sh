#!/bin/sh
# Drive the Emporetic client without exposing the platform key in argv, stdout, shell history, or logs.
# Usage: EMPORETIC_INSTANCE=name client.sh <client subcommand and arguments...>
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)

# Accept a source checkout or the unpacked release (whose binary is at its root).
find_client_root() {
  search_dir=$1
  while :; do
    if [ -f "$search_dir/bin/emporetic-agent" ] || [ -f "$search_dir/emporetic-agent" ] ||
       { [ -f "$search_dir/go.mod" ] && [ -d "$search_dir/cmd/emporetic-agent" ]; }; then
      printf '%s\n' "$search_dir"
      return 0
    fi
    [ "$search_dir" = "/" ] && return 1
    search_dir=$(dirname -- "$search_dir")
  done
}

repo=${EMPORETIC_CLIENT_REPO:-}
if [ -z "$repo" ]; then
  repo=$(find_client_root "$script_dir" || true)
fi
if [ -z "$repo" ] || [ ! -d "$repo" ]; then
  echo "Could not locate the client. Set EMPORETIC_CLIENT_REPO to the source checkout or unpacked release directory." >&2
  exit 3
fi

repo=$(CDPATH= cd -- "$repo" && pwd -P)
# Skills resolve through current into versions/<version>; use the stable launcher.
if [ "$(basename -- "$(dirname -- "$repo")")" = versions ] && [ -x "$repo/../../emporetic-agent" ]; then
  repo=$(CDPATH= cd -- "$repo/../.." && pwd -P)
fi
binary="$repo/bin/emporetic-agent"
if { [ ! -f "$binary" ] || [ ! -x "$binary" ]; } &&
   [ -f "$repo/emporetic-agent" ] && [ -x "$repo/emporetic-agent" ]; then
  binary="$repo/emporetic-agent"
fi
if [ ! -f "$binary" ] || [ ! -x "$binary" ]; then
  echo "Client binary missing or not executable under $repo. Run $script_dir/build-client.sh for setup guidance." >&2
  exit 3
fi

# The identity directory holds both the configuration and the platform key; the working folder stays code only.
instance=${EMPORETIC_INSTANCE:-}
if [ -z "$instance" ]; then
  echo "Set EMPORETIC_INSTANCE to one of this machine's instances (see registry.json under ~/.emporetic)." >&2
  exit 2
fi
identity_root=${EMPORETIC_HOME:-$HOME/.emporetic}
if [ ! -r "$identity_root/$instance/config.json" ]; then
  echo "No instance there: run $binary register --instance $instance --kind issuer|worker first (workspace defaults to <home>/workspace/<instance>)." >&2
  exit 2
fi
set -- "--home" "$identity_root" "--instance" "$instance" "$@"

if [ -z "${LOCAL_AGENT_COMMAND:-}" ]; then
  agent_file=${EMPORETIC_LOCAL_AGENT_FILE:-$HOME/.config/emporetic/local_agent.json}
  if [ -r "$agent_file" ]; then
    LOCAL_AGENT_COMMAND=$(tr -d '\n' <"$agent_file")
    export LOCAL_AGENT_COMMAND
  fi
fi

exec "$binary" "$@"
