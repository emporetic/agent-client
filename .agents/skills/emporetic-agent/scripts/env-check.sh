#!/bin/sh
# Preflight for the Emporetic Agent client. Never prints the platform key value.
# Usage: EMPORETIC_INSTANCE=name env-check.sh
set -u

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

status=0
say_ok() { printf '[ok]   %s\n' "$1"; }
say_bad() { printf '[fail] %s\n' "$1"; status=1; }
say_warn() { printf '[warn] %s\n' "$1"; }

repo=${EMPORETIC_CLIENT_REPO:-}
if [ -z "$repo" ]; then
  repo=$(find_client_root "$script_dir" || true)
fi
if [ -z "$repo" ] || [ ! -d "$repo" ]; then
  say_bad "client root not found; set EMPORETIC_CLIENT_REPO to the source checkout or unpacked release directory"
  printf '\nPreflight failed.\n'
  exit 1
fi
repo=$(CDPATH= cd -- "$repo" && pwd -P) || exit 1
say_ok "client root $repo"

binary="$repo/bin/emporetic-agent"
if { [ ! -f "$binary" ] || [ ! -x "$binary" ]; } &&
   [ -f "$repo/emporetic-agent" ] && [ -x "$repo/emporetic-agent" ]; then
  binary="$repo/emporetic-agent"
fi
if [ -f "$binary" ] && [ -x "$binary" ]; then
  say_ok "client binary $binary"
else
  say_bad "client missing or not executable: run $script_dir/build-client.sh for setup guidance"
fi

if command -v git >/dev/null 2>&1; then
  say_ok "git on PATH ($(git --version 2>/dev/null | sed -n 's/^git version //p'))"
else
  say_bad "git is required before claiming a task"
fi

instance=${EMPORETIC_INSTANCE:-}
identity_root=${EMPORETIC_HOME:-$HOME/.emporetic}
if [ -z "$instance" ]; then
  say_bad "set EMPORETIC_INSTANCE to an instance listed in $identity_root/registry.json"
  config=""
else
  config="$identity_root/$instance/config.json"
fi

if [ -n "$config" ] && [ -r "$config" ]; then
  say_ok "identity directory configuration $config"
elif [ -n "$config" ]; then
  say_bad "no configuration at $config (register the instance first)"
fi

url=$(sed -n 's/.*"api_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$config" 2>/dev/null | head -n 1)
if [ -n "$url" ]; then
  case "$url" in
    *"/api/v1"*) say_warn "api_url ends with /api/v1; the client appends it itself" ;;
    *) say_ok "api_url has no /api/v1 suffix ($url)" ;;
  esac
fi

poll=$(sed -n 's/.*"poll_seconds"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$config" 2>/dev/null | head -n 1)
timeout=$(sed -n 's/.*"timeout_seconds"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$config" 2>/dev/null | head -n 1)
[ -n "$poll" ] && { [ "$poll" -ge 1 ] && [ "$poll" -le 300 ] && say_ok "poll_seconds $poll" || say_bad "poll_seconds must be 1-300 (got $poll)"; }
[ -n "$timeout" ] && { [ "$timeout" -ge 1 ] && [ "$timeout" -le 604800 ] && say_ok "timeout_seconds $timeout" || say_bad "timeout_seconds must be 1-604800 (got $timeout): real model runs need a large value"; }

# The key lives inside the identity directory: the model browses the working folder, not this file.
if [ -n "$config" ] && [ -r "$config" ]; then
  perms=$(stat -f '%Lp' "$config" 2>/dev/null || stat -c '%a' "$config" 2>/dev/null)
  case "${perms:-600}" in
    600 | 400) say_ok "configuration permissions ${perms:-600}" ;;
    *) say_warn "$config permissions ${perms:-unknown}: use 600 so the platform key is not world readable" ;;
  esac
  api_key=$(sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$config" | head -n 1)
  if [ -n "$api_key" ]; then
    say_ok "platform key sits in the identity directory"
  else
    say_bad "paste the platform key into $config; it is shown once when you register the Agent on the website"
  fi
fi

if [ -f "$binary" ] && [ -x "$binary" ] && [ -r "$config" ]; then
  # This intentionally invalid ID exercises the local wrapper without making an API request.
  out=$("$script_dir/client.sh" show tsk_ 2>&1 || true)
  case "$out" in
    *"requires a valid task ID"*) say_ok "wrapper rejects an invalid task ID locally; server authentication has not been checked" ;;
    *"platform key"*) say_bad "no usable platform key (it belongs in the instance config.json)" ;;
    *) say_bad "setup rejected by the client: $(printf '%s\n' "$out" | sed -n '1p')" ;;
  esac

  if grep -q 'adapter\.mjs' "$config" 2>/dev/null; then
    if command -v node >/dev/null 2>&1; then
      say_ok "example adapter in use and node is available"
    else
      say_bad "configuration calls examples/adapter.mjs but node is missing"
    fi
    if [ -z "${LOCAL_AGENT_COMMAND:-}" ]; then
      agent_file=${EMPORETIC_LOCAL_AGENT_FILE:-$HOME/.config/emporetic/local_agent.json}
      if [ -r "$agent_file" ]; then
        say_ok "local Agent command file $agent_file"
      else
        say_bad "adapter needs LOCAL_AGENT_COMMAND (JSON argv array): create $agent_file, see references/bootstrap.md"
      fi
    fi
  fi

  runs=$(sed -n 's/.*"runs_dir"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$config" 2>/dev/null | head -n 1)
  if [ -n "${runs:-}" ]; then
    case "$runs" in
      /*) dir=$runs ;;
      *) dir=$(CDPATH= cd -- "$(dirname -- "$config")" && pwd -P)"/$runs" ;;
    esac
    if mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ]; then
      chmod 700 "$dir" 2>/dev/null || true
      say_ok "task directory writable $dir"
    else
      say_bad "cannot write task directory $dir"
    fi
  fi
fi

printf '\n'
if [ "$status" = 0 ]; then
  printf 'Preflight ready. Verify connectivity and authentication with one real request:\n  scripts/client.sh tasks\n'
else
  printf 'Preflight incomplete: fix the failures above, then rerun this script.\n'
fi
exit "$status"
