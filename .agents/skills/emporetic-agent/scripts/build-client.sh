#!/bin/sh
# Build the client when the binary is absent; report its path either way.
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
binary="$repo/bin/emporetic-agent"
if { [ ! -f "$binary" ] || [ ! -x "$binary" ]; } &&
   [ -f "$repo/emporetic-agent" ] && [ -x "$repo/emporetic-agent" ]; then
  binary="$repo/emporetic-agent"
fi
if [ -f "$binary" ] && [ -x "$binary" ]; then
  printf '%s ready\n' "$binary"
  exit 0
fi

if [ ! -f "$repo/go.mod" ] || [ ! -d "$repo/cmd/emporetic-agent" ]; then
  echo "No executable client or source checkout at $repo. Restore the release binary at $repo/emporetic-agent and make it executable (chmod +x)." >&2
  exit 3
fi

if ! command -v go >/dev/null 2>&1; then
  cat <<'EOF' >&2
Go toolchain not found and no compiled client available. Either install Go 1.26+ and rerun this script,
or use the published release for your operating system and architecture (chmod +x emporetic-agent).
EOF
  exit 4
fi

version=$(go version 2>/dev/null | sed -n 's/.*go\([0-9][^.]*\)\.\([0-9]*\).*/\1.\2/p')
major=$(printf '%s' "${version:-0}" | cut -d. -f1)
minor=$(printf '%s' "${version:-0}" | cut -d. -f2)
if [ "${major:-0}" -lt 1 ] || { [ "${major:-0}" -eq 1 ] && [ "${minor:-0}" -lt 26 ]; }; then
  echo "Go 1.26 or newer is required to build the client (found ${version:-unknown})." >&2
  exit 4
fi

cd "$repo"
mkdir -p bin
go build -o bin/emporetic-agent ./cmd/emporetic-agent
printf 'Built %s\n' "$binary"
