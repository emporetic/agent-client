#!/usr/bin/env sh
# Keep deployment leftovers out of the open-source repository: personal hostnames, LAN registry
# plumbing and per-machine labels belong on the build machine, not in Git.
#
# The script only reads files, so it is safe both as a CI check and as a gate before publishing
# a commit to GitHub. Nothing here rewrites anything anymore: Forgejo's runner, its container
# registry and its package proxy left this repository when CI moved to GitHub Actions.
#
# CI runs it through `sh`, which on Linux runners means dash: stay POSIX, no `pipefail`, no arrays,
# nothing else the bash interpreter invented.
#
# Usage: ci/publicize.sh <tree-dir>   # non-zero exit means a private string was found
set -eu

tree="${1:?usage: $0 <tree-dir>}"
[ -d "$tree" ] || { echo "no such directory: $tree" >&2; exit 1; }

cd "$tree"

python3 - <<'PY'
import pathlib, sys

# This very script carries the token tables as literal text, so it stays out of its own scan:
# reporting itself would turn the guard into its own leak.
SELF = "publicize.sh"


def is_text(path):
    try:
        with path.open("rb") as fp:
            head = fp.read(8192)
    except OSError:
        return False
    if b"\0" in head:
        return False
    try:
        head.decode("utf-8")
    except UnicodeDecodeError:
        return False
    return True


def files():
    root = pathlib.Path(".")
    for path in root.rglob("*"):
        rel = path.relative_to(root)
        if not path.is_file() or path.name == SELF or ".git" in rel.parts:
            continue
        if not is_text(path):
            continue
        yield path


# Self-hosted tooling that no longer lives here, checked case-insensitively (FORGEJO_TOKEN counts).
CASE_TOKENS = ["for" + "gejo", "gi" + "tea", "na" + "s."]

# Personal strings, exact match. Split so the table never matches itself.
EXACT_TOKENS = [
    "zho" + "udians",
    "dao" + "cloud",
    "书" + "房",
    "/Users/" + "zho" + "u/",
    "zhoudi" + "an64",
    "data.forgejo." + "org",
    "oauth2:$" + "GITHUB_TOKEN",
    "registry.example.internal",  # leftover placeholder from an earlier scrub
]

leaks = []
for path in files():
    try:
        text = path.read_text("utf-8")
    except UnicodeDecodeError:
        continue
    lowered = text.lower()
    for token in CASE_TOKENS:
        if token in lowered:
            leaks.append(f"{path}: {token}")
    for token in EXACT_TOKENS:
        if token in text:
            leaks.append(f"{path}: {token}")

if leaks:
    print("publicize: deployment leftovers found:\n  " + "\n  ".join(leaks), file=sys.stderr)
    sys.exit(1)

print("publicize: clean tree, no deployment leftovers")
PY
